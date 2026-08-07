package timeline

import (
	"encoding/json"
	"errors"
	"math"
	"math/bits"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxConfigBuckets          = 600
	maxConfigOperations       = 128
	maxOperationKeyBytes      = 256
	maxRetainedRuns           = 8
	overflowOperationKey      = "(other)"
	operationHistogramBuckets = 64
)

type runKey struct {
	runID string
	epoch uint64
}

type operationStat struct {
	count, successes, failures int64
	total, maximum             int64
	hist                       [operationHistogramBuckets]int64
}

type mutableBucket struct {
	bucket Bucket
	http   map[string]*operationStat
	sql    map[string]*operationStat
}

type mutableRun struct {
	start     RunStart
	buckets   []*mutableBucket
	httpKeys  map[string]struct{}
	sqlKeys   map[string]struct{}
	inFlight  int64
	truncated bool
	dropped   uint64
	prevPools map[string]PoolPoint
	prevProc  *ProcessPoint
	prevHost  *HostPoint
	prevAt    time.Time
}

type Collector struct {
	cfg Config

	mu        sync.Mutex
	active    *mutableRun
	completed map[runKey]Section
	order     []runKey
	pending   map[runKey]RunTermination
}

func New(cfg Config) (*Collector, error) {
	if cfg.Interval == 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.MaxBuckets == 0 {
		cfg.MaxBuckets = DefaultMaxBuckets
	}
	if cfg.MaxOperations == 0 {
		cfg.MaxOperations = DefaultMaxOperations
	}
	if cfg.Interval < 100*time.Millisecond || cfg.Interval > time.Minute ||
		cfg.MaxBuckets < 2 || cfg.MaxBuckets > maxConfigBuckets ||
		cfg.MaxOperations < 1 || cfg.MaxOperations > maxConfigOperations {
		return nil, errors.New("timeline: invalid interval or storage bounds")
	}
	return &Collector{
		cfg: cfg, completed: make(map[runKey]Section), pending: make(map[runKey]RunTermination),
	}, nil
}

// Start opens a run exactly once. It returns true only when the caller owns a
// newly active run and may start its sampler. Lifecycle delivery can race in
// either direction, so a terminal event received first is consumed here and
// returns false after publishing the completed section.
func (c *Collector) Start(start RunStart) bool {
	if c == nil || start.RunID == "" || start.Epoch == 0 || start.At.IsZero() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := runKey{start.RunID, start.Epoch}
	if _, exists := c.completed[key]; exists {
		return false
	}
	if c.active != nil && c.active.start.RunID == start.RunID && c.active.start.Epoch == start.Epoch {
		return false
	}
	if c.active != nil && (c.active.start.RunID != start.RunID || c.active.start.Epoch != start.Epoch) {
		c.finishLocked(RunTermination{
			RunID: c.active.start.RunID, Epoch: c.active.start.Epoch, At: start.At,
			Reason: "preempted", Validity: "invalid",
		})
	}
	c.active = &mutableRun{
		start: start, httpKeys: make(map[string]struct{}), sqlKeys: make(map[string]struct{}),
		prevPools: make(map[string]PoolPoint),
	}
	if terminal, ok := c.pending[key]; ok {
		delete(c.pending, key)
		c.finishLocked(terminal)
		return false
	}
	return true
}

type eventToken struct{ key runKey }

func (c *Collector) HTTPStart(at time.Time) any {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	run := c.active
	if run == nil {
		c.mu.Unlock()
		return nil
	}
	bucket := c.bucketLocked(run, at, false)
	if bucket == nil {
		c.mu.Unlock()
		return nil
	}
	if run.inFlight < math.MaxInt64 {
		run.inFlight++
	} else {
		incrementDropped(&run.dropped)
	}
	if run.inFlight > bucket.bucket.HTTPInFlightMax {
		bucket.bucket.HTTPInFlightMax = run.inFlight
	}
	token := eventToken{key: runKey{run.start.RunID, run.start.Epoch}}
	c.mu.Unlock()
	return token
}

func (c *Collector) HTTPCancel(at time.Time, token any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if run := c.active; run != nil && eventMatches(run, token) {
		if run.inFlight > 0 {
			run.inFlight--
		}
		_ = c.bucketLocked(run, at, false)
	}
	c.mu.Unlock()
}

