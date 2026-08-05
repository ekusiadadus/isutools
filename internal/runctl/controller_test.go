package runctl

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// startedController returns a controller with one required generation
// collector and one required baseline collector already registered.
func startedController(t *testing.T) (*Controller, *fakeGeneration, *fakeBaseline, *recordingHealth) {
	t.Helper()
	c, hr := newTestController(t, nil)
	g := newFakeGeneration("http")
	b := newFakeBaseline("proc")
	registerPair(t, c, g, Registration{Name: "http", Required: true}, b, Registration{Name: "proc", Required: true})
	return c, g, b, hr
}

func TestStartRunTakesBoundaryAndReportsWindows(t *testing.T) {
	ctx := context.Background()
	c, g, b, _ := startedController(t)

	start, err := c.StartRun(ctx, StartRunOptions{Reason: "api", Trigger: "test"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.State != StateStarted || start.Validity != ValidityValid {
		t.Fatalf("start = %s/%s, want started/valid", start.State, start.Validity)
	}
	if start.RunID == "" || start.Nonce == "" || start.Epoch == 0 {
		t.Fatalf("start identity incomplete: %#v", start)
	}
	if len(start.Collectors) != 2 {
		t.Fatalf("got %d collector records, want one per registered collector", len(start.Collectors))
	}
	if start.GenerationWindow.Spread > SpreadLimitGeneration {
		t.Fatalf("generation spread = %v, want within %v", start.GenerationWindow.Spread, SpreadLimitGeneration)
	}
	if start.BoundaryWindow.Min.IsZero() || start.BoundaryWindow.Max.Before(start.BoundaryWindow.Min) {
		t.Fatalf("boundary window = %#v", start.BoundaryWindow)
	}

	if begins, _, _, _, _ := g.counts(); begins != 1 {
		t.Fatalf("BeginBoundary called %d times, want 1", begins)
	}
	if captures, _, _ := b.counts(); captures != 1 {
		t.Fatalf("CaptureBaseline called %d times, want 1", captures)
	}

	status, ok := c.Status(start.RunID)
	if !ok || status.State != StateStarted {
		t.Fatalf("Status = %#v ok=%v, want started", status, ok)
	}
}

func TestStartRunWhileStartedReturnsErrRunActive(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	if _, err := c.StartRun(ctx, StartRunOptions{}); err != nil {
		t.Fatalf("first StartRun: %v", err)
	}
	if _, err := c.StartRun(ctx, StartRunOptions{}); !errors.Is(err, ErrRunActive) {
		t.Fatalf("second StartRun = %v, want ErrRunActive", err)
	}
}

func TestStartRunSameNonceReplaysIdenticalResult(t *testing.T) {
	ctx := context.Background()
	c, g, _, _ := startedController(t)

	first, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-1"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	second, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-1"})
	if err != nil {
		t.Fatalf("replayed StartRun: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replay differs:\n first = %#v\nsecond = %#v", first, second)
	}
	if begins, _, _, _, _ := g.counts(); begins != 1 {
		t.Fatalf("BeginBoundary called %d times, want exactly 1 for a replayed nonce", begins)
	}

	// A replayed result must not be mutable through the caller's copy.
	second.Collectors[0].Name = "tampered"
	third, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-1"})
	if err != nil {
		t.Fatalf("third StartRun: %v", err)
	}
	if third.Collectors[0].Name == "tampered" {
		t.Fatal("StartResult is not defensively copied: a caller mutated the stored record")
	}
}

