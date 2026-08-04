// Package counters is the generic user-defined counter API: one line in
// application code (isutools.Count("cache_hit")) makes cache hit/miss and
// similar custom events visible per benchmark generation.
package counters

import (
	"sort"
	"strings"
	"sync"
)

const (
	// DefaultMaxNames bounds dynamic counter identities per generation.
	DefaultMaxNames = 1024
	// MaxNameBytes bounds the retained memory for each identity.
	MaxNameBytes = 128
	// OverflowName receives observations outside either bound.
	OverflowName = "(other)"
)

// Entry is one counter value in a snapshot.
type Entry struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// Registry is a concurrency-safe named counter set.
type Registry struct {
	mu      sync.Mutex
	counts  map[string]int64
	dropped uint64
}

// Default is the registry the facade helpers write into.
var Default = NewRegistry()

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{counts: map[string]int64{}}
}

// Add increments a named counter by delta.
func (r *Registry) Add(name string, delta int64) {
	r.mu.Lock()
	name = strings.TrimSpace(name)
	if name == "" || len(name) > MaxNameBytes {
		name = OverflowName
		r.dropped++
	} else if _, exists := r.counts[name]; !exists && len(r.counts) >= DefaultMaxNames {
		name = OverflowName
		r.dropped++
	}
	r.counts[name] += delta
	r.mu.Unlock()
}

// Dropped returns how many observations were merged into OverflowName.
func (r *Registry) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Snapshot returns entries sorted by count descending (ties by name).
func (r *Registry) Snapshot() []Entry {
	r.mu.Lock()
	entries := make([]Entry, 0, len(r.counts))
	for name, count := range r.counts {
		entries = append(entries, Entry{Name: name, Count: count})
	}
	r.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

// Reset clears all counters (called per generation).
func (r *Registry) Reset() {
	r.mu.Lock()
	r.counts = map[string]int64{}
	r.dropped = 0
	r.mu.Unlock()
}