func (c *Collector) HTTPFinish(at time.Time, token any, key string, duration time.Duration, failed bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	run := c.active
	if run == nil || !eventMatches(run, token) {
		c.mu.Unlock()
		return
	}
	if run.inFlight > 0 {
		run.inFlight--
	}
	if bucket := c.bucketLocked(run, at, false); bucket != nil {
		key = c.operationKeyLocked(run, run.httpKeys, key)
		observeOperation(bucket.http, key, duration, failed, &run.dropped)
	}
	c.mu.Unlock()
}

func (c *Collector) SQLStart(at time.Time) any {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || at.Before(c.active.start.At) {
		return nil
	}
	return eventToken{key: runKey{c.active.start.RunID, c.active.start.Epoch}}
}

func (c *Collector) SQLFinish(at time.Time, token any, key string, duration time.Duration, failed bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	run := c.active
	if run != nil && eventMatches(run, token) {
		if bucket := c.bucketLocked(run, at, false); bucket != nil {
			key = c.operationKeyLocked(run, run.sqlKeys, key)
			observeOperation(bucket.sql, key, duration, failed, &run.dropped)
		}
	}
	c.mu.Unlock()
}

func (c *Collector) SQLCancel(time.Time, any) {}

func (c *Collector) ObserveSQL(at time.Time, key string, duration time.Duration, failed bool) {
	token := c.SQLStart(at)
	c.SQLFinish(at, token, key, duration, failed)
}

func eventMatches(run *mutableRun, token any) bool {
	value, ok := token.(eventToken)
	return ok && run != nil && value.key.runID == run.start.RunID && value.key.epoch == run.start.Epoch
}

// Tick carries current in-flight demand into an otherwise idle bucket.
func (c *Collector) Tick(at time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if run := c.active; run != nil {
		if bucket := c.bucketLocked(run, at, true); bucket != nil && run.inFlight > bucket.bucket.HTTPInFlightMax {
			bucket.bucket.HTTPInFlightMax = run.inFlight
		}
	}
	c.mu.Unlock()
}

func (c *Collector) Sample(sample ResourceSample) {
	if c == nil || sample.At.IsZero() {
		return
	}
	c.mu.Lock()
	run := c.active
	if run == nil {
		c.mu.Unlock()
		return
	}
	bucket := c.bucketLocked(run, sample.At, true)
	if bucket == nil {
		c.mu.Unlock()
		return
	}
	interval := sample.At.Sub(run.prevAt)
	if !run.prevAt.IsZero() && interval > 0 {
		bucket.bucket.DBPools = poolDeltas(run.prevPools, sample.Pools)
		bucket.bucket.Process = processDelta(run.prevProc, sample.Process)
		bucket.bucket.Host = hostDelta(run.prevHost, sample.Host, interval)
	}
	run.prevPools = copyPools(sample.Pools)
	run.prevProc = copyProcess(sample.Process)
	run.prevHost = copyHost(sample.Host)
	run.prevAt = sample.At
	c.mu.Unlock()
}

func (c *Collector) Terminate(terminal RunTermination) {
	if c == nil || terminal.RunID == "" || terminal.Epoch == 0 || terminal.At.IsZero() || terminal.Reason == "" {
		return
	}
	c.mu.Lock()
	if c.active == nil || c.active.start.RunID != terminal.RunID || c.active.start.Epoch != terminal.Epoch {
		key := runKey{terminal.RunID, terminal.Epoch}
		if _, exists := c.completed[key]; exists {
			c.mu.Unlock()
			return
		}
		if _, exists := c.pending[key]; !exists {
			if len(c.pending) >= maxRetainedRuns {
				for old := range c.pending {
					delete(c.pending, old)
					break
				}
			}
			c.pending[key] = terminal
		}
		c.mu.Unlock()
		return
	}
	c.finishLocked(terminal)
	c.mu.Unlock()
}

func (c *Collector) Section(runID string, epoch uint64) (Section, bool) {
	if c == nil {
		return Section{}, false
	}
	c.mu.Lock()
	key := runKey{runID, epoch}
	if section, ok := c.completed[key]; ok {
		out := cloneSection(section)
		c.mu.Unlock()
		return out, true
	}
	if c.active != nil && c.active.start.RunID == runID && c.active.start.Epoch == epoch {
		section := c.sectionLocked(c.active, RunTermination{})
		c.mu.Unlock()
		return section, true
	}
	c.mu.Unlock()
	return Section{}, false
}

