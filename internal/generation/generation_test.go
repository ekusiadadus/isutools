package generation

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) add(n int) {
	c.mu.Lock()
	c.n += n
	c.mu.Unlock()
}

func (c *counter) snapshot() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func newCounterManager() *Manager[*counter, int] {
	return New(
		func() *counter { return &counter{} },
		func(c *counter) int { return c.snapshot() },
	)
}

func TestStartsAtGenerationOne(t *testing.T) {
	m := newCounterManager()
	got := m.Snapshot()
	if got.Generation != 1 || got.Value != 0 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSwapWaitsForOldLeaseAndNewWorkUsesNewGeneration(t *testing.T) {
	m := newCounterManager()
	old := m.Acquire()
	old.Value().add(2)

	result := make(chan Frozen[int], 1)
	go func() { result <- m.SwapAndSnapshot() }()

	deadline := time.Now().Add(time.Second)
	for m.CurrentGeneration() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.CurrentGeneration() != 2 {
		t.Fatal("reset did not publish generation 2")
	}

	current := m.Acquire()
	if current.Generation() != 2 {
		t.Fatalf("new lease generation = %d, want 2", current.Generation())
	}
	current.Value().add(7)
	current.Done()

	select {
	case <-result:
		t.Fatal("swap returned before old in-flight measurement completed")
	default:
	}

	old.Value().add(3)
	old.Done()
	select {
	case frozen := <-result:
		if frozen.Generation != 1 || frozen.Value != 5 {
			t.Fatalf("frozen = %#v, want generation 1 value 5", frozen)
		}
	case <-time.After(time.Second):
		t.Fatal("swap did not finish after old lease completed")
	}

	got := m.Snapshot()
	if got.Generation != 2 || got.Value != 7 {
		t.Fatalf("current = %#v, want generation 2 value 7", got)
	}
}

func TestLeaseDoneIsIdempotent(t *testing.T) {
	m := newCounterManager()
	lease := m.Acquire()
	lease.Value().add(1)
	lease.Done()
	lease.Done()
	frozen := m.SwapAndSnapshot()
	if frozen.Value != 1 {
		t.Fatalf("frozen value = %d, want 1", frozen.Value)
	}
}

func TestConcurrentSwapsAreSerialized(t *testing.T) {
	m := newCounterManager()
	old := m.Acquire()

	results := make(chan Frozen[int], 2)
	go func() { results <- m.SwapAndSnapshot() }()
	deadline := time.Now().Add(time.Second)
	for m.CurrentGeneration() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	go func() { results <- m.SwapAndSnapshot() }()

	time.Sleep(10 * time.Millisecond)
	if got := m.CurrentGeneration(); got != 2 {
		t.Fatalf("second swap overtook first: current generation = %d", got)
	}
	old.Done()

	a := <-results
	b := <-results
	if a.Generation != 1 || b.Generation != 2 {
		t.Fatalf("swap results = %#v then %#v", a, b)
	}
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

func TestSwapDoesNotBlockAndSealedWaitHonoursContext(t *testing.T) {
	m := newCounterManager()
	lease := m.Acquire()
	lease.Value().add(4)

	// The swap is a pointer move; it must not wait for the lease.
	swapped := make(chan Sealed[*counter, int], 1)
	go func() { swapped <- m.Swap() }()
	var sealed Sealed[*counter, int]
	select {
	case sealed = <-swapped:
	case <-time.After(2 * time.Second):
		t.Fatal("Swap blocked on in-flight work")
	}
	if sealed.Generation() != 1 {
		t.Fatalf("sealed generation = %d, want 1", sealed.Generation())
	}
	if got := m.CurrentGeneration(); got != 2 {
		t.Fatalf("current generation after Swap = %d, want 2", got)
	}
	if sealed.Settled() {
		t.Fatal("a sealed generation with a live lease must not report settled")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := sealed.Wait(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait on a cancelled context = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Wait took %v to honour a cancelled context", elapsed)
	}

	// A nil context is a caller mistake measurement must survive rather than
	// panic on: it waits for the work itself.
	nilWait := make(chan error, 1)
	go func() { nilWait <- sealed.Wait(nil) }()
	select {
	case err := <-nilWait:
		t.Fatalf("Wait(nil) returned %v while a lease was still live", err)
	case <-time.After(20 * time.Millisecond):
	}

	// Work that finishes after the abandoned wait still lands in its own
	// generation, and the same Sealed can be waited on again.
	lease.Value().add(3)
	lease.Done()
	select {
	case err := <-nilWait:
		if err != nil {
			t.Fatalf("Wait(nil): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait(nil) did not return after the lease finished")
	}
	if err := sealed.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after the lease finished: %v", err)
	}
	if !sealed.Settled() {
		t.Fatal("a sealed generation with no leases must report settled")
	}
	if frozen := sealed.Freeze(); frozen.Generation != 1 || frozen.Value != 7 {
		t.Fatalf("frozen = %#v, want generation 1 value 7", frozen)
	}
}

// TestSwapAndSnapshotGivesUpOnWorkThatNeverFinishes is the regression test for
// the uninterruptible wait: with a sync.Cond the shim parked one goroutine per
// swap — permanently, and while holding the serialization lock, so every later
// swap parked behind it too.
func TestSwapAndSnapshotGivesUpOnWorkThatNeverFinishes(t *testing.T) {
	const swaps = 8

	m := newCounterManager()
	// Give up quickly so the give-up path is testable without waiting out the
	// production bound.
	m.SetCompatWait(20 * time.Millisecond)

	// A lease that is never released models a query that never returns.
	stuck := m.Acquire()
	t.Cleanup(stuck.Done)
	stuck.Value().add(9)

	baseline := runtime.NumGoroutine()

	for i := 0; i < swaps; i++ {
		result := make(chan Frozen[int], 1)
		go func() { result <- m.SwapAndSnapshot() }()
		select {
		case frozen := <-result:
			if want := int64(i + 1); frozen.Generation != want {
				t.Fatalf("swap %d froze generation %d, want %d", i, frozen.Generation, want)
			}
			// The first swap freezes the stuck generation best-effort rather
			// than blocking on it; later ones freeze empty generations.
			if want := 0; i > 0 && frozen.Value != want {
				t.Fatalf("swap %d froze value %d, want %d", i, frozen.Value, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("swap %d never returned: work that never finishes parked it", i)
		}
	}

	if got, want := m.CurrentGeneration(), int64(swaps+1); got != want {
		t.Fatalf("current generation = %d, want %d", got, want)
	}
	// Every swap goroutine must have exited: none of them may still be parked
	// on the generation the stuck lease pins.
	if got := goroutinesBelow(baseline, 2*time.Second); got > baseline {
		t.Fatalf("goroutines = %d, want at most the %d present before %d swaps", got, baseline, swaps)
	}
}

// TestDefaultCompatWaitIsTheSharedDrainBudget pins the bound to the one
// authority for it. The shim used to carry an independently invented 30s
// constant — three times the run controller's drain budget — so "how long is a
// wedged query worth waiting for?" had two different answers depending on which
// entry point closed the generation. Asserting the identity, not a duration,
// is what makes a future divergence fail here instead of in production.
func TestDefaultCompatWaitIsTheSharedDrainBudget(t *testing.T) {
	if DefaultCompatWait != runctl.DrainBudget {
		t.Fatalf("DefaultCompatWait = %v, want runctl.DrainBudget (%v): the bound must come from runctl, not from a local constant",
			DefaultCompatWait, runctl.DrainBudget)
	}
}

// TestSwapAndSnapshotReportsCutShortWhenWorkNeverFinishes is the regression
// test for the silent truncation: the shim discarded its wait outcome, so a
// generation frozen with work still in flight was indistinguishable from a
// complete one and got published as whole data.
func TestSwapAndSnapshotReportsCutShortWhenWorkNeverFinishes(t *testing.T) {
	m := newCounterManager()
	m.SetCompatWait(20 * time.Millisecond)

	// A lease that is never released models a query that never returns.
	stuck := m.Acquire()
	t.Cleanup(stuck.Done)
	stuck.Value().add(9)

	frozen := m.SwapAndSnapshot()
	if !frozen.CutShort {
		t.Fatalf("frozen = %#v, want CutShort=true: work was still in flight when the wait gave up", frozen)
	}
	if frozen.Generation != 1 || frozen.Value != 9 {
		t.Fatalf("frozen = %#v, want generation 1 value 9", frozen)
	}
}

// TestSwapAndSnapshotDoesNotReportCutShortForSettledWork is the other half of
// the contract: a healthy drain must never be labelled partial, or the flag
// means nothing. It covers both a generation that was already empty at the swap
// and one whose lease lands during the wait.
func TestSwapAndSnapshotDoesNotReportCutShortForSettledWork(t *testing.T) {
	m := newCounterManager()

	// Already settled at the swap: nothing to wait for at all.
	done := m.Acquire()
	done.Value().add(2)
	done.Done()
	first := m.SwapAndSnapshot()
	if first.CutShort {
		t.Fatalf("frozen = %#v, want CutShort=false for a generation with no live leases", first)
	}
	if first.Generation != 1 || first.Value != 2 {
		t.Fatalf("frozen = %#v, want generation 1 value 2", first)
	}

	// Settles during the wait: the wait succeeded, so the data is whole.
	slow := m.Acquire()
	release := make(chan struct{})
	go func() {
		<-release
		slow.Value().add(5)
		slow.Done()
	}()
	result := make(chan Frozen[int], 1)
	go func() { result <- m.SwapAndSnapshot() }()
	close(release)
	select {
	case second := <-result:
		if second.CutShort {
			t.Fatalf("frozen = %#v, want CutShort=false: the lease finished inside the bound", second)
		}
		if second.Generation != 2 || second.Value != 5 {
			t.Fatalf("frozen = %#v, want generation 2 value 5", second)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("swap did not return after the lease finished")
	}

	// A live-generation snapshot is not a truncated drain: nothing waited, so
	// nothing was cut short.
	if live := m.Snapshot(); live.CutShort {
		t.Fatalf("Snapshot = %#v, want CutShort=false", live)
	}
}

// TestSwapAndSnapshotContextHonoursTheCallersDeadline pins the third half of
// the defect: the shim always built its wait on context.Background(), so a
// caller whose request was already gone still paid the full bound while holding
// the reset lock and the operation slot.
func TestSwapAndSnapshotContextHonoursTheCallersDeadline(t *testing.T) {
	m := newCounterManager()
	// Deliberately left at the production bound: the caller's context, not the
	// manager's budget, is what has to end this wait.
	stuck := m.Acquire()
	t.Cleanup(stuck.Done)
	stuck.Value().add(4)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan Frozen[int], 1)
	start := time.Now()
	go func() { result <- m.SwapAndSnapshotContext(ctx) }()
	select {
	case frozen := <-result:
		if elapsed := time.Since(start); elapsed >= DefaultCompatWait {
			t.Fatalf("swap took %v, the caller's 20ms deadline was ignored", elapsed)
		}
		if !frozen.CutShort {
			t.Fatalf("frozen = %#v, want CutShort=true", frozen)
		}
		if frozen.Generation != 1 || frozen.Value != 4 {
			t.Fatalf("frozen = %#v, want generation 1 value 4", frozen)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SwapAndSnapshotContext ignored the caller's deadline: still waiting after 2s")
	}

	// An already-cancelled context still rotates: the swap is the point of the
	// call, and refusing it would lose a window nobody holds a handle to.
	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	frozen := m.SwapAndSnapshotContext(dead)
	if frozen.Generation != 2 {
		t.Fatalf("frozen = %#v, want generation 2", frozen)
	}
	if got := m.CurrentGeneration(); got != 3 {
		t.Fatalf("current generation = %d, want 3", got)
	}

	// A nil context is a caller mistake measurement must survive.
	m.SetCompatWait(20 * time.Millisecond)
	if frozen := m.SwapAndSnapshotContext(nil); frozen.Generation != 3 {
		t.Fatalf("frozen = %#v, want generation 3", frozen)
	}
}
