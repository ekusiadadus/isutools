package accesslog

import (
	"math/bits"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxKeys bounds distinct method/path rows in one generation.
	DefaultMaxKeys = 10000
	// OverflowURI is used when the bounded key budget is exhausted.
	OverflowURI   = "(other)"
	maxDimensions = 64
	numBuckets    = 64
)

// DimensionCount is a deterministic value/count pair used for cache status
// and content type breakdowns.
type DimensionCount struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// Entry is one method/path aggregate. Status 101 contributes to count and
// bytes, but is deliberately excluded from latency and residual fields.
type Entry struct {
	Method string `json:"method"`
	URI    string `json:"uri"`

	Count        int64         `json:"count"`
	LatencyCount int64         `json:"latency_count"`
	RequestTotal time.Duration `json:"request_total_ns"`
	RequestAvg   time.Duration `json:"request_avg_ns"`
	RequestP95   time.Duration `json:"request_p95_ns"`
	RequestMax   time.Duration `json:"request_max_ns"`

	BytesTotal int64 `json:"bytes_total"`
	BytesAvg   int64 `json:"bytes_avg"`

	UpstreamTotal    time.Duration `json:"upstream_total_ns"`
	UpstreamAttempts int64         `json:"upstream_attempts"`
	ResidualTotal    time.Duration `json:"residual_total_ns"`
	ResidualCount    int64         `json:"residual_count"`
	NoUpstreamCount  int64         `json:"no_upstream_timing_count"`

	Status304 int64 `json:"status_304"`
	Status499 int64 `json:"status_499"`
	Status5xx int64 `json:"status_5xx"`

	CacheStatuses []DimensionCount `json:"cache_statuses"`
	ContentTypes  []DimensionCount `json:"content_types"`
}

// Snapshot is an immutable copy of one access-log generation.
type Snapshot struct {
	Entries      []Entry     `json:"entries"`
	Flows        []FlowEntry `json:"flows,omitempty"`
	Lines        int64       `json:"lines"`
	PartialLines int64       `json:"partial_lines"`
	Health       Health      `json:"health"`
}

// FlowEntry is one observed session transition (previous request -> next).
// Aggregated from the pseudonymous sess: log field, it shows how users
// actually move through the application.
type FlowEntry struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int64  `json:"count"`
}

// maxFlowSessions bounds the per-generation session tracking map.
const maxFlowSessions = 10000

type aggregateStat struct {
	method, uri string
	count       int64
	latency     int64
	totalNS     int64
	maxNS       int64
	buckets     [numBuckets]int64
	bytes       int64
	upstreamNS  int64
	upAttempts  int64
	residualNS  int64
	residualN   int64
	noUpstream  int64
	status304   int64
	status499   int64
	status5xx   int64
	cache       map[string]int64
	ctype       map[string]int64
}

// Aggregator is a concurrency-safe bounded access-log aggregation table.
type Aggregator struct {
	mu           sync.Mutex
	maxKeys      int
	stats        map[string]*aggregateStat
	lines        int64
	partialLines int64
	lastBySess   map[string]string
	flows        map[string]int64
}

// NewAggregator returns an empty table. A non-positive maxKeys selects the
// default bound.
func NewAggregator(maxKeys int) *Aggregator {
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	return &Aggregator{
		maxKeys:    maxKeys,
		stats:      make(map[string]*aggregateStat),
		lastBySess: make(map[string]string),
		flows:      make(map[string]int64),
	}
}

// Observe adds a parsed record.
func (a *Aggregator) Observe(rec Record) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lines++
	if rec.Partial {
		a.partialLines++
	}
	if rec.Session != "" {
		page := rec.Method + " " + rec.URI
		if prev, ok := a.lastBySess[rec.Session]; ok && prev != page {
			a.flows[prev+"\x00"+page]++
		}
		if len(a.lastBySess) < maxFlowSessions {
			a.lastBySess[rec.Session] = page
		} else if _, ok := a.lastBySess[rec.Session]; ok {
			a.lastBySess[rec.Session] = page
		}
	}
	key := rec.Method + "\x00" + rec.URI
	st := a.stats[key]
	if st == nil {
		if len(a.stats) >= a.maxKeys {
			key = "\x00" + OverflowURI
			st = a.stats[key]
		}
		if st == nil {
			method, uri := rec.Method, rec.URI
			if key == "\x00"+OverflowURI {
				method, uri = "", OverflowURI
			}
			st = &aggregateStat{method: method, uri: uri, cache: make(map[string]int64), ctype: make(map[string]int64)}
			a.stats[key] = st
		}
	}
	st.count++
	st.bytes += rec.Bytes
	addDimension(st.cache, rec.CacheStatus)
	addDimension(st.ctype, rec.ContentType)
	switch {
	case rec.Status == 304:
		st.status304++
	case rec.Status == 499:
		st.status499++
	case rec.Status >= 500:
		st.status5xx++
	}
	// A WebSocket access-log record is written when the connection closes, so
	// its request/upstream timings are not comparable to ordinary requests.
	if rec.Status == 101 {
		return
	}
	if rec.NoUpstreamTiming {
		st.noUpstream++
	}
	if rec.UpstreamValid {
		st.upstreamNS += rec.UpstreamTotal.Nanoseconds()
		st.upAttempts += int64(rec.UpstreamAttempts)
	}
	ns := rec.RequestTime.Nanoseconds()
	st.latency++
	st.totalNS += ns
	if ns > st.maxNS {
		st.maxNS = ns
	}
	st.buckets[durationBucket(ns)]++
	if rec.UpstreamValid && rec.UpstreamComplete && rec.RequestTime >= rec.UpstreamTotal {
		st.residualNS += (rec.RequestTime - rec.UpstreamTotal).Nanoseconds()
		st.residualN++
	}
}

