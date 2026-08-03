// Package httpstats provides bounded, in-memory HTTP request measurements.
// It deliberately records URL paths, never query strings.
package httpstats

import (
	"bufio"
	"io"
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

type generation struct {
	table    *table
	inFlight int
}

// Collector owns HTTP measurements and generation boundaries. Reset swaps in
// a fresh table and waits for requests already in flight, then returns their
// completed generation.
type Collector struct {
	mu      sync.Mutex
	resetMu sync.Mutex
	changed *sync.Cond
	current *generation
	maxKeys int
	rules   []Rule
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
		current: &generation{table: newTable(cfg.maxKeys)},
	}
	c.changed = sync.NewCond(&c.mu)
	return c
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
		capture := &responseWriter{ResponseWriter: w}
		wrapped := preserveOptionalInterfaces(capture)

		defer func() {
			panicked := recover()
			if panicked != nil && capture.status == 0 {
				capture.status = http.StatusInternalServerError
			}
			if capture.status == 0 {
				capture.status = http.StatusOK
			}
			id := identity{
				method:   r.Method,
				path:     c.pathFor(r),
				protocol: r.Proto,
				status:   capture.status,
			}
			c.finish(g, id, time.Since(start), capture.bytes)
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

// Reset atomically starts a new generation, waits for requests that started in
// the old one to finish, and returns the completed old generation.
func (c *Collector) Reset() Snapshot {
	c.resetMu.Lock()
	defer c.resetMu.Unlock()
	c.mu.Lock()
	old := c.current
	c.current = &generation{table: newTable(c.maxKeys)}
	for old.inFlight != 0 {
		c.changed.Wait()
	}
	c.mu.Unlock()
	return old.table.snapshot()
}

func (c *Collector) begin() *generation {
	c.mu.Lock()
	g := c.current
	g.inFlight++
	c.mu.Unlock()
	return g
}

func (c *Collector) finish(g *generation, id identity, duration time.Duration, responseBytes int64) {
	g.table.observe(id, duration, responseBytes)
	c.mu.Lock()
	g.inFlight--
	if g.inFlight == 0 {
		c.changed.Broadcast()
	}
	c.mu.Unlock()
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
			return int64(1) << uint(i)
		}
	}
	return s.max
}

var uuidSegment = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (c *Collector) pathFor(r *http.Request) string {
	if pattern := requestPatternPath(r.Pattern); pattern != "" {
		return pattern
	}
	path := r.URL.Path
	if c.rules != nil {
		for _, rule := range c.rules {
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
		if isDecimal(base) || uuidSegment.MatchString(base) {
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

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.ensureStatus()
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *responseWriter) ensureStatus() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
}

type flushFeature struct {
	w *responseWriter
	f http.Flusher
}

func (f flushFeature) Flush() {
	f.w.ensureStatus()
	f.f.Flush()
}

type hijackFeature struct{ h http.Hijacker }

func (h hijackFeature) Hijack() (net.Conn, *bufio.ReadWriter, error) { return h.h.Hijack() }

type pushFeature struct{ p http.Pusher }

func (p pushFeature) Push(target string, opts *http.PushOptions) error { return p.p.Push(target, opts) }

type readFromFeature struct {
	w  *responseWriter
	rf io.ReaderFrom
}

func (r readFromFeature) ReadFrom(src io.Reader) (int64, error) {
	r.w.ensureStatus()
	n, err := r.rf.ReadFrom(src)
	r.w.bytes += n
	return n, err
}

func preserveOptionalInterfaces(w *responseWriter) http.ResponseWriter {
	f, hasF := w.ResponseWriter.(http.Flusher)
	h, hasH := w.ResponseWriter.(http.Hijacker)
	p, hasP := w.ResponseWriter.(http.Pusher)
	rf, hasR := w.ResponseWriter.(io.ReaderFrom)
	mask := 0
	if hasF {
		mask |= 1
	}
	if hasH {
		mask |= 2
	}
	if hasP {
		mask |= 4
	}
	if hasR {
		mask |= 8
	}
	ff := flushFeature{w: w, f: f}
	hf := hijackFeature{h: h}
	pf := pushFeature{p: p}
	rff := readFromFeature{w: w, rf: rf}
	switch mask {
	case 0:
		return w
	case 1:
		return struct {
			*responseWriter
			flushFeature
		}{w, ff}
	case 2:
		return struct {
			*responseWriter
			hijackFeature
		}{w, hf}
	case 3:
		return struct {
			*responseWriter
			flushFeature
			hijackFeature
		}{w, ff, hf}
	case 4:
		return struct {
			*responseWriter
			pushFeature
		}{w, pf}
	case 5:
		return struct {
			*responseWriter
			flushFeature
			pushFeature
		}{w, ff, pf}
	case 6:
		return struct {
			*responseWriter
			hijackFeature
			pushFeature
		}{w, hf, pf}
	case 7:
		return struct {
			*responseWriter
			flushFeature
			hijackFeature
			pushFeature
		}{w, ff, hf, pf}
	case 8:
		return struct {
			*responseWriter
			readFromFeature
		}{w, rff}
	case 9:
		return struct {
			*responseWriter
			flushFeature
			readFromFeature
		}{w, ff, rff}
	case 10:
		return struct {
			*responseWriter
			hijackFeature
			readFromFeature
		}{w, hf, rff}
	case 11:
		return struct {
			*responseWriter
			flushFeature
			hijackFeature
			readFromFeature
		}{w, ff, hf, rff}
	case 12:
		return struct {
			*responseWriter
			pushFeature
			readFromFeature
		}{w, pf, rff}
	case 13:
		return struct {
			*responseWriter
			flushFeature
			pushFeature
			readFromFeature
		}{w, ff, pf, rff}
	case 14:
		return struct {
			*responseWriter
			hijackFeature
			pushFeature
			readFromFeature
		}{w, hf, pf, rff}
	default:
		return struct {
			*responseWriter
			flushFeature
			hijackFeature
			pushFeature
			readFromFeature
		}{w, ff, hf, pf, rff}
	}
}
