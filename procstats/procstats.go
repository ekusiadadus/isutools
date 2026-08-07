// Package procstats measures per-process CPU and RSS over a reset-to-snapshot
// interval using Linux procfs. The filesystem is injectable so parsing and
// interval behavior can be tested on every development platform.
package procstats

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

const (
	// CollectorName is the immutable run snapshot section populated by this
	// collector.
	CollectorName     = "proc"
	defaultTopN       = 10
	defaultClockTicks = 100
	maxHealthErrors   = 16
)

// Status summarizes whether a collector produced a complete interval.
type Status string

const (
	StatusOK          Status = "ok"
	StatusPartial     Status = "partial"
	StatusUnavailable Status = "unavailable"
)

// Health describes collection failures without making the measured
// application fail. Errors is bounded; Dropped always contains the full count.
type Health struct {
	Status  Status   `json:"status"`
	Partial bool     `json:"partial"`
	Dropped uint64   `json:"dropped"`
	Errors  []string `json:"errors,omitempty"`
}

// Process is one process's reset-to-snapshot CPU delta and end-of-interval RSS.
type Process struct {
	PID          int     `json:"pid"`
	Command      string  `json:"command"`
	CPUPercent   float64 `json:"cpuPercent"`
	CPUSeconds   float64 `json:"cpuSeconds"`
	RSSBytes     uint64  `json:"rssBytes"`
	DeltaJiffies uint64  `json:"deltaJiffies"`
	Starttime    uint64  `json:"starttime"`
	Appeared     bool    `json:"appeared,omitempty"`
	PIDReused    bool    `json:"pidReused,omitempty"`
}

// Snapshot is the process report for the current reset-to-snapshot interval.
// CPU percent follows top: one fully occupied core is 100%.
type Snapshot struct {
	StartedAt       time.Time `json:"startedAt"`
	EndedAt         time.Time `json:"endedAt"`
	IntervalJiffies uint64    `json:"intervalJiffies"`
	CPUs            int       `json:"cpus"`
	CPUTotal        *CPUTotal `json:"cpuTotal,omitempty"`
	TopCPU          []Process `json:"topCPU"`
	TopRSS          []Process `json:"topRSS"`
	Health          Health    `json:"health"`
}

// CPUTotal is the whole-machine utilization over the interval, following
// top's %Cpu(s) convention where all cores together are 100%. It answers
// "is the hardware actually saturated, or is capacity left idle?".
type CPUTotal struct {
	BusyPercent   float64 `json:"busyPercent"`
	UserPercent   float64 `json:"userPercent"`
	SystemPercent float64 `json:"systemPercent"`
	IOWaitPercent float64 `json:"iowaitPercent"`
	StealPercent  float64 `json:"stealPercent"`
	IdlePercent   float64 `json:"idlePercent"`
}

// Point is one cumulative process/host CPU reading for time-bucket deltas.
// It intentionally contains no command names or per-PID rows.
type Point struct {
	TotalJiffies   uint64
	BusyJiffies    uint64
	IOWaitJiffies  uint64
	ProcessJiffies uint64
	CPUs           int
	RSSBytes       uint64
}

// cpuTimes is the aggregate /proc/stat cpu line split into categories.
type cpuTimes struct {
	total  uint64
	user   uint64 // user + nice
	system uint64 // system + irq + softirq
	idle   uint64
	iowait uint64
	steal  uint64
}

// Option configures a Collector.
type Option func(*Collector)

// WithProcRoot selects a procfs root. The default is /proc.
func WithProcRoot(root string) Option {
	return func(c *Collector) {
		if root != "" {
			c.fs = os.DirFS(root)
		}
	}
}

// WithFS supplies a procfs-like filesystem. It is primarily intended for
// deterministic tests and non-Linux development hosts.
func WithFS(fsys fs.FS) Option {
	return func(c *Collector) {
		if fsys != nil {
			c.fs = fsys
		}
	}
}

// WithTopN sets the maximum number of entries in each CPU and RSS list.
func WithTopN(n int) Option {
	return func(c *Collector) {
		if n > 0 {
			c.topN = n
		}
	}
}

