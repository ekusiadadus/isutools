package sqlstats

import (
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/generation"
)

// Frozen is a completed SQL generation.
type Frozen struct {
	Generation int64
	Entries    []agg.Entry
}

// Store owns generation-scoped SQL aggregation tables.
type Store struct {
	manager *generation.Manager[*agg.Table, []agg.Entry]
}

// NewStore constructs a generation-scoped SQL store.
func NewStore(maxKeys int) *Store {
	return &Store{manager: generation.New(
		func() *agg.Table { return agg.NewTable(maxKeys) },
		func(table *agg.Table) []agg.Entry { return table.Snapshot() },
	)}
}

type measurement struct {
	lease *generation.Lease[*agg.Table, []agg.Entry]
	once  sync.Once
}

func (s *Store) begin() *measurement {
	return &measurement{lease: s.manager.Acquire()}
}

func (s *Store) finish(m *measurement, query string, duration time.Duration, failed bool) {
	if m == nil {
		return
	}
	m.once.Do(func() {
		defer m.lease.Done()
		m.lease.Value().ObserveResult(query, duration, failed)
	})
}

func (s *Store) discard(m *measurement) {
	if m == nil {
		return
	}
	m.once.Do(m.lease.Done)
}

// Observe adds an already-normalized query to the current generation.
func (s *Store) Observe(query string, duration time.Duration) {
	s.ObserveResult(query, duration, false)
}

// ObserveResult adds an already-normalized query result to the current generation.
func (s *Store) ObserveResult(query string, duration time.Duration, failed bool) {
	m := s.begin()
	s.finish(m, query, duration, failed)
}

// Snapshot returns the current generation's SQL aggregates.
func (s *Store) Snapshot() []agg.Entry {
	return s.manager.Snapshot().Value
}

// CurrentGeneration identifies the generation accepting new observations.
func (s *Store) CurrentGeneration() int64 {
	return s.manager.CurrentGeneration()
}

// Rotate publishes a new empty generation and freezes the previous one after
// all observations that started there have completed.
func (s *Store) Rotate() Frozen {
	frozen := s.manager.SwapAndSnapshot()
	return Frozen{Generation: frozen.Generation, Entries: frozen.Value}
}

// Reset starts a new generation and discards the frozen data. Prefer Rotate
// when the caller needs to retain the previous generation.
func (s *Store) Reset() { _ = s.Rotate() }
