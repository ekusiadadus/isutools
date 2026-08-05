package runctl

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestAbortDuringFinishing_NoSnapshotPublished is the regression test for the
// failure this package exists to make impossible: a background worker that
// belongs to an abandoned run publishing its snapshot anyway, dragging the run
// back to "finished" and handing an operator data from a run that was
// explicitly thrown away.
//
// Run with -race (and ideally -count=200): the abort is issued while the
// worker is blocked in Drain, so the two race on every iteration.
func TestAbortDuringFinishing_NoSnapshotPublished(t *testing.T) {
	const iterations = 25
	ctx := context.Background()

	for i := 0; i < iterations; i++ {
		func() {
			c, _ := newTestController(t, nil)
			g := newFakeGeneration("http")
			g.drainBlock = make(chan struct{})
			b := newFakeBaseline("proc")
			registerPair(t, c, g, Registration{Name: "http", Required: true}, b, Registration{Name: "proc", Required: true})

			start, err := c.StartRun(ctx, StartRunOptions{})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			if _, err := c.FinishRun(ctx, start.RunID); err != nil {
				t.Fatalf("FinishRun: %v", err)
			}

			var (
				wg      sync.WaitGroup
				result  AbortResult
				abortEr error
				polled  error
			)
			wg.Add(2)
			go func() {
				defer wg.Done()
				result, abortEr = c.AbortRun(ctx, start.RunID, ReasonExplicit)
			}()
			go func() {
				defer wg.Done()
				// Poll while the abort races the worker: a snapshot must never
				// be observable, not even transiently.
				for j := 0; j < 200; j++ {
					if _, err := c.SnapshotOf(start.RunID); err == nil {
						polled = errors.New("a snapshot became readable for an aborted run")
						return
					}
				}
			}()
			wg.Wait()
			close(g.drainBlock)

			if abortEr != nil {
				t.Fatalf("AbortRun: %v", abortEr)
			}
			if polled != nil {
				t.Fatal(polled)
			}
			if _, err := c.SnapshotOf(start.RunID); err == nil {
				t.Fatal("the aborted run published a snapshot")
			}
			if c.PublishedSnapshots() != 0 {
				t.Fatalf("%d snapshots were published for an aborted run", c.PublishedSnapshots())
			}
			status, _ := c.Status(start.RunID)
			if status.State != StateAborted {
				t.Fatalf("state = %s, want aborted; the worker dragged the run back", status.State)
			}
			if c.StaleRejections() == 0 {
				t.Fatal("the epoch fence never rejected anything, so the race was not actually exercised")
			}
			if result.RunID != start.RunID || result.Reason != ReasonExplicit {
				t.Fatalf("abort result = %#v", result)
			}
		}()
	}
}

// TestStaleWorkerCommitRejected checks the fence directly: once a run is
// abandoned, neither a state change nor a snapshot from its worker may land.
func TestStaleWorkerCommitRejected(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := startedController(t)

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	staleEpoch := start.Epoch

	if err := c.commit(staleEpoch, func(s *runSlot) { s.trigger = "before-abort" }); err != nil {
		t.Fatalf("a current-epoch commit must be accepted: %v", err)
	}

	if _, err := c.AbortRun(ctx, start.RunID, ReasonExplicit); err != nil {
		t.Fatalf("AbortRun: %v", err)
	}
	before := c.StaleRejections()

	if err := c.commit(staleEpoch, func(s *runSlot) { s.state = StateFinished }); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale commit = %v, want ErrStaleEpoch", err)
	}
	if err := c.publish(staleEpoch, &Snapshot{RunID: start.RunID}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale publish = %v, want ErrStaleEpoch", err)
	}
	if c.StaleRejections() != before+2 {
		t.Fatalf("stale rejections = %d, want %d", c.StaleRejections(), before+2)
	}

	status, _ := c.Status(start.RunID)
	if status.State != StateAborted {
		t.Fatalf("state = %s, want aborted", status.State)
	}
	if _, err := c.SnapshotOf(start.RunID); err == nil {
		t.Fatal("a stale publish stored a snapshot")
	}
	if c.PublishedSnapshots() != 0 {
		t.Fatal("a stale publish was counted as published")
	}
}

