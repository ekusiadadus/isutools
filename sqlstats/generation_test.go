package sqlstats

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/runctl"
)

func newGenTestCollector(t *testing.T) (*GenerationCollector, *Store) {
	t.Helper()
	store := NewStore(agg.DefaultMaxKeys)
	return NewGenerationCollector(store), store
}

func drainAndCollect(t *testing.T, c *GenerationCollector, h runctl.GenerationHandle) Frozen {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Drain(ctx, h); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	value, err := c.Collect(h)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	frozen, ok := value.(Frozen)
	if !ok {
		t.Fatalf("Collect returned %T, want Frozen", value)
	}
	return frozen
}

func totalCount(entries []agg.Entry) int64 {
	var total int64
	for _, entry := range entries {
		total += entry.Count
	}
	return total
}

func TestGenerationCollectorBoundaryFreezeDrainCollect(t *testing.T) {
	c, store := newGenTestCollector(t)
	ctx := context.Background()

	store.Observe("SELECT 1", time.Millisecond) // before the run

	start, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if !start.Committed {
		t.Fatal("BeginBoundary must commit the swap")
	}
	if start.At.IsZero() {
		t.Fatal("BeginBoundary must report the measured swap time")
	}

	store.Observe("SELECT 2", time.Millisecond) // inside the run
	store.Observe("SELECT 2", time.Millisecond)

	final, err := c.Freeze(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !final.Committed {
		t.Fatal("Freeze must commit the swap")
	}

	store.Observe("SELECT 3", time.Millisecond) // after the run

	before := drainAndCollect(t, c, start.Handle)
	if got := totalCount(before.Entries); got != 1 {
		t.Fatalf("pre-run generation counted %d queries, want 1", got)
	}
	run := drainAndCollect(t, c, final.Handle)
	if got := totalCount(run.Entries); got != 2 {
		t.Fatalf("run generation counted %d queries, want 2", got)
	}
	if len(run.Entries) != 1 || run.Entries[0].Key != "SELECT 2" {
		t.Fatalf("run generation entries = %#v, want only SELECT 2", run.Entries)
	}
	if run.Generation <= before.Generation {
		t.Fatalf("generations must advance: %d then %d", before.Generation, run.Generation)
	}
	// The query issued after the freeze belongs to the generation that is
	// still open, not to the run that was just closed.
	if got := totalCount(store.Snapshot()); got != 1 {
		t.Fatalf("post-freeze generation counted %d queries, want 1", got)
	}
}

func TestGenerationCollectorDrainHonoursContextCancellation(t *testing.T) {
	c, store := newGenTestCollector(t)

	// A query that has started but not finished pins the generation, so the
	// rotation cannot freeze it. This is exactly the case Drain has to survive.
	pending := store.begin()
	t.Cleanup(func() { store.discard(pending) })

	res, err := c.BeginBoundary(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if !res.Committed {
		t.Fatal("the swap must commit even while a query is in flight")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err = c.Drain(ctx, res.Handle)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Drain must report the cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain error = %v, want a wrapped context.Canceled", err)
	}
	if elapsed > runctl.DrainCancelGrace {
		t.Fatalf("Drain took %v, want at most DrainCancelGrace (%v)", elapsed, runctl.DrainCancelGrace)
	}
	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrNotDrained) {
		t.Fatalf("Collect before the drain settled = %v, want ErrNotDrained", err)
	}

	// Once the in-flight query completes the same handle drains normally.
	store.discard(pending)
	done, cancelDone := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDone()
	if err := c.Drain(done, res.Handle); err != nil {
		t.Fatalf("Drain after the query finished: %v", err)
	}
	if _, err := c.Collect(res.Handle); err != nil {
		t.Fatalf("Collect after Drain: %v", err)
	}
}

func TestGenerationCollectorReplaysSameRunAndEpoch(t *testing.T) {
	c, store := newGenTestCollector(t)
	ctx := context.Background()

	first, err := c.BeginBoundary(ctx, "run-1", 7)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	generationAfterFirst := store.CurrentGeneration()

	second, err := c.BeginBoundary(ctx, "run-1", 7)
	if err != nil {
		t.Fatalf("replayed BeginBoundary: %v", err)
	}
	if !second.At.Equal(first.At) || second.Committed != first.Committed {
		t.Fatalf("replay returned %+v, want %+v", second, first)
	}
	if second.Handle != first.Handle {
		t.Fatal("replay must return the same handle")
	}
	if store.CurrentGeneration() != generationAfterFirst {
		t.Fatal("replay must not rotate a second time")
	}

	// The closing boundary of the same run is a different phase, so it is a
	// separate operation rather than a replay of the opening one.
	final, err := c.Freeze(ctx, "run-1", 7)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if final.Handle == first.Handle {
		t.Fatal("Freeze must close its own generation")
	}
	replayFinal, err := c.Freeze(ctx, "run-1", 7)
	if err != nil {
		t.Fatalf("replayed Freeze: %v", err)
	}
	if replayFinal.Handle != final.Handle || !replayFinal.At.Equal(final.At) {
		t.Fatalf("replayed Freeze returned %+v, want %+v", replayFinal, final)
	}

	drainAndCollect(t, c, first.Handle)
	drainAndCollect(t, c, final.Handle)
}

func TestGenerationCollectorRejectsStaleEpoch(t *testing.T) {
	c, _ := newGenTestCollector(t)
	ctx := context.Background()

	if _, err := c.BeginBoundary(ctx, "run-2", 5); err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	res, err := c.BeginBoundary(ctx, "run-1", 4)
	if !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("stale epoch error = %v, want ErrStaleEpoch", err)
	}
	if res.Committed {
		t.Fatal("a rejected boundary must not report a commit")
	}
	if _, err := c.Freeze(ctx, "run-1", 4); !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("stale epoch Freeze error = %v, want ErrStaleEpoch", err)
	}
}