// Snapshot returns rows sorted by total request time descending.
func (a *Aggregator) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := Snapshot{Lines: a.lines, PartialLines: a.partialLines, Entries: make([]Entry, 0, len(a.stats))}
	for key, count := range a.flows {
		from, to, _ := strings.Cut(key, "\x00")
		result.Flows = append(result.Flows, FlowEntry{From: from, To: to, Count: count})
	}
	sort.Slice(result.Flows, func(i, j int) bool {
		if result.Flows[i].Count != result.Flows[j].Count {
			return result.Flows[i].Count > result.Flows[j].Count
		}
		return result.Flows[i].From+result.Flows[i].To < result.Flows[j].From+result.Flows[j].To
	})
	if len(result.Flows) > 20 {
		result.Flows = result.Flows[:20]
	}
	for _, st := range a.stats {
		e := Entry{
			Method: st.method, URI: st.uri, Count: st.count, LatencyCount: st.latency,
			RequestTotal: time.Duration(st.totalNS), RequestMax: time.Duration(st.maxNS),
			RequestP95: time.Duration(percentile95(st)), BytesTotal: st.bytes,
			UpstreamTotal: time.Duration(st.upstreamNS), UpstreamAttempts: st.upAttempts,
			ResidualTotal: time.Duration(st.residualNS), ResidualCount: st.residualN,
			NoUpstreamCount: st.noUpstream, Status304: st.status304, Status499: st.status499,
			Status5xx: st.status5xx, CacheStatuses: dimensions(st.cache), ContentTypes: dimensions(st.ctype),
		}
		if st.latency > 0 {
			e.RequestAvg = time.Duration(st.totalNS / st.latency)
		}
		if st.count > 0 {
			e.BytesAvg = st.bytes / st.count
		}
		result.Entries = append(result.Entries, e)
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		if result.Entries[i].RequestTotal != result.Entries[j].RequestTotal {
			return result.Entries[i].RequestTotal > result.Entries[j].RequestTotal
		}
		if result.Entries[i].URI != result.Entries[j].URI {
			return result.Entries[i].URI < result.Entries[j].URI
		}
		return result.Entries[i].Method < result.Entries[j].Method
	})
	return result
}

// Reset clears the table and restores its key budget.
func (a *Aggregator) Reset() {
	a.mu.Lock()
	a.stats = make(map[string]*aggregateStat)
	a.lines = 0
	a.partialLines = 0
	a.lastBySess = make(map[string]string)
	a.flows = make(map[string]int64)
	a.mu.Unlock()
}

func addDimension(values map[string]int64, value string) {
	if _, exists := values[value]; !exists && len(values) >= maxDimensions {
		value = OverflowURI
	}
	values[value]++
}

func dimensions(values map[string]int64) []DimensionCount {
	result := make([]DimensionCount, 0, len(values))
	for value, count := range values {
		result = append(result, DimensionCount{Value: value, Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Count != result[j].Count {
			return result[i].Count > result[j].Count
		}
		return result[i].Value < result[j].Value
	})
	return result
}

func durationBucket(ns int64) int {
	b := bits.Len64(uint64(ns))
	if b >= numBuckets {
		b = numBuckets - 1
	}
	return b
}

func percentile95(st *aggregateStat) int64 {
	if st.latency == 0 {
		return 0
	}
	target := (st.latency*95 + 99) / 100
	var seen int64
	for i, count := range st.buckets {
		seen += count
		if count > 0 && seen >= target {
			if i == 0 {
				return 0
			}
			if i == numBuckets-1 {
				return st.maxNS
			}
			return int64(1) << uint(i)
		}
	}
	return st.maxNS
}
