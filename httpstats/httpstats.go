// Package httpstats provides bounded, in-memory HTTP request measurements.
// It deliberately records URL paths, never query strings.
package httpstats

import (
	"fmt"
	"math/bits"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultMaxKeys is the maximum number of distinct HTTP identities held by
	// a Collector. New identities past the limit are merged into OverflowPath.
	DefaultMaxKeys = 10000
	// OverflowPath identifies observations merged after the key limit.
	OverflowPath = "(other)"

	numShards  = 16
	numBuckets = 40
)

// Rule replaces matching text in a request path. Supplying path rules disables
// the built-in numeric and UUID segment normalization.
type Rule struct {
	Pattern     *regexp.Regexp
	Replacement string
}

// Option configures a Collector.
type Option func(*config)

type config struct {
	maxKeys int
	rules   []Rule
}

// WithMaxKeys changes the distinct-key limit. A value of zero merges every
// observation into the overflow entry. Negative values are ignored.
func WithMaxKeys(max int) Option {
	return func(cfg *config) {
		if max >= 0 {
			cfg.maxKeys = max
		}
	}
}

// WithPathRules replaces the built-in path normalization with rules applied in
// order. Rules with a nil Pattern are ignored.
func WithPathRules(rules []Rule) Option {
	return func(cfg *config) {
		cfg.rules = append([]Rule(nil), rules...)
	}
}

// Entry is one aggregated HTTP identity. Duration fields are encoded as
// integer nanoseconds in JSON. P95 is the upper bound of a log2 bucket.
type Entry struct {
	Key        string        `json:"key"`
	Method     string        `json:"method"`
	Path       string        `json:"path"`
	Protocol   string        `json:"protocol"`
	Status     int           `json:"status"`
	Count      int64         `json:"count"`
	Total      time.Duration `json:"total_ns"`
	Avg        time.Duration `json:"avg_ns"`
	Max        time.Duration `json:"max_ns"`
	P95        time.Duration `json:"p95_ns"`
	TotalBytes int64         `json:"total_bytes"`
	AvgBytes   int64         `json:"avg_bytes"`
}

// Snapshot is a point-in-time copy sorted by total duration descending.
type Snapshot []Entry

// ConnSnapshot summarizes long-lived connections (WebSocket upgrades and
// text/event-stream responses) which are excluded from the latency table so
// they cannot distort p95/avg. Active spans generations; the rest reset
// with the generation.
type ConnSnapshot struct {
	Total        int64   `json:"total"`
	Active       int64   `json:"active"`
	AvgSeconds   float64 `json:"avg_seconds"`
	P95Seconds   float64 `json:"p95_seconds"`
	MaxSeconds   float64 `json:"max_seconds"`
	BytesRead    int64   `json:"bytes_read"`
	BytesWritten int64   `json:"bytes_written"`
}

type identity struct {
	method   string
	path     string
	protocol string
	status   int
}

type stat struct {
	count   int64
	total   int64
	max     int64
	bytes   int64
	buckets [numBuckets]int64
}

type shard struct {
	mu    sync.Mutex
	stats map[identity]*stat
}

type table struct {
	maxKeys int64
	keys    atomic.Int64
	shards  [numShards]shard
}

// Collector owns HTTP measurements and generation boundaries. Reset swaps in
// a fresh table and waits for requests already in flight, then returns their
// completed generation; the same mechanism backs the runctl.GenerationCollector
// implementation in generation.go.
//
// Lock order: mu may be held while acquiring connMu, never the reverse.
type Collector struct {
	mu      sync.Mutex
	resetMu sync.Mutex
	current *generation
	maxKeys int
	rules   []Rule

	// gens is the boundary bookkeeping, guarded by mu.
	gens generationState

	connMu      sync.Mutex
	connTotal   int64
	connActive  int64
	connDurSum  time.Duration
	connDurMax  time.Duration
	connRead    int64
	connWrote   int64
	connBuckets [64]int64
}

// Default is used by the package-level Middleware helper.
var Default = New()