func TestStartRunSameNonceWaitsForAnInFlightStart(t *testing.T) {
	ctx := context.Background()
	c, _, b, _ := startedController(t)
	b.captureDelay = 80 * time.Millisecond

	var (
		wg           sync.WaitGroup
		first, other StartResult
		firstErr     error
		otherErr     error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		first, firstErr = c.StartRun(ctx, StartRunOptions{Nonce: "shared"})
	}()
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond)
		other, otherErr = c.StartRun(ctx, StartRunOptions{Nonce: "shared"})
	}()
	wg.Wait()

	if firstErr != nil || otherErr != nil {
		t.Fatalf("StartRun errors: %v / %v", firstErr, otherErr)
	}
	if first.RunID != other.RunID {
		t.Fatalf("concurrent same-nonce starts produced %s and %s", first.RunID, other.RunID)
	}
	if captures, _, _ := b.counts(); captures != 1 {
		t.Fatalf("baseline captured %d times, want 1", captures)
	}
}

func TestFinishRunProducesSnapshotFromFrozenHandles(t *testing.T) {
	ctx := context.Background()
	c, g, b, _ := startedController(t)
	b.setLive(100)

	start, err := c.StartRun(ctx, StartRunOptions{Trigger: "bench"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	b.setLive(175)

	accepted, err := c.FinishRun(ctx, start.RunID)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if accepted.RunID != start.RunID || accepted.Validity != ValidityValid {
		t.Fatalf("accepted = %#v", accepted)
	}

	// Load applied after the freeze must not reach the snapshot.
	b.setLive(9999)

	status, err := c.Await(ctx, start.RunID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if status.State != StateFinished || status.Validity != ValidityValid {
		t.Fatalf("status = %#v, want finished/valid", status)
	}

	snap, err := c.SnapshotOf(start.RunID)
	if err != nil {
		t.Fatalf("SnapshotOf: %v", err)
	}
	if snap.Trigger != "bench" {
		t.Fatalf("snapshot trigger = %q, want bench", snap.Trigger)
	}
	if got := snap.Sections["proc"]; got != (fakeSample{Value: 75}) {
		t.Fatalf("proc section = %#v, want the frozen 175-100 delta", got)
	}
	if got := snap.Sections["http"]; got != "http-interval" {
		t.Fatalf("http section = %#v", got)
	}
	if _, _, drains, collects, releases := g.counts(); drains == 0 || collects != 1 || releases == 0 {
		t.Fatalf("generation lifecycle: drains=%d collects=%d releases=%d", drains, collects, releases)
	}
	if _, collects, releases := b.counts(); collects != 1 || releases == 0 {
		t.Fatalf("baseline lifecycle: collects=%d releases=%d", collects, releases)
	}
}

func TestFinishRunIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	first, err := c.FinishRun(ctx, start.RunID)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := c.Await(ctx, start.RunID); err != nil {
		t.Fatalf("Await: %v", err)
	}
	second, err := c.FinishRun(ctx, start.RunID)
	if err != nil {
		t.Fatalf("repeated FinishRun: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated FinishRun differs:\n%#v\n%#v", first, second)
	}
}

// TestFinishAcceptedIsImmutableWhileTheRunDegrades pins the split between the
// record a caller already holds and the run's final verdict: the worker may
// still downgrade validity, but a replayed FinishRun must return byte-for-byte
// what the first call returned.
func TestFinishAcceptedIsImmutableWhileTheRunDegrades(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, func(o *Options) { o.Budgets.Drain = 40 * time.Millisecond })
	g := newFakeGeneration("http")
	g.drainBlock = make(chan struct{})
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	first, err := c.FinishRun(ctx, start.RunID)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if first.Validity != ValidityValid {
		t.Fatalf("accepted validity = %s, want valid at freeze time", first.Validity)
	}

	status, err := c.Await(ctx, start.RunID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if status.Validity != ValidityPartial {
		t.Fatalf("run validity = %s, want partial after the drain timed out", status.Validity)
	}

	second, err := c.FinishRun(ctx, start.RunID)
	if err != nil {
		t.Fatalf("repeated FinishRun: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("the accepted record changed under the caller:\n%#v\n%#v", first, second)
	}
	close(g.drainBlock)
}

func TestAckTransitionsAndGuards(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	if err := c.Ack("nope"); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("Ack(unknown) = %v, want ErrUnknownRun", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := c.Ack(start.RunID); !errors.Is(err, ErrRunActive) {
		t.Fatalf("Ack(started) = %v, want ErrRunActive", err)
	}
	if err := c.Ack("other"); !errors.Is(err, ErrRunActive) {
		t.Fatalf("Ack(unknown while active) = %v, want ErrRunActive", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := c.Await(ctx, start.RunID); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if err := c.AckBy(start.RunID, AckedBySave); err != nil {
		t.Fatalf("AckBy: %v", err)
	}
	status, _ := c.Status(start.RunID)
	if status.State != StateAcknowledged || status.AckedBy != AckedBySave {
		t.Fatalf("status = %#v, want acknowledged by save", status)
	}
	if err := c.Ack(start.RunID); err != nil {
		t.Fatalf("repeated Ack must be a no-op, got %v", err)
	}
	if _, err := c.SnapshotOf(start.RunID); err != nil {
		t.Fatalf("an acknowledged snapshot must stay readable: %v", err)
	}
}

func TestAbortUnknownRunIsIdempotentSuccess(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	res, err := c.AbortRun(ctx, "never-existed", ReasonExplicit)
	if err != nil {
		t.Fatalf("aborting an unknown run must succeed, got %v", err)
	}
	if res.RunID != "never-existed" || res.Detached {
		t.Fatalf("result = %#v", res)
	}
}

func TestAbortRunReleasesHandlesAndAllowsRestart(t *testing.T) {
	ctx := context.Background()
	c, g, b, _ := startedController(t)

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	res, err := c.AbortRun(ctx, start.RunID, ReasonExplicit)
	if err != nil {
		t.Fatalf("AbortRun: %v", err)
	}
	if res.Detached {
		t.Fatalf("an idle run must join instantly, got %#v", res)
	}
	status, _ := c.Status(start.RunID)
	if status.State != StateAborted || status.Validity != ValidityInvalid {
		t.Fatalf("status = %#v, want aborted/invalid", status)
	}
	if _, err := c.SnapshotOf(start.RunID); !errors.Is(err, ErrRunAborted) {
		t.Fatalf("SnapshotOf(aborted) = %v, want ErrRunAborted", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); !errors.Is(err, ErrRunAborted) {
		t.Fatalf("FinishRun(aborted) = %v, want ErrRunAborted", err)
	}
	if _, err := c.StartRun(ctx, StartRunOptions{Nonce: start.Nonce}); !errors.Is(err, ErrRunAborted) {
		t.Fatalf("StartRun with an aborted nonce = %v, want ErrRunAborted", err)
	}

	next, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("a new run must be startable after an abort: %v", err)
	}
	if next.RunID == start.RunID {
		t.Fatal("the new run reused the aborted run ID")
	}
	if _, _, _, _, releases := g.counts(); releases == 0 {
		t.Fatal("generation handles were never released")
	}
	if _, _, releases := b.counts(); releases == 0 {
		t.Fatal("baseline handles were never released")
	}

	repeat, err := c.AbortRun(ctx, start.RunID, ReasonExplicit)
	if err != nil || !reflect.DeepEqual(repeat, res) {
		t.Fatalf("repeated abort = %#v, %v; want the stored result", repeat, err)
	}
}

func TestStartRunPreempt_AbortsActiveRunAndRecordsInvalid(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	victim, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	winner, err := c.StartRun(ctx, StartRunOptions{Preempt: true, Reason: "initialize"})
	if err != nil {
		t.Fatalf("preempting StartRun: %v", err)
	}
	if winner.PreemptedRunID != victim.RunID {
		t.Fatalf("PreemptedRunID = %q, want %q", winner.PreemptedRunID, victim.RunID)
	}
	if winner.State != StateStarted {
		t.Fatalf("winner state = %s, want started", winner.State)
	}

	status, ok := c.Status(victim.RunID)
	if !ok {
		t.Fatal("the preempted run must stay on record")
	}
	if status.State != StateAborted || status.Validity != ValidityInvalid {
		t.Fatalf("preempted run = %#v, want aborted/invalid", status)
	}
	if want := ReasonPreemptedBy + winner.RunID; status.Reason != want {
		t.Fatalf("reason = %q, want %q", status.Reason, want)
	}
	if _, err := c.SnapshotOf(victim.RunID); err == nil {
		t.Fatal("a preempted run must never expose a snapshot")
	}
}

func TestStartRunPreempt_OfFinishedRunKeepsSnapshot(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	first, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, first.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := c.Await(ctx, first.RunID); err != nil {
		t.Fatalf("Await: %v", err)
	}

	if _, err := c.StartRun(ctx, StartRunOptions{}); !errors.Is(err, ErrRunActive) {
		t.Fatalf("starting over an uncollected snapshot = %v, want ErrRunActive", err)
	}
	if _, err := c.StartRun(ctx, StartRunOptions{Preempt: true}); err != nil {
		t.Fatalf("preempting a finished run: %v", err)
	}

	status, _ := c.Status(first.RunID)
	if status.State != StateAcknowledged || status.AckedBy != AckedByPreempt {
		t.Fatalf("status = %#v, want acknowledged by preempt", status)
	}
	if _, err := c.SnapshotOf(first.RunID); err != nil {
		t.Fatalf("a finished snapshot must survive preemption: %v", err)
	}
}

func TestStartRunPreempt_UnderRace(t *testing.T) {
	ctx := context.Background()
	c, g, _, _ := startedController(t)
	g.drainBlock = make(chan struct{})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	const racers = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners []string
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.StartRun(ctx, StartRunOptions{Preempt: true})
			if err != nil {
				return
			}
			mu.Lock()
			winners = append(winners, res.RunID)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(g.drainBlock)

	if len(winners) == 0 {
		t.Fatal("no preempting start succeeded")
	}
	if _, err := c.SnapshotOf(start.RunID); err == nil {
		t.Fatal("the preempted finishing run published a snapshot")
	}
	// Only one run may be live no matter how many racers there were.
	live := 0
	c.mu.Lock()
	for _, s := range c.slots {
		if s.state.active() {
			live++
		}
	}
	c.mu.Unlock()
	if live > 1 {
		t.Fatalf("%d runs are active at once", live)
	}
}

func TestAwaitAndStatusForUnknownRuns(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	if _, ok := c.Status("nope"); ok {
		t.Fatal("Status reported an unknown run")
	}
	if _, err := c.Await(ctx, "nope"); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("Await(unknown) = %v, want ErrUnknownRun", err)
	}
	if _, err := c.FinishRun(ctx, "nope"); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("FinishRun(unknown) = %v, want ErrUnknownRun", err)
	}
	if _, err := c.SnapshotOf("nope"); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("SnapshotOf(unknown) = %v, want ErrUnknownRun", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, "other"); !errors.Is(err, ErrRunActive) {
		t.Fatalf("FinishRun(other) while active = %v, want ErrRunActive", err)
	}
	if _, err := c.SnapshotOf(start.RunID); !errors.Is(err, ErrRunActive) {
		t.Fatalf("SnapshotOf(in-flight) = %v, want ErrRunActive", err)
	}
	status, err := c.Await(ctx, start.RunID)
	if err != nil || status.State != StateStarted {
		t.Fatalf("Await(started) = %#v, %v", status, err)
	}
}

func TestAwaitRespectsCallerCancellation(t *testing.T) {
	c, g, _, _ := startedController(t)
	g.drainBlock = make(chan struct{})

	start, err := c.StartRun(context.Background(), StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(context.Background(), start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Await(ctx, start.RunID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await = %v, want the caller's deadline", err)
	}
	close(g.drainBlock)
}

func TestRetainedRunsEvictsTheOldestInactiveRecord(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	var ids []string
	for i := 0; i < RetainedRuns+1; i++ {
		start, err := c.StartRun(ctx, StartRunOptions{Preempt: true})
		if err != nil {
			t.Fatalf("StartRun %d: %v", i, err)
		}
		ids = append(ids, start.RunID)
	}

	if _, ok := c.Status(ids[0]); ok {
		t.Fatalf("run %s should have been evicted once %d records were retained", ids[0], RetainedRuns)
	}
	for _, id := range ids[1:] {
		if _, ok := c.Status(id); !ok {
			t.Fatalf("run %s should still be retained", id)
		}
	}
	// An evicted run is unknown, but aborting it must still succeed.
	if _, err := c.AbortRun(ctx, ids[0], ReasonExplicit); err != nil {
		t.Fatalf("aborting an evicted run: %v", err)
	}
	if _, err := c.FinishRun(ctx, ids[0]); err == nil {
		t.Fatal("finishing an evicted run must fail")
	}
}

func TestSweepExpiresSnapshotsAndTombstones(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	c, _ := newTestController(t, func(o *Options) { o.Now = clock.Now })
	g, b := newFakeGeneration("http"), newFakeBaseline("proc")
	registerPair(t, c, g, Registration{Name: "http", Required: true}, b, Registration{Name: "proc"})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := c.Await(ctx, start.RunID); err != nil {
		t.Fatalf("Await: %v", err)
	}

	clock.advance(2 * time.Minute)
	c.Sweep()

	status, ok := c.Status(start.RunID)
	if !ok || status.State != StateExpired {
		t.Fatalf("status = %#v ok=%v, want expired", status, ok)
	}
	if _, err := c.SnapshotOf(start.RunID); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("SnapshotOf(expired) = %v, want ErrUnknownRun", err)
	}
	if _, err := c.Await(ctx, start.RunID); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("Await(expired) = %v, want ErrUnknownRun", err)
	}
	if _, err := c.StartRun(ctx, StartRunOptions{Nonce: start.Nonce}); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("StartRun with an expired nonce = %v, want ErrUnknownRun", err)
	}

	clock.advance(2 * time.Minute)
	c.Sweep()
	if _, ok := c.Status(start.RunID); ok {
		t.Fatal("the tombstone should have been dropped")
	}
}

func TestSweepAbortsRunsPastTheirStartedTTL(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	c, _ := newTestController(t, func(o *Options) { o.Now = clock.Now })
	g := newFakeGeneration("http")
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	clock.advance(2 * time.Minute)
	c.Sweep()

	status, _ := c.Status(start.RunID)
	if status.State != StateAborted || status.Reason != ReasonStartedTTL {
		t.Fatalf("status = %#v, want aborted by started-ttl", status)
	}
}

func TestFinishLeaseExpiry_AbortsRun(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	c, hr := newTestController(t, func(o *Options) { o.Now = clock.Now })
	g := newFakeGeneration("http")
	g.drainBlock = make(chan struct{})
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	clock.advance(10 * time.Second)
	c.Sweep()
	close(g.drainBlock)

	status, _ := c.Status(start.RunID)
	if status.State != StateAborted || status.Reason != ReasonFinishLeaseExpired {
		t.Fatalf("status = %#v, want aborted by finish-lease-expired", status)
	}
	if !hr.has(HealthLeaseExpired) {
		t.Fatalf("health key %s was not recorded", HealthLeaseExpired)
	}
	if _, err := c.SnapshotOf(start.RunID); err == nil {
		t.Fatal("a lease-expired run must not publish a snapshot")
	}
	if _, err := c.StartRun(ctx, StartRunOptions{}); err != nil {
		t.Fatalf("the next run must start after a lease expiry: %v", err)
	}
}

func TestWatchdogRunsAutomatically(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	c, err := New(Options{Budgets: testBudgets(), Now: clock.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	g := newFakeGeneration("http")
	if err := c.RegisterGeneration(Registration{Name: "http", Required: true}, g); err != nil {
		t.Fatalf("RegisterGeneration: %v", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	clock.advance(2 * time.Minute)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := c.Status(start.RunID); status.State == StateAborted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the watchdog never reclaimed the run")
}

func TestNoCollectorsStillProducesARun(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, nil)

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.Validity != ValidityValid || len(start.Collectors) != 0 {
		t.Fatalf("start = %#v", start)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	status, err := c.Await(ctx, start.RunID)
	if err != nil || status.State != StateFinished {
		t.Fatalf("Await = %#v, %v", status, err)
	}
}

func TestEnrichHookRunsBeforePublish(t *testing.T) {
	ctx := context.Background()
	var seen string
	c, _ := newTestController(t, func(o *Options) {
		o.Enrich = func(_ context.Context, s *Snapshot) error {
			seen = s.RunID
			s.Sections["explain"] = "plan"
			return nil
		}
	})
	g := newFakeGeneration("http")
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := c.Await(ctx, start.RunID); err != nil {
		t.Fatalf("Await: %v", err)
	}
	snap, err := c.SnapshotOf(start.RunID)
	if err != nil {
		t.Fatalf("SnapshotOf: %v", err)
	}
	if seen != start.RunID || snap.Sections["explain"] != "plan" {
		t.Fatalf("enrichment did not reach the snapshot: %#v", snap.Sections)
	}
}

func TestEnrichFailureDegradesToPartial(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, func(o *Options) {
		o.Enrich = func(context.Context, *Snapshot) error { return fmt.Errorf("explain refused") }
	})
	g := newFakeGeneration("http")
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, _ := c.StartRun(ctx, StartRunOptions{})
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	status, err := c.Await(ctx, start.RunID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if status.Validity != ValidityPartial {
		t.Fatalf("validity = %s, want partial after a failed enrichment", status.Validity)
	}
}

func TestDefaultControllerIsShared(t *testing.T) {
	first := Default()
	if first == nil {
		t.Fatal("Default returned nil")
	}
	second := Default()
	if first != second {
		t.Fatal("Default must return one process-wide Controller")
	}
}

func TestValidityOnlyDegrades(t *testing.T) {
	tests := []struct {
		a, b, want Validity
	}{
		{ValidityValid, ValidityValid, ValidityValid},
		{ValidityValid, ValidityPartial, ValidityPartial},
		{ValidityPartial, ValidityValid, ValidityPartial},
		{ValidityPartial, ValidityInvalid, ValidityInvalid},
		{ValidityInvalid, ValidityPartial, ValidityInvalid},
		{"", ValidityValid, ValidityValid},
	}
	for _, tt := range tests {
		if got := worse(tt.a, tt.b); got != tt.want {
			t.Fatalf("worse(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestHandleAccessors(t *testing.T) {
	var zeroGen GenerationHandle
	if !zeroGen.Zero() {
		t.Fatal("the zero generation handle must report Zero")
	}
	gen := NewGenerationHandle("run-1", 7, "http", 3, "token")
	if gen.Zero() || gen.Token() != "token" || gen.Gen != 3 || gen.Epoch != 7 {
		t.Fatalf("generation handle = %#v", gen)
	}

	var zeroBase BaselineHandle
	if !zeroBase.Zero() {
		t.Fatal("the zero baseline handle must report Zero")
	}
	at := time.Now()
	base := NewBaselineHandle("run-1", 7, "proc", PhaseStartBaseline, at, fakeSample{Value: 5})
	if base.Zero() || base.Sample() != (fakeSample{Value: 5}) || !base.SampledAt.Equal(at) {
		t.Fatalf("baseline handle = %#v", base)
	}
}
