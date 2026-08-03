// Package agg is the shared aggregation core: a concurrency-safe, bounded
// key→latency table with log2-bucket histograms for approximate percentiles.
package agg

import (
	"math/bits"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	numShards = 16
	// numBuckets covers durations up to ~2^39ns (~9 min) in log2 buckets.
	numBuckets = 40
	// DefaultMaxKeys bounds distinct keys per table. Observations for new
	// keys beyond the cap are merged into OverflowKey instead of being
	// silently dropped.
	DefaultMaxKeys = 10000
	// OverflowKey is the merged bucket for observations past the key cap.
	OverflowKey = "(other)"
)

type stat struct {
	count      int64
	errorCount int64
	total      int64 // nanoseconds
	max        int64
	buckets    [numBuckets]int64
}

type shard struct {
	mu    sync.Mutex
	stats map[string]*stat
}

// Table aggregates durations by key. All methods are safe for concurrent use.
type Table struct {
	maxKeys int64
	keys    atomic.Int64
	shards  [numShards]shard
}

// Entry is one aggregated row of a Snapshot. Duration fields marshal to
// JSON as integer nanoseconds.
type Entry struct {
	Key        string        `json:"key"`
	Count      int64         `json:"count"`
	ErrorCount int64         `json:"error_count"`
	Total      time.Duration `json:"total_ns"`
	Avg        time.Duration `json:"avg_ns"`
	Max        time.Duration `json:"max_ns"`
	P95        time.Duration `json:"p95_ns"`
}

// NewTable returns a Table holding at most maxKeys distinct keys.
func NewTable(maxKeys int) *Table {
	t := &Table{maxKeys: int64(maxKeys)}
	for i := range t.shards {
		t.shards[i].stats = make(map[string]*stat)
	}
	return t
}

// Observe records one duration for key. New keys are reserved atomically so
// concurrent observations cannot exceed the configured normal-key budget.
func (t *Table) Observe(key string, d time.Duration) {
	t.ObserveResult(key, d, false)
}

// ObserveResult records one duration and whether the operation failed.
func (t *Table) ObserveResult(key string, d time.Duration, failed bool) {
	ns := d.Nanoseconds()
	if ns < 0 {
		ns = 0
	}
	sh := &t.shards[fnv32a(key)%numShards]
	sh.mu.Lock()
	s, ok := sh.stats[key]
	if !ok {
		if key != OverflowKey && !t.reserveKey() {
			sh.mu.Unlock()
			t.ObserveResult(OverflowKey, d, failed)
			return
		}
		s = &stat{}
		sh.stats[key] = s
	}
	s.count++
	if failed {
		s.errorCount++
	}
	s.total += ns
	if ns > s.max {
		s.max = ns
	}
	s.buckets[bucketFor(ns)]++
	sh.mu.Unlock()
}

func (t *Table) reserveKey() bool {
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

// Snapshot returns all entries sorted by total time descending
// (ties broken by key for determinism).
func (t *Table) Snapshot() []Entry {
	entries := make([]Entry, 0, 64)
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for k, s := range sh.stats {
			entries = append(entries, Entry{
				Key:        k,
				Count:      s.count,
				ErrorCount: s.errorCount,
				Total:      time.Duration(s.total),
				Avg:        time.Duration(s.total / s.count),
				Max:        time.Duration(s.max),
				P95:        time.Duration(p95(s)),
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

// Reset clears all entries and restores the key budget.
func (t *Table) Reset() {
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		sh.stats = make(map[string]*stat)
		sh.mu.Unlock()
	}
	t.keys.Store(0)
}

// bucketFor maps ns into its log2 bucket: bucket i holds [2^(i-1), 2^i).
func bucketFor(ns int64) int {
	b := bits.Len64(uint64(ns))
	if b >= numBuckets {
		b = numBuckets - 1
	}
	return b
}

// p95 returns the upper bound of the bucket containing the 95th percentile.
func p95(s *stat) int64 {
	target := (s.count*95 + 99) / 100
	var cum int64
	for i, c := range s.buckets {
		cum += c
		if c > 0 && cum >= target {
			if i == 0 {
				return 0
			}
			return int64(1) << uint(i)
		}
	}
	return s.max
}

func fnv32a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}
