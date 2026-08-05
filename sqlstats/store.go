package sqlstats

import (
	"context"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/generation"
)

// Frozen is a completed SQL generation.
type Frozen struct {
	Generation int64
	Entries    []agg.Entry
	// CutShort reports that the rotation gave up on its bound with queries still
	// running in the generation it closed, so Entries may be missing their rows.
	// Callers publish such a generation as a partial section: a rotation that
	// truncates silently reports an under-counted SQL table as a whole one.
	CutShort bool
}

// RotateDrainBudget bounds how long Rotate waits for the queries pinned to the
// generation it closes.
//
// It is generation.DefaultCompatWait, which is runctl.DrainBudget: a rotation
// is the drain step of a run boundary expressed through the pre-generation API,
// and httpstats.ResetDrainBudget names the same authority for the same reason.
// TestRotateDrainBudgetIsTheSharedDrainBudget fails if they ever diverge.
const RotateDrainBudget = generation.DefaultCompatWait

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
//
// The wait is bounded by RotateDrainBudget, or by whatever SetRotateDrainBudget
// installed, so a query that never returns delays the rotation instead of
// parking it. A rotation that gave up returns Frozen.CutShort, which is the
// caller's cue to publish the generation as partial. Callers holding a request
// context should use RotateContext.
func (s *Store) Rotate() Frozen {
	return s.RotateContext(context.Background())
}

// RotateContext is Rotate bounded by the caller's context as well as by the
// drain budget, whichever ends first. A nil context means the budget alone.
//
// It exists because Rotate runs on the /reset path while the handler holds the
// process-wide reset lock and the operation slot: without the caller's context
// a rotation waiting out a wedged query head-of-line-blocks every other admin
// endpoint for the whole budget, long after the request that asked for it is
// gone.
func (s *Store) RotateContext(ctx context.Context) Frozen {
	frozen := s.manager.SwapAndSnapshotContext(ctx)
	return Frozen{Generation: frozen.Generation, Entries: frozen.Value, CutShort: frozen.CutShort}
}

// SetRotateDrainBudget bounds the wait performed by Rotate. A non-positive
// value restores RotateDrainBudget. It exists for callers whose own operation
// budget is tighter than the run controller's; the default is already bounded,
// so leaving it alone is safe.
func (s *Store) SetRotateDrainBudget(budget time.Duration) {
	s.manager.SetCompatWait(budget)
}

// Reset starts a new generation and discards the frozen data. Prefer Rotate
// when the caller needs to retain the previous generation.
func (s *Store) Reset() { _ = s.Rotate() }