// New returns an independent Collector.
func New(opts ...Option) *Collector {
	cfg := config{maxKeys: DefaultMaxKeys}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	c := &Collector{
		maxKeys: cfg.maxKeys,
		rules:   cfg.rules,
	}
	c.current = c.newGeneration()
	return c
}

// ParseRules parses an ISUTOOLS_PATH_RULES spec: semicolon-separated
// "regex=replacement" pairs, split on the LAST '=' so regexes may contain
// '='. Example: "^/@[^/]+$=/@*;^/posts/[0-9]+$=/posts/*".
func ParseRules(spec string) ([]Rule, error) {
	rules := []Rule{}
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.LastIndexByte(part, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("httpstats: rule %q is not regex=replacement", part)
		}
		pattern, err := regexp.Compile(part[:eq])
		if err != nil {
			return nil, fmt.Errorf("httpstats: rule %q: %w", part, err)
		}
		rules = append(rules, Rule{Pattern: pattern, Replacement: part[eq+1:]})
	}
	return rules, nil
}

// SetRules replaces the collector's path rules at runtime (e.g. from the
// ISUTOOLS_PATH_RULES environment variable).
func (c *Collector) SetRules(rules []Rule) {
	c.mu.Lock()
	c.rules = rules
	c.mu.Unlock()
}

// Middleware instruments next using Default.
func Middleware(next http.Handler) http.Handler {
	return Default.Middleware(next)
}

// Middleware returns a handler that measures method, normalized path,
// protocol, status, duration, and response body bytes.
func (c *Collector) Middleware(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g := c.begin()
		start := time.Now()
		tracker := &connectionTracker{collector: c, generation: g, started: start}
		capture := &responseWriter{ResponseWriter: w}
		capture.onCommit = func(status int) {
			if status == http.StatusSwitchingProtocols ||
				strings.HasPrefix(capture.Header().Get("Content-Type"), "text/event-stream") {
				tracker.start(false)
			}
		}
		capture.onHijack = func(conn net.Conn) net.Conn {
			tracker.start(true)
			return &trackedConn{Conn: conn, onClose: tracker.finishHijacked}
		}
		wrapped := preserveOptionalInterfaces(capture)

		defer func() {
			panicked := recover()
			if panicked != nil && capture.status == 0 {
				capture.status = http.StatusInternalServerError
			}
			if capture.status == 0 {
				capture.status = http.StatusOK
			}
			if strings.HasPrefix(capture.Header().Get("Content-Type"), "text/event-stream") {
				tracker.start(false)
			}
			if tracker.finishHandler(capture.bytes) {
				// Long-lived connections are released from the request generation
				// as soon as they are confirmed, so Reset never waits for them.
			} else {
				id := identity{
					method:   r.Method,
					path:     c.pathFor(r),
					protocol: r.Proto,
					status:   capture.status,
				}
				c.finish(g, id, time.Since(start), capture.bytes)
			}
			if panicked != nil {
				panic(panicked)
			}
		}()

		next.ServeHTTP(wrapped, r)
	})
}

// Snapshot returns the currently active generation without clearing it.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	t := c.current.table
	c.mu.Unlock()
	return t.snapshot()
}

// Connections reports long-lived connection stats for the current generation.
func (c *Collector) Connections() ConnSnapshot {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.connectionsLocked()
}

// takeConn returns the connection stats of the window that is closing and
// clears the counters. Active is carried over because those connections are
// still open and will be counted again when they close.
func (c *Collector) takeConn() ConnSnapshot {
	c.connMu.Lock()
	snap := c.connectionsLocked()
	c.connTotal, c.connDurSum, c.connDurMax = 0, 0, 0
	c.connRead, c.connWrote = 0, 0
	c.connBuckets = [64]int64{}
	c.connMu.Unlock()
	return snap
}