func (c *Collector) finishLocked(terminal RunTermination) {
	run := c.active
	if run == nil {
		return
	}
	c.bucketLocked(run, terminal.At, true)
	section := c.sectionLocked(run, terminal)
	section.Analysis = Analyze(section)
	if body, err := json.Marshal(section); err != nil || len(body) > MaxSerializedBytes {
		section.Buckets = nil
		section.Analysis = Analysis{Available: false, Reason: "timeline-size-limit"}
		section.Truncated = true
	}
	key := runKey{run.start.RunID, run.start.Epoch}
	c.completed[key] = section
	c.order = append(c.order, key)
	for len(c.order) > maxRetainedRuns {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.completed, oldest)
	}
	c.active = nil
}

func (c *Collector) sectionLocked(run *mutableRun, terminal RunTermination) Section {
	section := Section{
		Schema: SchemaV1, RunID: run.start.RunID, Epoch: run.start.Epoch, StartedAt: run.start.At,
		IntervalNs: int64(c.cfg.Interval), MaxBuckets: c.cfg.MaxBuckets, MaxOperations: c.cfg.MaxOperations,
		Truncated: run.truncated, OverflowedEvents: run.dropped, FinishedAt: terminal.At,
		Validity: terminal.Validity, StopReason: terminal.Reason,
		Buckets: make([]Bucket, 0, len(run.buckets)),
	}
	for _, mutable := range run.buckets {
		bucket := mutable.bucket
		bucket.HTTP = operationRows(mutable.http)
		bucket.SQL = operationRows(mutable.sql)
		bucket.DBPools = append([]PoolBucket(nil), bucket.DBPools...)
		if mutable.bucket.Process != nil {
			value := *mutable.bucket.Process
			bucket.Process = &value
		}
		if mutable.bucket.Host != nil {
			value := *mutable.bucket.Host
			bucket.Host = &value
		}
		section.Buckets = append(section.Buckets, bucket)
	}
	return section
}

func (c *Collector) bucketLocked(run *mutableRun, at time.Time, endInclusive bool) *mutableBucket {
	if at.Before(run.start.At) {
		return nil
	}
	delta := at.Sub(run.start.At)
	if endInclusive && delta > 0 && delta%c.cfg.Interval == 0 {
		delta--
	}
	index := int(delta / c.cfg.Interval)
	if index >= c.cfg.MaxBuckets {
		index = c.cfg.MaxBuckets - 1
		run.truncated = true
	}
	for len(run.buckets) <= index {
		i := len(run.buckets)
		start := run.start.At.Add(time.Duration(i) * c.cfg.Interval)
		run.buckets = append(run.buckets, &mutableBucket{
			bucket: Bucket{Index: i, Start: start, End: start.Add(c.cfg.Interval)},
			http:   make(map[string]*operationStat), sql: make(map[string]*operationStat),
		})
	}
	return run.buckets[index]
}

func (c *Collector) operationKeyLocked(run *mutableRun, keys map[string]struct{}, key string) string {
	key = safeOperationKey(key)
	if _, exists := keys[key]; exists || key == overflowOperationKey {
		return key
	}
	if len(keys) >= c.cfg.MaxOperations {
		incrementDropped(&run.dropped)
		return overflowOperationKey
	}
	keys[key] = struct{}{}
	return key
}

func safeOperationKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > maxOperationKeyBytes || !utf8.ValidString(key) ||
		strings.IndexFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return overflowOperationKey
	}
	return key
}

func observeOperation(table map[string]*operationStat, key string, duration time.Duration, failed bool, dropped *uint64) {
	stat := table[key]
	if stat == nil {
		stat = &operationStat{}
		table[key] = stat
	}
	ns := duration.Nanoseconds()
	if ns < 0 {
		ns = 0
	}
	if stat.count == math.MaxInt64 || ns > 0 && stat.total > math.MaxInt64-ns {
		incrementDropped(dropped)
		return
	}
	stat.count++
	if failed {
		if stat.failures < math.MaxInt64 {
			stat.failures++
		}
	} else if stat.successes < math.MaxInt64 {
		stat.successes++
	}
	stat.total += ns
	if ns > stat.maximum {
		stat.maximum = ns
	}
	bucket := bits.Len64(uint64(ns))
	if bucket >= len(stat.hist) {
		bucket = len(stat.hist) - 1
	}
	stat.hist[bucket]++
}

