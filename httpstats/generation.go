package httpstats

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// CollectorName is the snapshot section this collector fills.
const CollectorName = "httpstats"

// ResetDrainBudget bounds how long the Reset compatibility shim waits for the
// requests pinned to the generation it closes.
//
// It is runctl.DrainBudget because Reset is exactly the drain step of a run
// boundary expressed through the pre-generation API, and the two must not be
// able to disagree about how long a wedged request is worth waiting for.
const ResetDrainBudget = runctl.DrainBudget

var (
	// errForeignHandle rejects a handle minted by another collector, or a zero
	// handle. Measurement must never panic into the application, so a wrong
	// handle is an error rather than a failed type assertion.
	errForeignHandle = errors.New("httpstats: handle does not belong to this collector")
	// errReleasedHandle rejects Collect on a generation whose memory was
	// already freed. Returning empty data instead would look like a run that
	// served no traffic.
	errReleasedHandle = errors.New("httpstats: handle was already released")
)

// Collector implements the generation half of the reset coordinator contract.
var _ runctl.GenerationCollector = (*Collector)(nil)

// Result is the fixed data of one drained generation: the latency table plus
// the long-lived connection totals accumulated in the same window.
type Result struct {
	HTTP        Snapshot     `json:"http"`
	Connections ConnSnapshot `json:"connections"`
}

// generation is one measurement window. Requests are pinned to the generation
// that was current when they started, so a boundary never splits a single
// request across two windows.
type generation struct {
	owner *Collector
	gen   uint64
	table *table

	// inFlight, sealed, released and conn are guarded by owner.mu.
	inFlight int
	sealed   bool
	released bool
	conn     ConnSnapshot

	// done is closed exactly once, when the generation is both sealed and
	// empty; Drain waits on it. A sync.Cond cannot be interrupted by a
	// context, which is why the wait is a channel: a request that never
	// returns must not be able to wedge a caller that has a deadline.
	done      chan struct{}
	closeOnce sync.Once
	freeOnce  sync.Once
}

// sealLocked marks g as no longer current. From this moment inFlight can only
// fall, because begin pins new requests to c.current. Callers hold owner.mu.
func (g *generation) sealLocked() {
	g.sealed = true
	g.settleLocked()
}

// settleLocked closes the completion channel when, and only when, the
// generation is both sealed and empty. The condition lives here rather than at
// the call sites so that no caller can close a live generation: a live one
// drops to zero in-flight between requests all the time, and closing it there
// would panic on the second close or — once guarded — make a later Drain
// return while requests are still writing to the generation it claims to have
// settled. Callers hold owner.mu.
func (g *generation) settleLocked() {
	if !g.sealed || g.inFlight != 0 {
		return
	}
	g.closeOnce.Do(func() { close(g.done) })
}

// boundaryMemo remembers the result of the last boundary of one kind, which is
// what makes a retry of the same (runID, epoch) return an identical result
// instead of swapping a second time.
type boundaryMemo struct {
	runID  string
	epoch  runctl.Epoch
	result runctl.BoundaryResult
}

// generationState is the collector's boundary bookkeeping, guarded by mu.
type generationState struct {
	next   uint64
	epoch  runctl.Epoch
	begun  *boundaryMemo
	frozen *boundaryMemo

	// resetBudget overrides ResetDrainBudget for Reset. Zero means the default,
	// so a Collector built by New is already bounded.
	resetBudget time.Duration
	// resetsCutShort counts the Reset calls that returned on their budget
	// instead of on a settled generation. Reset cannot report that in its
	// return value without breaking every existing caller, so it is recorded
	// here and read back through ResetsCutShort.
	resetsCutShort int64
}

// newGeneration builds the next generation. Callers hold mu, except New which
// runs before the collector is reachable.
func (c *Collector) newGeneration() *generation {
	g := &generation{
		owner: c,
		gen:   c.gens.next,
		table: newTable(c.maxKeys),
		done:  make(chan struct{}),
	}
	c.gens.next++
	return g
}

// swapLocked installs a fresh generation and returns the previous one, sealed
// and carrying the connection totals of its own window. Callers hold mu.
func (c *Collector) swapLocked() *generation {
	old := c.current
	c.current = c.newGeneration()
	old.conn = c.takeConn()
	old.sealLocked()
	return old
}

// Name identifies the snapshot section this collector fills.
func (c *Collector) Name() string { return CollectorName }

// BeginBoundary swaps in a fresh generation and returns a handle to the one it
// just closed. It only moves a pointer, so it does not block on in-flight
// requests.
func (c *Collector) BeginBoundary(ctx context.Context, runID string, ep runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, ep, &c.gens.begun)
}

// Freeze seals the current generation and returns its handle. Requests that
// start after Freeze are pinned to the next generation and stay outside the
// run.
func (c *Collector) Freeze(ctx context.Context, runID string, ep runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, ep, &c.gens.frozen)
}

// boundary performs one generation swap, memoized per (runID, epoch) so that a
// retry is a no-op that returns the first result, including its timestamp.
func (c *Collector) boundary(ctx context.Context, runID string, ep runctl.Epoch, memo **boundaryMemo) (runctl.BoundaryResult, error) {
	c.mu.Lock()
	if m := *memo; m != nil && m.runID == runID && m.epoch == ep {
		result := m.result
		c.mu.Unlock()
		return result, nil
	}
	if ep < c.gens.epoch {
		current := c.gens.epoch
		c.mu.Unlock()
		return runctl.BoundaryResult{At: time.Now()},
			fmt.Errorf("httpstats: epoch %d is behind %d: %w", ep, current, runctl.ErrStaleEpoch)
	}
	if err := contextErr(ctx); err != nil {
		c.mu.Unlock()
		return runctl.BoundaryResult{At: time.Now()}, fmt.Errorf("httpstats: boundary for run %s: %w", runID, err)
	}
	old := c.swapLocked()
	at := time.Now()
	c.gens.epoch = ep
	result := runctl.BoundaryResult{
		Handle:    runctl.NewGenerationHandle(runID, ep, CollectorName, old.gen, old),
		At:        at,
		Committed: true,
	}
	*memo = &boundaryMemo{runID: runID, epoch: ep, result: result}
	c.mu.Unlock()
	return result, nil
}

