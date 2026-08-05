package runctl

import (
	"context"
	"errors"
	"testing"
	"time"
)

// clockSpendingBaseline charges the injected clock for the time its closing
// sample takes. It is how a test spends lease time inside the *synchronous*
// freeze phase without waiting for it in real time.
type clockSpendingBaseline struct {
	*fakeBaseline
	clock *fakeClock
	spend time.Duration
}

func (b clockSpendingBaseline) CaptureFinal(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	b.clock.advance(b.spend)
	return b.fakeBaseline.CaptureFinal(ctx, runID, ep)
}

// parkingDrainGeneration charges the clock for the background worker's drain
// and then parks inside it, so a sweep can happen at a moment when the worker
// is provably still working and still inside its own budget.
//
// Only the closing generation is parked. The opening boundary hands the
// previous generation to a drain worker of its own, and that drain must stay
// out of the way; Freeze therefore stamps the handle it hands out with a token
// only this collector recognises, which is how Drain tells the two apart.
type parkingDrainGeneration struct {
	*fakeGeneration
	clock   *fakeClock
	spend   time.Duration
	final   *int
	entered chan struct{}
	release chan struct{}
}

func newParkingDrainGeneration(name string, clock *fakeClock, spend time.Duration) parkingDrainGeneration {
	return parkingDrainGeneration{
		fakeGeneration: newFakeGeneration(name),
		clock:          clock,
		spend:          spend,
		final:          new(int),
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
}

func (g parkingDrainGeneration) Freeze(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error) {
	res, err := g.fakeGeneration.Freeze(ctx, runID, ep)
	res.Handle = NewGenerationHandle(runID, ep, g.Name(), res.Handle.Gen, g.final)
	return res, err
}

func (g parkingDrainGeneration) Drain(ctx context.Context, h GenerationHandle) error {
	if h.Token() != any(g.final) {
		return g.fakeGeneration.Drain(ctx, h)
	}
	g.clock.advance(g.spend)
	close(g.entered)
	select {
	case <-g.release:
		return g.fakeGeneration.Drain(ctx, h)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestFinishLeaseIsRearmedWhenTheWorkerTakesOver pins what the finish lease
// means. It is armed before the synchronous freeze, which is what bounds that
// freeze; if it were never re-armed the background worker would inherit
// whatever the freeze spent, and a worker comfortably inside Drain +
// SnapshotBuild + Enrich could be aborted as "finish-lease-expired" — a verdict
// about a worker that was never late, paid for by discarding a correct
// measurement.
//
// The budgets below are legal by Budgets.Validate: the freeze spends 800ms of a
// one second lease, the worker 400ms of its own. Only their sum exceeds a
// single lease.
func TestFinishLeaseIsRearmedWhenTheWorkerTakesOver(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	budgets := testBudgets()
	budgets.FinishSync = 900 * time.Millisecond
	budgets.PhaseFreeze = 300 * time.Millisecond
	budgets.PhaseFinal = 600 * time.Millisecond
	budgets.PerCollectorBaseline = 300 * time.Millisecond
	budgets.FinishLease = time.Second
	c, hr := newTestController(t, func(o *Options) {
		o.Now = clock.Now
		o.Budgets = budgets
	})

	gen := newParkingDrainGeneration("http", clock, 400*time.Millisecond)
	base := clockSpendingBaseline{
		fakeBaseline: newFakeBaseline("proc"),
		clock:        clock,
		spend:        800 * time.Millisecond,
	}
	if err := c.RegisterGeneration(Registration{Name: "http", Required: true}, gen); err != nil {
		t.Fatalf("RegisterGeneration: %v", err)
	}
	if err := c.RegisterBaseline(Registration{Name: "proc", Required: true}, base); err != nil {
		t.Fatalf("RegisterBaseline: %v", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// Sweep with the worker parked inside its drain, one and a fifth leases
	// after the finish was requested but only two fifths of one after the
	// worker took over.
	<-gen.entered
	c.Sweep()
	close(gen.release)

	status, err := c.Await(ctx, start.RunID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if status.State != StateFinished {
		t.Fatalf("state = %s (reason %q), want %s: a worker inside its budget must not be aborted for the freeze that preceded it",
			status.State, status.Reason, StateFinished)
	}
	if hr.has(HealthLeaseExpired) {
		t.Error("health blamed the worker for time the synchronous freeze spent")
	}
	snap, err := c.SnapshotOf(start.RunID)
	if err != nil {
		t.Fatalf("SnapshotOf: %v; a completed measurement must still be published", err)
	}
	if snap.Sections["http"] == nil || snap.Sections["proc"] == nil {
		t.Fatalf("snapshot sections = %#v, want both collectors", snap.Sections)
	}
	if got := c.PublishedSnapshots(); got != 1 {
		t.Fatalf("published snapshots = %d, want 1", got)
	}
}

// TestFinishLeaseStillBoundsTheSynchronousFreeze is the other half of the
// contract: re-arming the lease for the worker must not leave the synchronous
// freeze unbounded, or a collector wedged inside Freeze or CaptureFinal would
// hold the run forever.
//
// The freeze phases run on the FinishRun goroutine, before any background
// worker exists, so only the lease FinishRun arms *before* those phases can
// reclaim this run — the worker's re-arm has not happened and never will. The
// phase code hands the collector a context but cannot make it honour one, which
// is why the collector below deliberately ignores it: that is the wedge the
// pre-freeze lease exists for. Without that arming slot.lease stays zero and
// Sweep's non-zero-lease guard skips the slot forever, so the run is never
// reclaimed and no further run can ever start.
func TestFinishLeaseStillBoundsTheSynchronousFreeze(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()

	// A short finish lease with the synchronous budgets kept underneath it, so
	// the table still validates. The clock is injected, so the lease costs the
	// test no real time; the numbers only have to stay in hierarchy.
	budgets := testBudgets()
	budgets.FinishLease = time.Second
	budgets.FinishSync = 800 * time.Millisecond
	budgets.PhaseFreeze = 200 * time.Millisecond
	budgets.PhaseFinal = 600 * time.Millisecond
	budgets.PerCollectorGeneration = 100 * time.Millisecond
	budgets.PerCollectorBaseline = 500 * time.Millisecond
	budgets.AbortJoin = 30 * time.Millisecond

	c, hr := newTestController(t, func(o *Options) {
		o.Now = clock.Now
		o.Budgets = budgets
	})
	wedged := newBlockingBaseline("proc", true) // ignores the context it is given
	if err := c.RegisterBaseline(Registration{Name: "proc", Required: true}, wedged); err != nil {
		t.Fatalf("RegisterBaseline: %v", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// FinishRun cannot return while the freeze is wedged, so it runs on a
	// goroutine of its own for the rest of the test.
	finished := make(chan error, 1)
	go func() {
		_, err := c.FinishRun(ctx, start.RunID)
		finished <- err
	}()

	select {
	case <-wedged.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("CaptureFinal was never entered")
	}

	// Pin the situation under test: the run is finishing, the collector is
	// inside the synchronous freeze, and no background worker has taken over —
	// so the lease observed here can only be the one FinishRun armed.
	c.mu.Lock()
	slot := c.lookupLocked(start.RunID)
	var (
		state          RunState
		lease          time.Time
		workerTookOver bool
	)
	if slot != nil {
		state, lease, workerTookOver = slot.state, slot.lease, slot.finishSet
	}
	c.mu.Unlock()
	if slot == nil {
		t.Fatalf("run %s is no longer retained while its freeze is still running", start.RunID)
	}
	if state != StateFinishing {
		t.Fatalf("state = %s, want %s while the freeze phases are running", state, StateFinishing)
	}
	if workerTookOver {
		t.Fatal("the background worker already published its acceptance, so this test is not exercising the synchronous freeze")
	}
	if lease.IsZero() {
		t.Fatal("the synchronous freeze is running under no lease at all: a wedged collector would hold the run forever and Sweep would skip it")
	}

	clock.advance(2 * budgets.FinishLease)
	c.Sweep()

	status, _ := c.Status(start.RunID)
	if status.State != StateAborted || status.Reason != ReasonFinishLeaseExpired {
		t.Fatalf("status = %#v, want aborted by %s: the watchdog must reclaim a run wedged in its freeze", status, ReasonFinishLeaseExpired)
	}
	if !hr.has(HealthLeaseExpired) {
		t.Fatalf("health key %s was not recorded", HealthLeaseExpired)
	}
	if !status.Detached {
		t.Fatal("the abort claimed a clean join while the collector was still inside CaptureFinal")
	}
	if _, err := c.StartRun(ctx, StartRunOptions{}); err != nil {
		t.Fatalf("the next run must start once the lease reclaimed the wedged one: %v", err)
	}

	close(wedged.release)
	select {
	case err := <-finished:
		if !errors.Is(err, ErrRunAborted) {
			t.Fatalf("FinishRun = %v, want ErrRunAborted after the lease reclaimed the run", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun never returned after the collector was released")
	}
}