// connectionsLocked builds the connection summary. Callers hold connMu.
func (c *Collector) connectionsLocked() ConnSnapshot {
	snap := ConnSnapshot{
		Total: c.connTotal, Active: c.connActive,
		BytesRead: c.connRead, BytesWritten: c.connWrote,
	}
	if c.connTotal > 0 {
		snap.AvgSeconds = c.connDurSum.Seconds() / float64(c.connTotal)
		target := (c.connTotal*95 + 99) / 100
		var seen int64
		for bucket, count := range c.connBuckets {
			seen += count
			if count > 0 && seen >= target {
				if bucket == 0 {
					snap.P95Seconds = 0
				} else if bucket == len(c.connBuckets)-1 {
					snap.P95Seconds = c.connDurMax.Seconds()
				} else {
					upper := time.Duration(int64(1) << uint(bucket))
					if upper > c.connDurMax {
						upper = c.connDurMax
					}
					snap.P95Seconds = upper.Seconds()
				}
				break
			}
		}
	}
	snap.MaxSeconds = c.connDurMax.Seconds()
	return snap
}

func (c *Collector) connStart() {
	c.connMu.Lock()
	c.connActive++
	c.connMu.Unlock()
}

func (c *Collector) connFinish(d time.Duration, bytesRead, bytesWritten int64) {
	c.connMu.Lock()
	c.connActive--
	c.connTotal++
	c.connDurSum += d
	bucket := bits.Len64(uint64(max(d.Nanoseconds(), 0)))
	if bucket >= len(c.connBuckets) {
		bucket = len(c.connBuckets) - 1
	}
	c.connBuckets[bucket]++
	c.connRead += bytesRead
	c.connWrote += bytesWritten
	if d > c.connDurMax {
		c.connDurMax = d
	}
	c.connMu.Unlock()
}

// connectionTracker owns the one-way transition from an ordinary request to
// a confirmed long-lived connection. A requested Upgrade header alone is not
// confirmation: rejected handshakes stay in the ordinary latency table.
type connectionTracker struct {
	mu         sync.Mutex
	collector  *Collector
	generation *generation
	started    time.Time
	long       bool
	hijacked   bool
	finished   bool
}

func (t *connectionTracker) start(hijacked bool) {
	t.mu.Lock()
	if t.long {
		if hijacked {
			t.hijacked = true
		}
		t.mu.Unlock()
		return
	}
	t.long = true
	t.hijacked = hijacked
	t.mu.Unlock()
	t.collector.connStart()
	t.collector.release(t.generation)
}

func (t *connectionTracker) finishHandler(bytesWritten int64) bool {
	t.mu.Lock()
	if !t.long {
		t.mu.Unlock()
		return false
	}
	if t.hijacked || t.finished {
		t.mu.Unlock()
		return true
	}
	t.finished = true
	t.mu.Unlock()
	t.collector.connFinish(time.Since(t.started), 0, bytesWritten)
	return true
}

func (t *connectionTracker) finishHijacked(bytesRead, bytesWritten int64) {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	t.mu.Unlock()
	t.collector.connFinish(time.Since(t.started), bytesRead, bytesWritten)
}

type trackedConn struct {
	net.Conn
	read, wrote atomic.Int64
	closeOnce   sync.Once
	onClose     func(int64, int64)
}

func (c *trackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *trackedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.wrote.Add(int64(n))
	return n, err
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose(c.read.Load(), c.wrote.Load())
		}
	})
	return err
}

func newTable(maxKeys int) *table {
	t := &table{maxKeys: int64(maxKeys)}
	for i := range t.shards {
		t.shards[i].stats = make(map[identity]*stat)
	}
	return t
}

func (t *table) observe(id identity, duration time.Duration, responseBytes int64) {
	if duration < 0 {
		duration = 0
	}
	if responseBytes < 0 {
		responseBytes = 0
	}
	sh := &t.shards[hashIdentity(id)%numShards]
	sh.mu.Lock()
	s, ok := sh.stats[id]
	if !ok && id.path != OverflowPath {
		if !t.reserveKey() {
			sh.mu.Unlock()
			t.observe(identity{method: "*", path: OverflowPath, protocol: "*"}, duration, responseBytes)
			return
		}
		s = &stat{}
		sh.stats[id] = s
	} else if !ok {
		s = &stat{}
		sh.stats[id] = s
	}
	ns := duration.Nanoseconds()
	s.count++
	s.total += ns
	s.bytes += responseBytes
	if ns > s.max {
		s.max = ns
	}
	s.buckets[bucketFor(ns)]++
	sh.mu.Unlock()
}