// Drain waits for the requests pinned to the handle's generation to finish. It
// returns as soon as ctx is done, leaving no goroutine that touches any other
// generation: a late request writes to its own sealed table and nowhere else.
func (c *Collector) Drain(ctx context.Context, h runctl.GenerationHandle) error {
	g, err := c.generationOf(h)
	if err != nil {
		return err
	}
	select {
	case <-g.done:
		return nil
	default:
	}
	if ctx == nil {
		<-g.done
		return nil
	}
	select {
	case <-g.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Collect reads the fixed data of a drained generation. It never reads the
// collector's current generation.
func (c *Collector) Collect(h runctl.GenerationHandle) (any, error) {
	g, err := c.generationOf(h)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	released, conn := g.released, g.conn
	c.mu.Unlock()
	if released {
		return nil, fmt.Errorf("%w: gen %d", errReleasedHandle, g.gen)
	}
	return Result{HTTP: g.table.snapshot(), Connections: conn}, nil
}

// Release frees the table the handle pins. It is idempotent, and a request
// that arrives late from a drain timeout still finds a usable (empty) table
// rather than a nil map, because measurement may not panic into the
// application.
func (c *Collector) Release(h runctl.GenerationHandle) {
	g, err := c.generationOf(h)
	if err != nil {
		return
	}
	g.freeOnce.Do(func() {
		c.mu.Lock()
		g.released = true
		c.mu.Unlock()
		g.table.clear()
	})
}

// generationOf recovers the collector-internal generation a handle points at,
// rejecting zero handles and handles minted by another collector.
func (c *Collector) generationOf(h runctl.GenerationHandle) (*generation, error) {
	g, ok := h.Token().(*generation)
	if !ok || g == nil || g.owner != c {
		return nil, fmt.Errorf("%w: collector %q gen %d", errForeignHandle, h.Collector, h.Gen)
	}
	return g, nil
}

// Reset atomically starts a new generation, waits for requests that started in
// the old one to finish, and returns the completed old generation. It is a
// compatibility shim over the generation mechanism: the same per-generation
// done channel Drain uses.
//
// The wait is bounded by ResetDrainBudget, or by whatever SetResetDrainBudget
// installed. An unbounded wait here would reintroduce on the /reset path
// exactly what the done channel exists to prevent: /reset calls Reset while it
// holds the process-wide reset lock and the operation slot, so one request
// that never returns — a hijacked connection nobody confirmed, a handler
// parked on a wedged dependency — would wedge /reset and, through the same
// lock, /finish, /collect and /save, for the life of the process.
//
// When the budget expires the generation's table is returned as it stands: a
// usable, partial snapshot, because a late request writes only to its own
// sealed table and never to the live one. The cut is counted so the caller can
// read it back through ResetsCutShort and mark its section partial.
func (c *Collector) Reset() Snapshot {
	c.resetMu.Lock()
	defer c.resetMu.Unlock()
	c.mu.Lock()
	old := c.swapLocked()
	budget := c.gens.resetBudget
	c.mu.Unlock()
	if budget <= 0 {
		budget = ResetDrainBudget
	}
	// The ready case is tried first. A select over an already-settled
	// generation and an already-expired timer picks between them at random,
	// which would report a wait that never had to happen as a cut-short one.
	select {
	case <-old.done:
		return old.table.snapshot()
	default:
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-old.done:
	case <-timer.C:
		c.mu.Lock()
		c.gens.resetsCutShort++
		c.mu.Unlock()
	}
	return old.table.snapshot()
}

// SetResetDrainBudget bounds the wait performed by Reset. A non-positive value
// restores ResetDrainBudget. It exists for callers whose own operation budget
// is tighter than the run controller's; the default is already bounded, so
// leaving it alone is safe.
func (c *Collector) SetResetDrainBudget(budget time.Duration) {
	c.mu.Lock()
	c.gens.resetBudget = budget
	c.mu.Unlock()
}

// ResetsCutShort reports how many Reset calls returned on their budget with
// requests still pinned to the generation they closed. A non-zero value means
// at least one returned snapshot was missing requests that were still running,
// which is a partial section rather than a failed one.
func (c *Collector) ResetsCutShort() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gens.resetsCutShort
}

// begin pins a request to the current generation.
func (c *Collector) begin() *generation {
	c.mu.Lock()
	g := c.current
	g.inFlight++
	c.mu.Unlock()
	return g
}

// release ends an in-flight request without recording a latency row.
func (c *Collector) release(g *generation) {
	c.mu.Lock()
	g.inFlight--
	g.settleLocked()
	c.mu.Unlock()
}

// finish records a completed request and ends its in-flight status. Recording
// happens before the generation can settle, so a Drain that returns has seen
// every row of the generation it drained.
func (c *Collector) finish(g *generation, id identity, duration time.Duration, responseBytes int64) {
	g.table.observe(id, duration, responseBytes)
	c.mu.Lock()
	g.inFlight--
	g.settleLocked()
	c.mu.Unlock()
}

// contextErr tolerates a nil context: a measurement call must not panic on a
// caller's mistake.
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
