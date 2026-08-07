package sqlstats

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/generation"
	"github.com/ekusiadadus/isutools/internal/runctl"
)

// SectionName is the snapshot section this collector fills. It is the key the
// existing snapshot and health output already use for SQL, so a run snapshot
// and a legacy snapshot name the same data the same way.
const SectionName = "sql"

// Handle errors. They are sentinels because the run controller maps a Collect
// failure onto a stable machine-readable code and must be able to tell "this
// handle is not mine" from "this generation has no data yet".
var (
	// ErrForeignHandle rejects a handle minted by another collector or by
	// another instance of this one. Returning it is what keeps a mismatched
	// handle from being interpreted as a valid generation.
	ErrForeignHandle = errors.New("sqlstats: generation handle belongs to another collector")
	// ErrHandleReleased reports a handle whose data was already freed.
	ErrHandleReleased = errors.New("sqlstats: generation handle was released")
	// ErrNotDrained reports a generation whose rotation has not settled, so no
	// fixed value exists to collect yet.
	ErrNotDrained = errors.New("sqlstats: generation was not drained")
)

// boundaryKey identifies one boundary operation of one run. Every operation is
// idempotent per (runID, epoch), so the phase has to be part of the key: a run
// takes both an opening and a closing boundary under the same epoch.
type boundaryKey struct {
	runID string
	epoch runctl.Epoch
	phase runctl.Phase
}

// boundaryRecord is a replayable boundary outcome. The error is stored with
// the result so a retried call returns byte-for-byte what the first one did,
// rather than a second, differently-timed verdict.
type boundaryRecord struct {
	result runctl.BoundaryResult
	err    error
}

// GenerationCollector adapts the SQL store's generation machinery to
// runctl.GenerationCollector.
//
// The store's own Rotate publishes the new generation and then waits for every
// query that started in the old one, which would put an in-flight query on the
// boundary path. This adapter splits the two: the boundary performs only the
// pointer swap, and the wait moves into Drain, where the contract allows a
// context to cancel it.
//
// Nothing here waits for a query outside a caller's context. A query that
// never returns leaves its own generation unsettled and nothing else: the swap
// already happened, later boundaries are unaffected, and cancelling a Drain
// leaves no goroutine behind, because there is no goroutine — the wait belongs
// to the caller that asked for it.
type GenerationCollector struct {
	store *Store

	mu      sync.Mutex
	epoch   runctl.Epoch
	results map[boundaryKey]boundaryRecord
}

var _ runctl.GenerationCollector = (*GenerationCollector)(nil)

// NewGenerationCollector wraps a store. A nil store means the package default,
// which is where every proxied driver reports.
func NewGenerationCollector(store *Store) *GenerationCollector {
	if store == nil {
		store = Default
	}
	return &GenerationCollector{store: store, results: map[boundaryKey]boundaryRecord{}}
}

// SetEventObserver forwards event observation to the underlying store.
func (c *GenerationCollector) SetEventObserver(observer EventObserver) {
	if c != nil && c.store != nil {
		c.store.SetEventObserver(observer)
	}
}

// Name identifies the snapshot section this collector fills.
func (c *GenerationCollector) Name() string { return SectionName }

// BeginBoundary swaps in a fresh store generation and returns a handle to the
// one it just closed. It moves a pointer, so it does not block on in-flight
// queries.
func (c *GenerationCollector) BeginBoundary(ctx context.Context, runID string, ep runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, ep, runctl.PhaseStartBoundary)
}

// Freeze seals the running generation and returns its handle. Queries that
// start after it belong to the next generation, outside the run.
func (c *GenerationCollector) Freeze(ctx context.Context, runID string, ep runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, ep, runctl.PhaseFinishFreeze)
}