// clear drops every recorded identity and restores the key budget. A request
// that lands here after its generation was released re-populates an empty map
// instead of hitting a nil one.
func (t *table) clear() {
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		sh.stats = make(map[identity]*stat)
		sh.mu.Unlock()
	}
	t.keys.Store(0)
}

func (t *table) reserveKey() bool {
	for {
		used := t.keys.Load()
		if used >= t.maxKeys {
			return false
		}
		if t.keys.CompareAndSwap(used, used+1) {
			return true
		}
	}
}

func (t *table) snapshot() Snapshot {
	entries := make(Snapshot, 0, 64)
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for id, s := range sh.stats {
			entries = append(entries, Entry{
				Key:        displayKey(id),
				Method:     id.method,
				Path:       id.path,
				Protocol:   id.protocol,
				Status:     id.status,
				Count:      s.count,
				Total:      time.Duration(s.total),
				Avg:        time.Duration(s.total / s.count),
				Max:        time.Duration(s.max),
				P95:        time.Duration(p95(s)),
				TotalBytes: s.bytes,
				AvgBytes:   s.bytes / s.count,
			})
		}
		sh.mu.Unlock()
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Total != entries[j].Total {
			return entries[i].Total > entries[j].Total
		}
		return entries[i].Key < entries[j].Key
	})
	return entries
}

func displayKey(id identity) string {
	return id.method + " " + id.path + " " + id.protocol + " " + strconv.Itoa(id.status)
}

func hashIdentity(id identity) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	write := func(value string) {
		for i := 0; i < len(value); i++ {
			h ^= uint32(value[i])
			h *= prime32
		}
		h ^= 0xff
		h *= prime32
	}
	write(id.method)
	write(id.path)
	write(id.protocol)
	write(strconv.Itoa(id.status))
	return h
}

func bucketFor(ns int64) int {
	b := bits.Len64(uint64(ns))
	if b >= numBuckets {
		return numBuckets - 1
	}
	return b
}

func p95(s *stat) int64 {
	target := (s.count*95 + 99) / 100
	var cumulative int64
	for i, count := range s.buckets {
		cumulative += count
		if count > 0 && cumulative >= target {
			if i == 0 {
				return 0
			}
			if i == numBuckets-1 {
				return s.max
			}
			return int64(1) << uint(i)
		}
	}
	return s.max
}

var (
	uuidSegment = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	// A canonical ULID is 128 bits encoded as 26 Crockford Base32
	// characters. The first character is limited to 0-7 so arbitrary
	// 26-character slugs are not collapsed.
	ulidSegment = regexp.MustCompile(`(?i)^[0-7][0-9a-hjkmnp-tv-z]{25}$`)
)

func (c *Collector) pathFor(r *http.Request) string {
	if pattern := requestPatternPath(r.Pattern); pattern != "" {
		return pattern
	}
	path := r.URL.Path
	c.mu.Lock()
	rules := c.rules
	c.mu.Unlock()
	if rules != nil {
		for _, rule := range rules {
			if rule.Pattern != nil {
				path = rule.Pattern.ReplaceAllString(path, rule.Replacement)
			}
		}
		return path
	}
	return normalizePath(path)
}

func requestPatternPath(pattern string) string {
	fields := strings.Fields(pattern)
	if len(fields) == 0 {
		return ""
	}
	path := fields[len(fields)-1]
	if strings.HasPrefix(path, "/") {
		return path
	}
	if slash := strings.IndexByte(path, '/'); slash >= 0 {
		return path[slash:]
	}
	return ""
}

func normalizePath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		base, extension := segment, ""
		if dot := strings.LastIndexByte(segment, '.'); dot > 0 {
			base, extension = segment[:dot], segment[dot:]
		}
		if isDecimal(base) || uuidSegment.MatchString(base) || ulidSegment.MatchString(base) {
			segments[i] = "*" + extension
		}
	}
	return strings.Join(segments, "/")
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