// TestAbortJoinTimeout_DetachedRecorded proves that a worker which cannot be
// joined is a resource problem and not a correctness problem: the abort
// completes, the condition is visible in health, and the next run starts
// immediately even while the old worker is still running.
func TestAbortJoinTimeout_DetachedRecorded(t *testing.T) {
	ctx := context.Background()
	c, hr := newTestController(t, nil)
	g := newFakeGeneration("http")
	g.drainBlock = make(chan struct{})
	g.drainIgnoresCtx = true // a collector that ignores cancellation
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	result, err := c.AbortRun(ctx, start.RunID, ReasonExplicit)
	if err != nil {
		t.Fatalf("AbortRun: %v", err)
	}
	if !result.Detached {
		t.Fatal("AbortResult.Detached = false, want true when the worker outlives the join budget")
	}
	if !hr.has(HealthWorkerDetached) {
		t.Fatalf("health key %s was not recorded", HealthWorkerDetached)
	}
	status, _ := c.Status(start.RunID)
	if status.State != StateAborted || !status.Detached {
		t.Fatalf("status = %#v, want aborted and detached", status)
	}

	next, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("a detached worker must not block the next run: %v", err)
	}
	if next.RunID == start.RunID {
		t.Fatal("the new run reused the detached run ID")
	}

	// Release the stuck worker and confirm it still cannot publish anything.
	close(g.drainBlock)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c.StaleRejections() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.StaleRejections() == 0 {
		t.Fatal("the detached worker was never fenced")
	}
	if _, err := c.SnapshotOf(start.RunID); err == nil {
		t.Fatal("the detached worker published a snapshot")
	}
}

// TestConcurrentAbortsConverge checks that several callers aborting the same
// run all observe the same outcome instead of racing a second teardown.
func TestConcurrentAbortsConverge(t *testing.T) {
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

	const callers = 6
	results := make([]AbortResult, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := c.AbortRun(ctx, start.RunID, ReasonExplicit)
			if err != nil {
				t.Errorf("AbortRun: %v", err)
				return
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	close(g.drainBlock)

	for i, res := range results {
		if res.RunID != start.RunID {
			t.Fatalf("caller %d saw %#v", i, res)
		}
	}
	status, _ := c.Status(start.RunID)
	if status.State != StateAborted {
		t.Fatalf("state = %s, want aborted", status.State)
	}
}

// TestAbortDuringStartingFencesTheStarter covers the abort of a run whose
// opening boundary is still being taken: the starter must lose, not publish a
// half-taken boundary as a usable run.
func TestAbortDuringStartingFencesTheStarter(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, nil)
	b := newFakeBaseline("proc")
	b.captureDelay = 150 * time.Millisecond
	registerPair(t, c, nil, Registration{}, b, Registration{Name: "proc", Required: true})

	var (
		wg       sync.WaitGroup
		startErr error
		start    StartResult
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		start, startErr = c.StartRun(ctx, StartRunOptions{})
	}()

	// Give the starter time to claim the slot, then take it away.
	var runID string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if cur := c.currentLocked(); cur != nil && cur.state == StateStarting {
			runID = cur.runID
		}
		c.mu.Unlock()
		if runID != "" {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("no run reached the starting state")
	}
	if _, err := c.AbortRun(ctx, runID, ReasonExplicit); err != nil {
		t.Fatalf("AbortRun: %v", err)
	}
	wg.Wait()

	if !errors.Is(startErr, ErrRunAborted) {
		t.Fatalf("StartRun = %#v, %v; want ErrRunAborted", start, startErr)
	}
	status, _ := c.Status(runID)
	if status.State != StateAborted {
		t.Fatalf("state = %s, want aborted", status.State)
	}
	if _, err := c.StartRun(ctx, StartRunOptions{}); err != nil {
		t.Fatalf("the next run must start after a fenced start: %v", err)
	}
}