// WithTrackedPIDs marks process identities whose loss must make an interval
// partial. Host-wide process churn is expected, so an untracked PID that exits
// between boundaries is ignored; a tracked PID that exits is measurement loss.
// Non-positive PIDs are ignored.
func WithTrackedPIDs(pids ...int) Option {
	return func(c *Collector) {
		if c.trackedPIDs == nil {
			c.trackedPIDs = make(map[int]struct{}, len(pids))
		}
		for _, pid := range pids {
			if pid > 0 {
				c.trackedPIDs[pid] = struct{}{}
			}
		}
	}
}

// WithClockTicks sets USER_HZ used to convert process jiffies to seconds.
// Linux is normally 100; injection avoids a libc dependency and enables tests.
func WithClockTicks(ticks uint64) Option {
	return func(c *Collector) {
		if ticks > 0 {
			c.clockTicks = ticks
		}
	}
}

// WithPageSize sets the byte size used for statm resident pages.
func WithPageSize(bytes uint64) Option {
	return func(c *Collector) {
		if bytes > 0 {
			c.pageSize = bytes
		}
	}
}

// WithClock supplies wall time for interval metadata.
func WithClock(now func() time.Time) Option {
	return func(c *Collector) {
		if now != nil {
			c.now = now
		}
	}
}

// Collector owns a baseline and produces interval snapshots. Reset and
// Snapshot are serialized so a baseline cannot be changed mid-scan.
type Collector struct {
	mu          sync.Mutex
	fs          fs.FS
	topN        int
	clockTicks  uint64
	pageSize    uint64
	now         func() time.Time
	trackedPIDs map[int]struct{}

	baseline       sample
	baselineHealth Health
	lastHealth     Health
	baselineValid  bool

	// runResults makes CaptureBaseline/CaptureFinal idempotent. The immutable
	// boundary payload is carried by each handle; this map only remembers the
	// first result for retries and is pruned when a newer epoch arrives.
	runResults map[procRunKey]runctl.SampleResult
	runEpoch   runctl.Epoch
}

type procRunKey struct {
	runID string
	epoch runctl.Epoch
	phase runctl.Phase
}

type runBoundary struct {
	baseline bool
	snapshot Snapshot
}

var _ runctl.BaselineCollector = (*Collector)(nil)

type sample struct {
	at        time.Time
	total     uint64
	times     cpuTimes
	cpus      int
	processes map[int]procSample
}

type procSample struct {
	pid       int
	command   string
	cpu       uint64
	starttime uint64
	rssBytes  uint64
}

// New creates an idle collector. Call Reset immediately before the measured
// interval and Snapshot after it.
func New(options ...Option) *Collector {
	c := &Collector{
		fs:         os.DirFS("/proc"),
		topN:       defaultTopN,
		clockTicks: defaultClockTicks,
		pageSize:   uint64(os.Getpagesize()),
		now:        time.Now,
		lastHealth: healthy(),
		runResults: make(map[procRunKey]runctl.SampleResult),
	}
	for _, option := range options {
		if option != nil {
			option(c)
		}
	}
	return c
}

// Reset captures the interval baseline. Per-process failures are recorded in
// Health and do not fail Reset. A missing or malformed aggregate /proc/stat
// makes the interval unavailable and is returned to the caller.
func (c *Collector) Reset() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resetLocked()
}

func (c *Collector) resetLocked() error {
	current, h, err := c.readSample(false)
	if err != nil {
		h.unavailable(err.Error())
		c.baselineValid = false
		c.baselineHealth = h
		c.lastHealth = cloneHealth(h)
		return err
	}
	c.baseline = current
	c.baselineHealth = h
	c.lastHealth = cloneHealth(h)
	c.baselineValid = true
	return nil
}