func TestGenerationCollectorReleaseIsIdempotent(t *testing.T) {
	c, store := newGenTestCollector(t)
	ctx := context.Background()

	store.Observe("SELECT 1", time.Millisecond)
	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if err := c.Drain(ctx, res.Handle); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if _, err := c.Collect(res.Handle); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	c.Release(res.Handle)
	c.Release(res.Handle)
	c.Release(res.Handle)

	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrHandleReleased) {
		t.Fatalf("Collect after Release = %v, want ErrHandleReleased", err)
	}
	// Draining a released handle is still safe: the rotation it named has
	// already settled.
	if err := c.Drain(ctx, res.Handle); err != nil {
		t.Fatalf("Drain after Release: %v", err)
	}
}

func TestGenerationCollectorRejectsForeignHandle(t *testing.T) {
	c, _ := newGenTestCollector(t)
	other, _ := newGenTestCollector(t)
	ctx := context.Background()

	res, err := other.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	t.Cleanup(func() { other.Release(res.Handle) })

	if err := c.Drain(ctx, res.Handle); !errors.Is(err, ErrForeignHandle) {
		t.Fatalf("Drain of a foreign handle = %v, want ErrForeignHandle", err)
	}
	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrForeignHandle) {
		t.Fatalf("Collect of a foreign handle = %v, want ErrForeignHandle", err)
	}
	// Release has no error channel, so the only requirement is that a foreign
	// handle cannot make it panic or free another collector's data.
	c.Release(res.Handle)
	c.Release(runctl.GenerationHandle{})
	if err := other.Drain(ctx, res.Handle); err != nil {
		t.Fatalf("foreign Release broke the owner's drain: %v", err)
	}
	if _, err := other.Collect(res.Handle); err != nil {
		t.Fatalf("foreign Release freed another collector's generation: %v", err)
	}
}

func TestGenerationCollectorBoundaryRefusesDeadContext(t *testing.T) {
	c, store := newGenTestCollector(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := store.CurrentGeneration()
	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BeginBoundary on a cancelled context = %v, want a wrapped context.Canceled", err)
	}
	if res.Committed {
		t.Fatal("a refused boundary must not report a commit")
	}
	if got := store.CurrentGeneration(); got != before {
		t.Fatalf("a refused boundary swapped the store generation: %d -> %d", before, got)
	}

	// The refusal is not memoized: the same run and epoch still swaps once it
	// is given a context that is alive.
	res, err = c.BeginBoundary(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary after the refusal: %v", err)
	}
	if !res.Committed {
		t.Fatal("BeginBoundary after the refusal must commit the swap")
	}
	if got := store.CurrentGeneration(); got != before+1 {
		t.Fatalf("store generation = %d, want %d", got, before+1)
	}

	// A nil context is a caller mistake measurement has to survive: it may
	// degrade the measurement, never panic into the application.
	final, err := c.Freeze(nil, "run-1", 1) //nolint:staticcheck // nil ctx is the case under test
	if err != nil {
		t.Fatalf("Freeze with a nil context: %v", err)
	}
	if !final.Committed {
		t.Fatal("Freeze with a nil context must still commit the swap")
	}
	drainAndCollect(t, c, final.Handle)
}

// goroutinesBelow samples the goroutine count until it drops to limit or the
// deadline passes, then returns the last sample. It samples rather than reads
// once because a goroutine that has just been released still needs a moment to
// exit — a parked one never does, and the deadline is what tells them apart.
func goroutinesBelow(limit int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		n := runtime.NumGoroutine()
		if n <= limit || !time.Now().Before(deadline) {
			return n
		}
		time.Sleep(time.Millisecond)
	}
}