func operationRows(table map[string]*operationStat) []Operation {
	rows := make([]Operation, 0, len(table))
	for key, stat := range table {
		rows = append(rows, Operation{
			Key: key, Count: stat.count, Successes: stat.successes, Errors: stat.failures,
			TotalNs: stat.total, MaxNs: stat.maximum, P95Ns: histogramP95(stat),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].TotalNs != rows[j].TotalNs {
			return rows[i].TotalNs > rows[j].TotalNs
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}

func histogramP95(stat *operationStat) int64 {
	if stat == nil || stat.count <= 0 {
		return 0
	}
	// ceil(count*0.95), expressed without multiplication so hostile or
	// synthetic MaxInt64 counters cannot overflow.
	target := stat.count - stat.count/20
	var seen int64
	for index, count := range stat.hist {
		seen += count
		if seen >= target {
			if index == 0 {
				return 0
			}
			if index >= 63 {
				return stat.maximum
			}
			upper := int64(1) << uint(index)
			if upper > stat.maximum {
				return stat.maximum
			}
			return upper
		}
	}
	return stat.maximum
}

func poolDeltas(previous map[string]PoolPoint, current []PoolPoint) []PoolBucket {
	rows := make([]PoolBucket, 0, len(current))
	for _, point := range current {
		base, ok := previous[point.TargetID]
		if !ok || point.TargetID == "" || point.MaxOpen < 0 || point.Open < 0 || point.InUse < 0 || point.Idle < 0 ||
			point.WaitCount < 0 || base.WaitCount < 0 || point.WaitDuration < 0 || base.WaitDuration < 0 ||
			point.WaitCount < base.WaitCount || point.WaitDuration < base.WaitDuration {
			continue
		}
		rows = append(rows, PoolBucket{
			TargetID: safeOperationKey(point.TargetID), MaxOpen: point.MaxOpen, Open: point.Open,
			InUse: point.InUse, Idle: point.Idle, WaitCount: point.WaitCount - base.WaitCount,
			WaitDurationNs: int64(point.WaitDuration - base.WaitDuration),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TargetID < rows[j].TargetID })
	return rows
}

func processDelta(previous, current *ProcessPoint) *ProcessBucket {
	if previous == nil || current == nil || current.TotalJiffies <= previous.TotalJiffies ||
		current.BusyJiffies < previous.BusyJiffies || current.IOWaitJiffies < previous.IOWaitJiffies ||
		current.ProcessJiffies < previous.ProcessJiffies {
		return nil
	}
	total := current.TotalJiffies - previous.TotalJiffies
	busy := current.BusyJiffies - previous.BusyJiffies
	iowait := current.IOWaitJiffies - previous.IOWaitJiffies
	process := current.ProcessJiffies - previous.ProcessJiffies
	if busy > total || iowait > total || process > total {
		return nil
	}
	cpus := current.CPUs
	if cpus <= 0 {
		cpus = previous.CPUs
	}
	if cpus <= 0 || cpus > 1<<16 {
		return nil
	}
	return &ProcessBucket{
		BusyPercent:       float64(busy) * 100 / float64(total),
		IOWaitPercent:     float64(iowait) * 100 / float64(total),
		ProcessCPUPercent: float64(process) * float64(cpus) * 100 / float64(total),
		RSSBytes:          current.RSSBytes,
	}
}

func incrementDropped(value *uint64) {
	if value != nil && *value < math.MaxUint64 {
		*value++
	}
}

func hostDelta(previous, current *HostPoint, interval time.Duration) *HostBucket {
	if previous == nil || current == nil || interval <= 0 || previous.IOTicks < 0 || current.IOTicks < 0 ||
		previous.WeightedIO < 0 || current.WeightedIO < 0 || current.ReadBytes < previous.ReadBytes ||
		current.WriteBytes < previous.WriteBytes || current.IOTicks < previous.IOTicks || current.WeightedIO < previous.WeightedIO {
		return nil
	}
	return &HostBucket{
		ReadBytes:          current.ReadBytes - previous.ReadBytes,
		WriteBytes:         current.WriteBytes - previous.WriteBytes,
		DiskBusyPercentSum: float64(current.IOTicks-previous.IOTicks) * 100 / float64(interval),
		WeightedIOPercent:  float64(current.WeightedIO-previous.WeightedIO) * 100 / float64(interval),
	}
}

func copyPools(points []PoolPoint) map[string]PoolPoint {
	out := make(map[string]PoolPoint, len(points))
	for _, point := range points {
		if point.TargetID != "" {
			out[point.TargetID] = point
		}
	}
	return out
}

func copyProcess(point *ProcessPoint) *ProcessPoint {
	if point == nil {
		return nil
	}
	out := *point
	return &out
}

func copyHost(point *HostPoint) *HostPoint {
	if point == nil {
		return nil
	}
	out := *point
	return &out
}

func cloneSection(section Section) Section {
	body, err := json.Marshal(section)
	if err != nil {
		return Section{}
	}
	var out Section
	if err := json.Unmarshal(body, &out); err != nil {
		return Section{}
	}
	return out
}