// Snapshot reads the interval end and returns CPU and RSS top lists. Collection
// errors are reflected in Snapshot.Health instead of panicking or terminating
// the application.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Collector) snapshotLocked() Snapshot {
	result := Snapshot{EndedAt: c.now(), Health: cloneHealth(c.lastHealth)}
	if !c.baselineValid {
		if result.Health.Status == StatusOK {
			result.Health.unavailable("procstats baseline has not been reset")
		}
		c.lastHealth = cloneHealth(result.Health)
		return result
	}

	current, currentHealth, err := c.readSample(true)
	h := mergeHealth(c.baselineHealth, currentHealth)
	result.StartedAt = c.baseline.at
	result.EndedAt = current.at
	result.CPUs = current.cpus
	if err != nil {
		h.unavailable(err.Error())
		result.Health = h
		c.lastHealth = cloneHealth(h)
		return result
	}
	if current.total < c.baseline.total {
		h.unavailable("aggregate CPU jiffies decreased during interval")
		result.Health = h
		c.lastHealth = cloneHealth(h)
		return result
	}
	result.IntervalJiffies = current.total - c.baseline.total
	result.CPUTotal = computeCPUTotal(c.baseline.times, current.times)
	if current.cpus != c.baseline.cpus {
		h.drop(fmt.Sprintf("logical CPU count changed from %d to %d", c.baseline.cpus, current.cpus))
	}

	processes := make([]Process, 0, len(current.processes))
	seenBaseline := make(map[int]struct{}, len(current.processes))
	for pid, end := range current.processes {
		start, existed := c.baseline.processes[pid]
		process := Process{
			PID:       pid,
			Command:   end.command,
			RSSBytes:  end.rssBytes,
			Starttime: end.starttime,
		}
		switch {
		case !existed:
			process.Appeared = true
			process.DeltaJiffies = end.cpu
		case start.starttime != end.starttime:
			process.Appeared = true
			process.PIDReused = true
			process.DeltaJiffies = end.cpu
			seenBaseline[pid] = struct{}{}
			h.drop(fmt.Sprintf("pid %d was reused during interval", pid))
		case end.cpu < start.cpu:
			seenBaseline[pid] = struct{}{}
			h.drop(fmt.Sprintf("pid %d CPU jiffies decreased during interval", pid))
			continue
		default:
			seenBaseline[pid] = struct{}{}
			process.DeltaJiffies = end.cpu - start.cpu
		}
		if c.clockTicks > 0 {
			process.CPUSeconds = float64(process.DeltaJiffies) / float64(c.clockTicks)
		}
		if result.IntervalJiffies > 0 && result.CPUs > 0 {
			process.CPUPercent = float64(process.DeltaJiffies) * float64(result.CPUs) * 100 / float64(result.IntervalJiffies)
		}
		processes = append(processes, process)
	}

	for pid := range c.baseline.processes {
		if _, seen := seenBaseline[pid]; seen {
			continue
		}
		if _, stillPresent := current.processes[pid]; !stillPresent {
			if _, tracked := c.trackedPIDs[pid]; tracked {
				h.drop(fmt.Sprintf("tracked pid %d disappeared during interval", pid))
			}
		}
	}

	result.TopCPU = topProcesses(processes, c.topN, func(a, b Process) bool {
		if a.CPUPercent != b.CPUPercent {
			return a.CPUPercent > b.CPUPercent
		}
		if a.RSSBytes != b.RSSBytes {
			return a.RSSBytes > b.RSSBytes
		}
		return a.PID < b.PID
	})
	result.TopRSS = topProcesses(processes, c.topN, func(a, b Process) bool {
		if a.RSSBytes != b.RSSBytes {
			return a.RSSBytes > b.RSSBytes
		}
		if a.CPUPercent != b.CPUPercent {
			return a.CPUPercent > b.CPUPercent
		}
		return a.PID < b.PID
	})
	result.Health = h
	c.lastHealth = cloneHealth(h)
	return result
}

// Name identifies the immutable section this collector contributes to a run.
func (c *Collector) Name() string { return CollectorName }

// CaptureBaseline resets procstats at the coordinated opening boundary. A
// retry for the same run and epoch replays the first committed result instead
// of moving the baseline forward.
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.captureRunBoundary(ctx, runID, ep, runctl.PhaseStartBaseline)
}