// boundary is the shared implementation of both boundaries: they differ only
// in which generation the returned handle happens to name.
func (c *GenerationCollector) boundary(ctx context.Context, runID string, ep runctl.Epoch, phase runctl.Phase) (runctl.BoundaryResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ep < c.epoch {
		return runctl.BoundaryResult{At: time.Now()}, fmt.Errorf("%w: %s is at epoch %d, got %d",
			runctl.ErrStaleEpoch, SectionName, c.epoch, ep)
	}
	key := boundaryKey{runID: runID, epoch: ep, phase: phase}
	if prev, ok := c.results[key]; ok {
		return prev.result, prev.err
	}
	if err := contextErr(ctx); err != nil {
		// Refuse before touching any state: a boundary that cannot report its
		// own result must not move the store's generation, or the run would
		// lose a window nobody holds a handle to.
		return runctl.BoundaryResult{At: time.Now()},
			fmt.Errorf("sqlstats: boundary for run %s: %w", runID, err)
	}
	if ep > c.epoch {
		// Only the current epoch is replayable; anything older is answered
		// with ErrStaleEpoch, so keeping its records would just grow the map
		// for the lifetime of the process.
		c.epoch = ep
		c.results = make(map[boundaryKey]boundaryRecord, 2)
	}

	// The swap is a pointer move under the store's mutex: it cannot block on
	// an in-flight query, so the boundary always fits its budget and always
	// commits. Waiting for the queries pinned to the sealed generation is
	// Drain's job, where a context can call it off.
	g := &sqlGeneration{owner: c, sealed: c.store.manager.Swap()}
	g.gen = uint64(g.sealed.Generation())

	record := boundaryRecord{result: runctl.BoundaryResult{
		Handle:    runctl.NewGenerationHandle(runID, ep, SectionName, g.gen, g),
		At:        time.Now(),
		Committed: true,
	}}
	c.results[key] = record
	return record.result, record.err
}

// contextErr tolerates a nil context: measurement must not panic on a caller's
// mistake.
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// Drain waits for the queries pinned to the handle's generation, then freezes
// it. The wait is the caller's own: it ends when every query has finished or
// when ctx says to stop, whichever comes first, and abandoning it leaves no
// goroutine that will later touch any generation.
func (c *GenerationCollector) Drain(ctx context.Context, h runctl.GenerationHandle) error {
	g, err := c.generationFor(h)
	if err != nil {
		return err
	}
	if err := g.sealed.Wait(ctx); err != nil {
		return fmt.Errorf("sqlstats: generation %d did not settle: %w", g.gen, err)
	}
	g.freeze()
	return nil
}

// Collect returns the frozen generation. It reads only what the rotation fixed
// and never touches the store's current table.
func (c *GenerationCollector) Collect(h runctl.GenerationHandle) (any, error) {
	g, err := c.generationFor(h)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return nil, fmt.Errorf("%w: generation %d", ErrHandleReleased, g.gen)
	}
	if !g.settled {
		return nil, fmt.Errorf("%w: generation %d", ErrNotDrained, g.gen)
	}
	return Frozen{
		Generation: g.frozen.Generation,
		Entries:    append([]agg.Entry(nil), g.frozen.Entries...),
		CutShort:   g.frozen.CutShort,
	}, nil
}

// Release drops the frozen table. It is idempotent, and a handle this
// collector never minted is ignored rather than reported: Release has no error
// channel and must not panic into the caller.
func (c *GenerationCollector) Release(h runctl.GenerationHandle) {
	g, err := c.generationFor(h)
	if err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return
	}
	g.released = true
	g.frozen = Frozen{}
}

// generationFor resolves a handle to the generation it names, rejecting
// handles from other collectors instead of type-asserting blindly.
func (c *GenerationCollector) generationFor(h runctl.GenerationHandle) (*sqlGeneration, error) {
	g, ok := h.Token().(*sqlGeneration)
	if !ok || g == nil || g.owner != c {
		return nil, fmt.Errorf("%w: collector %q generation %d", ErrForeignHandle, h.Collector, h.Gen)
	}
	return g, nil
}

// sqlGeneration is the closed generation a handle points at.
type sqlGeneration struct {
	owner *GenerationCollector
	gen   uint64
	// sealed is the store generation this handle names. It accepts no new
	// queries, and its completion is a channel rather than a condition
	// variable, which is what makes Drain's wait cancellable.
	sealed generation.Sealed[*agg.Table, []agg.Entry]

	mu       sync.Mutex
	frozen   Frozen
	settled  bool
	released bool
}

// freeze fixes the drained generation's value. It runs once: a second Drain,
// or a Drain that races another, must not re-read a table that a late query
// may since have written to, and a released handle must stay released rather
// than resurrect the data it just dropped.
//
// CutShort is carried through rather than hard-coded false. On this path it is
// always false today, because Drain only calls freeze after a Wait that
// returned nil; carrying it means a future caller that freezes on a give-up
// cannot accidentally publish the result as complete.
func (g *sqlGeneration) freeze() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.settled || g.released {
		return
	}
	frozen := g.sealed.Freeze()
	g.frozen = Frozen{Generation: frozen.Generation, Entries: frozen.Value, CutShort: frozen.CutShort}
	g.settled = true
}
