// Package health records collector degradation without making failures fatal
// to the instrumented application.
package health

import (
	"sort"
	"sync"
)

// Status is the current state of a collector.
type Status string

const (
	StatusOK Status = "ok"
	// StatusInfo is useful configuration context, not an incomplete
	// measurement. Like disabled, it does not make a snapshot partial.
	StatusInfo     Status = "info"
	StatusDegraded Status = "degraded"
	StatusFailed   Status = "failed"
	StatusDisabled Status = "disabled"
)

// Entry is the machine-readable health state included in snapshots.
type Entry struct {
	Collector string `json:"collector"`
	Status    Status `json:"status"`
	Message   string `json:"message,omitempty"`
	Dropped   uint64 `json:"dropped"`
}

// Registry is a concurrency-safe collection of collector health entries.
type Registry struct {
	mu      sync.Mutex
	entries map[string]Entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]Entry)}
}

// Set replaces a collector's status and message while preserving its dropped
// count for the current generation.
func (r *Registry) Set(collector string, status Status, message string) {
	r.mu.Lock()
	entry := r.entries[collector]
	entry.Collector = collector
	entry.Status = status
	entry.Message = message
	r.entries[collector] = entry
	r.mu.Unlock()
}

// AddDropped increments the number of measurements that could not be retained.
func (r *Registry) AddDropped(collector string, count uint64) {
	if count == 0 {
		return
	}
	r.mu.Lock()
	entry := r.entries[collector]
	entry.Collector = collector
	if entry.Status == "" {
		entry.Status = StatusDegraded
	}
	entry.Dropped += count
	r.entries[collector] = entry
	r.mu.Unlock()
}

// ResetDropped starts a fresh generation of drop counters. Collector status
// remains visible because configuration and startup failures may persist.
func (r *Registry) ResetDropped() {
	r.mu.Lock()
	for name, entry := range r.entries {
		entry.Dropped = 0
		r.entries[name] = entry
	}
	r.mu.Unlock()
}

// Snapshot returns collector entries sorted by name and whether any enabled
// collector is incomplete.
func (r *Registry) Snapshot() ([]Entry, bool) {
	r.mu.Lock()
	entries := make([]Entry, 0, len(r.entries))
	partial := false
	for _, entry := range r.entries {
		entries = append(entries, entry)
		if entry.Dropped > 0 || entry.Status == StatusDegraded || entry.Status == StatusFailed {
			partial = true
		}
	}
	r.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Collector < entries[j].Collector
	})
	return entries, partial
}