// CaptureFinal freezes the process snapshot at the coordinated closing
// boundary. Report rendering later reads this immutable value and never extends
// the interval to dashboard/save time.
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.captureRunBoundary(ctx, runID, ep, runctl.PhaseFinishFinal)
}

func (c *Collector) captureRunBoundary(
	ctx context.Context,
	runID string,
	ep runctl.Epoch,
	phase runctl.Phase,
) (runctl.SampleResult, error) {
	if err := ctx.Err(); err != nil {
		return runctl.SampleResult{At: c.now()}, fmt.Errorf("procstats: %s: %w", phase, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	key := procRunKey{runID: runID, epoch: ep, phase: phase}
	if ep < c.runEpoch {
		return runctl.SampleResult{At: c.now()},
			fmt.Errorf("%w: procstats: epoch %d, current %d", runctl.ErrStaleEpoch, ep, c.runEpoch)
	}
	if ep > c.runEpoch {
		c.runEpoch = ep
		for old := range c.runResults {
			if old.epoch < ep {
				delete(c.runResults, old)
			}
		}
	}
	if fixed, ok := c.runResults[key]; ok {
		return fixed, nil
	}
	if err := ctx.Err(); err != nil {
		return runctl.SampleResult{At: c.now()}, fmt.Errorf("procstats: %s: %w", phase, err)
	}

	var boundary runBoundary
	var at time.Time
	switch phase {
	case runctl.PhaseStartBaseline:
		if err := c.resetLocked(); err != nil {
			return runctl.SampleResult{At: c.now()}, err
		}
		at = c.baseline.at
		boundary.baseline = true
	case runctl.PhaseFinishFinal:
		boundary.snapshot = cloneSnapshot(c.snapshotLocked())
		at = boundary.snapshot.EndedAt
	default:
		return runctl.SampleResult{At: c.now()}, fmt.Errorf("procstats: unsupported boundary phase %q", phase)
	}

	result := runctl.SampleResult{
		Handle:    runctl.NewBaselineHandle(runID, ep, CollectorName, phase, at, boundary),
		At:        at,
		Committed: true,
	}
	c.runResults[key] = result
	return result, nil
}

// Collect returns the snapshot frozen by CaptureFinal. It reads only the two
// handles, so neither a later dashboard refresh nor a delayed save can change
// the measured process interval.
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) {
	if base.RunID != final.RunID || base.Epoch != final.Epoch {
		return nil, fmt.Errorf("procstats: boundary handles name different runs")
	}
	baseValue, ok := base.Sample().(runBoundary)
	if !ok || !baseValue.baseline {
		return nil, fmt.Errorf("procstats: baseline handle carries %T, want procstats run boundary", base.Sample())
	}
	finalValue, ok := final.Sample().(runBoundary)
	if !ok || finalValue.baseline {
		return nil, fmt.Errorf("procstats: final handle carries %T, want procstats final boundary", final.Sample())
	}
	return cloneSnapshot(finalValue.snapshot), nil
}

// Release forgets retry bookkeeping. The handle itself retains a deep copy of
// its boundary, so release cannot change an interval already being collected.
func (c *Collector) Release(h runctl.BaselineHandle) {
	if h.Zero() || h.Collector != CollectorName {
		return
	}
	c.mu.Lock()
	delete(c.runResults, procRunKey{runID: h.RunID, epoch: h.Epoch, phase: h.Phase})
	c.mu.Unlock()
}

func cloneSnapshot(in Snapshot) Snapshot {
	out := in
	out.TopCPU = append([]Process(nil), in.TopCPU...)
	out.TopRSS = append([]Process(nil), in.TopRSS...)
	out.Health = cloneHealth(in.Health)
	if in.CPUTotal != nil {
		cpu := *in.CPUTotal
		out.CPUTotal = &cpu
	}
	return out
}

// Health returns a copy of the most recent Reset or Snapshot health.
func (c *Collector) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneHealth(c.lastHealth)
}