// TestGenerationCollectorHungQueryParksNoGoroutine is the regression test for
// the uninterruptible rotation wait.
//
// Rotating on a goroutine that waited for in-flight queries on a sync.Cond
// parked one goroutine per boundary — permanently, because a query that never
// returns never broadcasts, and while holding the store's rotation lock, so
// every later boundary parked behind it and its swap never happened either.
func TestGenerationCollectorHungQueryParksNoGoroutine(t *testing.T) {
	const boundaries = 8
	// Allow for goroutines the test binary starts for its own reasons; the
	// defect leaks one per boundary, which is far outside this slack.
	const slack = 2

	c, store := newGenTestCollector(t)
	baseline := runtime.NumGoroutine()

	for i := 0; i < boundaries; i++ {
		// Every generation gets a query that never completes, so no boundary
		// gets to settle by luck.
		hung := store.begin()
		t.Cleanup(func() { store.discard(hung) })

		before := store.CurrentGeneration()
		// A boundary is a pointer swap, so it must fit the per-collector
		// budget even though the generation it closes can never settle.
		ctx, cancel := context.WithTimeout(context.Background(), runctl.PerCollectorGenerationBudget)
		start := time.Now()
		res, err := c.BeginBoundary(ctx, "run-hung", runctl.Epoch(i+1))
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			t.Errorf("boundary %d: %v", i, err)
			continue
		}
		if !res.Committed {
			t.Errorf("boundary %d did not commit", i)
		}
		if elapsed > runctl.PerCollectorGenerationBudget {
			t.Errorf("boundary %d took %v, want at most %v", i, elapsed, runctl.PerCollectorGenerationBudget)
		}
		if after := store.CurrentGeneration(); after != before+1 {
			t.Errorf("boundary %d moved the store generation %d -> %d, want +1", i, before, after)
		}

		// The generation cannot settle, so the drain must end with its context
		// and not one moment later.
		drainCtx, cancelDrain := context.WithCancel(context.Background())
		cancelDrain()
		drainStart := time.Now()
		err = c.Drain(drainCtx, res.Handle)
		drainElapsed := time.Since(drainStart)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("boundary %d drain = %v, want a wrapped context.Canceled", i, err)
		}
		if drainElapsed > runctl.DrainCancelGrace {
			t.Errorf("boundary %d drain took %v, want at most %v", i, drainElapsed, runctl.DrainCancelGrace)
		}
		c.Release(res.Handle)
	}

	// Nothing may still be waiting on the queries that never returned.
	if got := goroutinesBelow(baseline+slack, 2*time.Second); got > baseline+slack {
		t.Errorf("goroutines = %d after %d boundaries with hung queries, want at most %d",
			got, boundaries, baseline+slack)
	}

	// The store is not wedged: a later boundary still swaps, drains and
	// collects the queries that did complete.
	store.Observe("SELECT after", time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	final, err := c.Freeze(ctx, "run-hung", runctl.Epoch(boundaries))
	if err != nil {
		t.Fatalf("boundary after the hung queries: %v", err)
	}
	frozen := drainAndCollect(t, c, final.Handle)
	if got := totalCount(frozen.Entries); got != 1 {
		t.Fatalf("generation after the hung queries counted %d queries, want 1", got)
	}
}

func TestGenerationCollectorNameMatchesSection(t *testing.T) {
	c, _ := newGenTestCollector(t)
	if c.Name() != SectionName {
		t.Fatalf("Name() = %q, want %q", c.Name(), SectionName)
	}
	if NewGenerationCollector(nil).store != Default {
		t.Fatal("a nil store must fall back to the package default")
	}
}

// TestGenerationCollectorDrainedGenerationIsNotCutShort pins the run-boundary
// path against the flag that the /reset path needs. Drain only freezes after a
// Wait that returned nil, so a generation collected through a run must never be
// labelled partial: if it ever is, either the freeze ran before the drain
// settled or the flag stopped meaning what it says.
func TestGenerationCollectorDrainedGenerationIsNotCutShort(t *testing.T) {
	c, store := newGenTestCollector(t)

	inflight := store.begin()
	store.Observe("SELECT counted", time.Millisecond)

	begun, err := c.BeginBoundary(context.Background(), "run-cutshort", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	// The query that started before the boundary finishes after it, in the
	// generation the handle names.
	store.finish(inflight, "SELECT late", time.Millisecond, false)

	frozen := drainAndCollect(t, c, begun.Handle)
	if frozen.CutShort {
		t.Fatalf("frozen = %#v, want CutShort=false: Drain settled before the freeze", frozen)
	}
	if got := totalCount(frozen.Entries); got != 2 {
		t.Fatalf("frozen entries = %#v, want both queries counted", frozen.Entries)
	}
}
