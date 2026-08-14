// Package redisstats records Redis-compatible command latency without ever
// retaining keys, values, arguments, or connection strings.
package redisstats

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
)

const (
	DefaultMaxCommands = 256
	MaxCommandBytes    = 64
	OverflowCommand    = "(other)"
)

// Entry is one sanitized command aggregate.
type Entry struct {
	Command    string        `json:"command"`
	Count      int64         `json:"count"`
	ErrorCount int64         `json:"error_count"`
	Total      time.Duration `json:"total_ns"`
	Avg        time.Duration `json:"avg_ns"`
	Max        time.Duration `json:"max_ns"`
	P95        time.Duration `json:"p95_ns"`
}

// Registry is safe for concurrent clients and atomic generation rotation.
type Registry struct {
	mu      sync.RWMutex
	table   *agg.Table
	names   map[string]struct{}
	max     int
	dropped uint64
}

var Default = NewRegistry(DefaultMaxCommands)

func NewRegistry(maxCommands int) *Registry {
	if maxCommands < 0 {
		maxCommands = DefaultMaxCommands
	}
	return &Registry{table: agg.NewTable(maxCommands + 1), names: map[string]struct{}{}, max: maxCommands}
}

// Observe records only the upper-case command token. Supplying a full command
// such as "GET private-key" is safe: everything after the first whitespace is
// discarded before the registry is touched.
func (r *Registry) Observe(command string, duration time.Duration, err error) {
	if r == nil {
		return
	}
	command, safe := sanitizeCommand(command)
	r.mu.Lock()
	if !safe {
		command = OverflowCommand
		r.dropped++
	} else if _, exists := r.names[command]; !exists {
		if len(r.names) >= r.max {
			command = OverflowCommand
			r.dropped++
		} else {
			r.names[command] = struct{}{}
		}
	}
	r.table.ObserveResult(command, duration, err != nil)
	r.mu.Unlock()
}

func (r *Registry) Snapshot() []Entry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	entries := entriesFromAgg(r.table.Snapshot())
	r.mu.RUnlock()
	return entries
}

func (r *Registry) Dropped() uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	dropped := r.dropped
	r.mu.RUnlock()
	return dropped
}

func (r *Registry) Reset() { r.rotate() }

func (r *Registry) rotate() (*agg.Table, uint64) {
	r.mu.Lock()
	old, dropped := r.table, r.dropped
	r.table = agg.NewTable(r.max + 1)
	r.names = map[string]struct{}{}
	r.dropped = 0
	r.mu.Unlock()
	return old, dropped
}

func entriesFromAgg(values []agg.Entry) []Entry {
	entries := make([]Entry, 0, len(values))
	for _, value := range values {
		entries = append(entries, Entry{
			Command: value.Key, Count: value.Count, ErrorCount: value.ErrorCount,
			Total: value.Total, Avg: value.Avg, Max: value.Max, P95: value.P95,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Total != entries[j].Total {
			return entries[i].Total > entries[j].Total
		}
		return entries[i].Command < entries[j].Command
	})
	return entries
}

func sanitizeCommand(command string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	command = strings.ToUpper(fields[0])
	if len(command) > MaxCommandBytes {
		return "", false
	}
	for i := range len(command) {
		c := command[i]
		if c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			continue
		}
		return "", false
	}
	return command, true
}