// TimelinePoint reads the cumulative counters needed by the optional
// run-aligned timeline. It shares the collector lock with boundary snapshots
// so procfs reads cannot interleave with a reset.
func (c *Collector) TimelinePoint() (Point, error) {
	if c == nil {
		return Point{}, errors.New("procstats: nil collector")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, _, err := c.readSample(true)
	if err != nil {
		return Point{}, err
	}
	point := Point{
		TotalJiffies: current.total, IOWaitJiffies: current.times.iowait,
		CPUs: current.cpus,
	}
	idleAndWait, ok := checkedUintAdd(current.times.idle, current.times.iowait)
	if !ok || idleAndWait > current.total {
		return Point{}, errors.New("procstats: aggregate busy counter is invalid")
	}
	point.BusyJiffies = current.total - idleAndWait
	for _, process := range current.processes {
		var added bool
		point.ProcessJiffies, added = checkedUintAdd(point.ProcessJiffies, process.cpu)
		if !added {
			return Point{}, errors.New("procstats: process CPU counter sum overflows")
		}
		point.RSSBytes, added = checkedUintAdd(point.RSSBytes, process.rssBytes)
		if !added {
			return Point{}, errors.New("procstats: process RSS sum overflows")
		}
	}
	return point, nil
}

func checkedUintAdd(a, b uint64) (uint64, bool) {
	if ^uint64(0)-a < b {
		return 0, false
	}
	return a + b, true
}

func (c *Collector) readSample(readRSS bool) (sample, Health, error) {
	result := sample{at: c.now(), processes: make(map[int]procSample)}
	h := healthy()
	stat, err := fs.ReadFile(c.fs, "stat")
	if err != nil {
		return result, h, fmt.Errorf("read proc stat: %w", err)
	}
	result.times, result.cpus, err = parseSystemStat(stat)
	result.total = result.times.total
	if err != nil {
		return result, h, fmt.Errorf("parse proc stat: %w", err)
	}
	entries, err := fs.ReadDir(c.fs, ".")
	if err != nil {
		return result, h, fmt.Errorf("read proc root: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		statPath := path.Join(entry.Name(), "stat")
		data, err := fs.ReadFile(c.fs, statPath)
		if err != nil {
			h.drop(fmt.Sprintf("pid %d stat: %v", pid, err))
			continue
		}
		process, err := parseProcessStat(data)
		if err != nil {
			h.drop(fmt.Sprintf("pid %d stat: %v", pid, err))
			continue
		}
		if process.pid != pid {
			h.drop(fmt.Sprintf("pid directory %d contains stat for pid %d", pid, process.pid))
			continue
		}
		if readRSS {
			statmPath := path.Join(entry.Name(), "statm")
			data, err = fs.ReadFile(c.fs, statmPath)
			if err != nil {
				h.problem(fmt.Sprintf("pid %d statm: %v", pid, err))
			} else if process.rssBytes, err = parseRSS(data, c.pageSize); err != nil {
				h.problem(fmt.Sprintf("pid %d statm: %v", pid, err))
			}
		}
		result.processes[pid] = process
	}
	return result, h, nil
}

func parseSystemStat(data []byte) (cpuTimes, int, error) {
	var times cpuTimes
	var total uint64
	cpus := 0
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "cpu" {
			if len(fields) < 2 {
				return cpuTimes{}, 0, errors.New("aggregate cpu line has no counters")
			}
			// guest and guest_nice are already included in user and nice.
			limit := len(fields)
			if limit > 9 {
				limit = 9
			}
			for i, field := range fields[1:limit] {
				value, err := strconv.ParseUint(field, 10, 64)
				if err != nil {
					return cpuTimes{}, 0, fmt.Errorf("aggregate cpu counter %q: %w", field, err)
				}
				total += value
				switch i {
				case 0, 1: // user, nice
					times.user += value
				case 2, 5, 6: // system, irq, softirq
					times.system += value
				case 3:
					times.idle += value
				case 4:
					times.iowait += value
				case 7:
					times.steal += value
				}
			}
			found = true
			continue
		}
		if strings.HasPrefix(fields[0], "cpu") && allDigits(strings.TrimPrefix(fields[0], "cpu")) {
			cpus++
		}
	}
	if !found {
		return cpuTimes{}, 0, errors.New("aggregate cpu line is missing")
	}
	if cpus == 0 {
		cpus = runtime.NumCPU()
	}
	times.total = total
	return times, cpus, nil
}

// computeCPUTotal converts baseline/current cpuTimes into interval
// percentages. Returns nil when the interval is empty or went backwards.
func computeCPUTotal(start, end cpuTimes) *CPUTotal {
	if end.total <= start.total {
		return nil
	}
	total := float64(end.total - start.total)
	delta := func(a, b uint64) float64 {
		if b < a {
			return 0
		}
		return float64(b-a) * 100 / total
	}
	result := &CPUTotal{
		UserPercent:   delta(start.user, end.user),
		SystemPercent: delta(start.system, end.system),
		IOWaitPercent: delta(start.iowait, end.iowait),
		StealPercent:  delta(start.steal, end.steal),
		IdlePercent:   delta(start.idle, end.idle),
	}
	result.BusyPercent = 100 - result.IdlePercent - result.IOWaitPercent
	if result.BusyPercent < 0 {
		result.BusyPercent = 0
	}
	return result
}

func parseProcessStat(data []byte) (procSample, error) {
	text := strings.TrimSpace(string(data))
	open := strings.IndexByte(text, '(')
	close := strings.LastIndex(text, ") ")
	if open <= 0 || close <= open {
		return procSample{}, errors.New("malformed comm field")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:open]))
	if err != nil || pid <= 0 {
		return procSample{}, errors.New("invalid pid")
	}
	fields := strings.Fields(text[close+2:])
	if len(fields) < 20 {
		return procSample{}, fmt.Errorf("only %d fields after comm", len(fields))
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return procSample{}, fmt.Errorf("utime: %w", err)
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return procSample{}, fmt.Errorf("stime: %w", err)
	}
	if ^uint64(0)-utime < stime {
		return procSample{}, errors.New("utime+stime overflows")
	}
	starttime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procSample{}, fmt.Errorf("starttime: %w", err)
	}
	return procSample{
		pid:       pid,
		command:   text[open+1 : close],
		cpu:       utime + stime,
		starttime: starttime,
	}, nil
}

func parseRSS(data []byte, pageSize uint64) (uint64, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, errors.New("resident pages field is missing")
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("resident pages: %w", err)
	}
	if pageSize > 0 && pages > ^uint64(0)/pageSize {
		return 0, errors.New("resident bytes overflow")
	}
	return pages * pageSize, nil
}

func topProcesses(processes []Process, n int, less func(a, b Process) bool) []Process {
	result := append([]Process(nil), processes...)
	sort.Slice(result, func(i, j int) bool { return less(result[i], result[j]) })
	if len(result) > n {
		result = result[:n]
	}
	if result == nil {
		return []Process{}
	}
	return result
}

func healthy() Health {
	return Health{Status: StatusOK}
}

func (h *Health) problem(message string) {
	if h.Status == StatusOK {
		h.Status = StatusPartial
	}
	h.Partial = true
	if len(h.Errors) < maxHealthErrors {
		h.Errors = append(h.Errors, message)
	}
}

func (h *Health) drop(message string) {
	h.Dropped++
	h.problem(message)
}

func (h *Health) unavailable(message string) {
	h.Status = StatusUnavailable
	h.Partial = true
	if len(h.Errors) < maxHealthErrors {
		h.Errors = append(h.Errors, message)
	}
}

func mergeHealth(left, right Health) Health {
	result := cloneHealth(left)
	result.Dropped += right.Dropped
	for _, message := range right.Errors {
		if len(result.Errors) >= maxHealthErrors {
			break
		}
		result.Errors = append(result.Errors, message)
	}
	if left.Status == StatusUnavailable || right.Status == StatusUnavailable {
		result.Status = StatusUnavailable
	} else if left.Status == StatusPartial || right.Status == StatusPartial {
		result.Status = StatusPartial
	} else {
		result.Status = StatusOK
	}
	result.Partial = result.Status != StatusOK || left.Partial || right.Partial
	return result
}

func cloneHealth(h Health) Health {
	h.Errors = append([]string(nil), h.Errors...)
	return h
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
