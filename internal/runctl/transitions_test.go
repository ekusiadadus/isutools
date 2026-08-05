package runctl

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waitForState polls until a run reaches the wanted state. Polling is used
// instead of a hook so the test observes the same view an API caller would.
func waitForState(t *testing.T, c *Controller, runID string, want RunState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if status, ok := c.Status(runID); ok && status.State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	status, _ := c.Status(runID)
	t.Fatalf("run %s never reached %s (now %s)", runID, want, status.State)
}

// currentRunID returns the newest run the Controller retains.
func currentRunID(t *testing.T, c *Controller) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		cur := c.currentLocked()
		var id string
		if cur != nil {
			id = cur.runID
		}
		c.mu.Unlock()
		if id != "" {
			return id
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no run was ever recorded")
	return ""
}

func TestFinishRunWhileStartingIsRejected(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, nil)
	b := newFakeBaseline("proc")
	b.captureDelay = 150 * time.Millisecond
	registerPair(t, c, nil, Registration{}, b, Registration{Name: "proc", Required: true})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.StartRun(ctx, StartRunOptions{}); err != nil {
			t.Errorf("StartRun: %v", err)
		}
	}()

	runID := currentRunID(t, c)
	waitForState(t, c, runID, StateStarting)
	if _, err := c.FinishRun(ctx, runID); !errors.Is(err, ErrRunTransitioning) {
		t.Fatalf("FinishRun(starting) = %v, want ErrRunTransitioning", err)
	}
	wg.Wait()
}

func TestConcurrentFinishRunsShareOneAcceptance(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, nil)
	b := newFakeBaseline("proc")
	registerPair(t, c, nil, Registration{}, b, Registration{Name: "proc", Required: true})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	b.captureDelay = 100 * time.Millisecond

	const callers = 4
	results := make([]FinishAccepted, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			accepted, err := c.FinishRun(ctx, start.RunID)
			if err != nil {
				t.Errorf("FinishRun: %v", err)
				return
			}
			results[i] = accepted
		}(i)
	}
	wg.Wait()

	for i, got := range results {
		if got.RunID != start.RunID || !got.AcceptedAt.Equal(results[0].AcceptedAt) {
			t.Fatalf("caller %d saw a different acceptance: %#v vs %#v", i, got, results[0])
		}
	}
	if captures, _, _ := b.counts(); captures != 2 {
		t.Fatalf("baseline captured %d times, want one per boundary", captures)
	}
}

func TestFinishRunWaitCanBeAbandonedByTheCaller(t *testing.T) {
	c, _ := newTestController(t, nil)
	b := newFakeBaseline("proc")
	registerPair(t, c, nil, Registration{}, b, Registration{Name: "proc", Required: true})

	start, err := c.StartRun(context.Background(), StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	b.captureDelay = 200 * time.Millisecond

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.FinishRun(context.Background(), start.RunID); err != nil {
			t.Errorf("first FinishRun: %v", err)
		}
	}()
	waitForState(t, c, start.RunID, StateFinishing)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.FinishRun(ctx, start.RunID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FinishRun = %v, want the caller's deadline", err)
	}
	wg.Wait()
}

func TestOperationsOnAnExpiredRun(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	c, _ := newTestController(t, func(o *Options) { o.Now = clock.Now })
	g := newFakeGeneration("http")
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-expired"})
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

	if _, err := c.FinishRun(ctx, start.RunID); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("FinishRun(expired) = %v, want ErrUnknownRun", err)
	}
	if err := c.Ack(start.RunID); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("Ack(expired) = %v, want ErrUnknownRun", err)
	}
	res, err := c.AbortRun(ctx, start.RunID, ReasonExplicit)
	if err != nil {
		t.Fatalf("aborting an expired run must succeed: %v", err)
	}
	if res.RunID != start.RunID {
		t.Fatalf("abort result = %#v", res)
	}
	if _, err := c.StartRun(ctx, StartRunOptions{}); err != nil {
		t.Fatalf("a new run must start after expiry: %v", err)
	}
}

func TestNonceCacheExpiresWithItsTTL(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	c, _ := newTestController(t, func(o *Options) { o.Now = clock.Now })
	g := newFakeGeneration("http")
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	first, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-ttl"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	// Push the record out of the retention window so only the nonce cache can
	// answer, then let the cache age out too.
	for i := 0; i < RetainedRuns+1; i++ {
		if _, err := c.StartRun(ctx, StartRunOptions{Preempt: true}); err != nil {
			t.Fatalf("StartRun %d: %v", i, err)
		}
	}
	if _, ok := c.Status(first.RunID); ok {
		t.Fatal("the first run should have been evicted")
	}

	replay, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-ttl", Preempt: true})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if replay.RunID != first.RunID {
		t.Fatalf("nonce replay returned %s, want the cached %s", replay.RunID, first.RunID)
	}

	clock.advance(2 * time.Minute)
	fresh, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-ttl", Preempt: true})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if fresh.RunID == first.RunID {
		t.Fatal("an expired nonce must not replay its cached result")
	}
}

func TestNonceCacheIsBounded(t *testing.T) {
	c, _ := newTestController(t, nil)
	c.mu.Lock()
	for i := 0; i < NonceHistoryMax*2; i++ {
		c.rememberNonceLocked("n", StartResult{State: StateStarted})
	}
	size := len(c.nonce)
	c.mu.Unlock()
	if size > NonceHistoryMax {
		t.Fatalf("nonce history holds %d entries, want at most %d", size, NonceHistoryMax)
	}

	c.mu.Lock()
	c.rememberNonceLocked("", StartResult{State: StateStarted})
	c.rememberNonceLocked("aborted", StartResult{State: StateAborted})
	_, cachedAborted := c.lookupNonceLocked("aborted")
	_, cachedMissing := c.lookupNonceLocked("never-seen")
	_, cachedEmpty := c.lookupNonceLocked("")
	c.mu.Unlock()
	if cachedAborted {
		t.Fatal("an aborted start must not be replayable: its run ID can never produce data")
	}
	if cachedMissing || cachedEmpty {
		t.Fatal("lookupNonceLocked invented a cache hit")
	}
}

func TestStartRunDuringAbortingRun(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, func(o *Options) { o.Budgets.AbortJoin = 2 * time.Second })
	g := newFakeGeneration("http")
	g.drainBlock = make(chan struct{})
	g.drainIgnoresCtx = true
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-abort"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.AbortRun(ctx, start.RunID, ReasonExplicit); err != nil {
			t.Errorf("AbortRun: %v", err)
		}
	}()
	waitForState(t, c, start.RunID, StateAborting)

	if _, err := c.StartRun(ctx, StartRunOptions{Nonce: "n-abort"}); !errors.Is(err, ErrRunTransitioning) {
		t.Fatalf("StartRun(same nonce, aborting) = %v, want ErrRunTransitioning", err)
	}
	if _, err := c.StartRun(ctx, StartRunOptions{}); !errors.Is(err, ErrRunActive) {
		t.Fatalf("StartRun(other, aborting) = %v, want ErrRunActive", err)
	}
	if err := c.Ack(start.RunID); !errors.Is(err, ErrRunAborted) {
		t.Fatalf("Ack(aborting) = %v, want ErrRunAborted", err)
	}

	// A preempting start must queue behind the teardown rather than racing it.
	waiting := make(chan StartResult, 1)
	go func() {
		res, err := c.StartRun(ctx, StartRunOptions{Preempt: true})
		if err != nil {
			t.Errorf("preempting StartRun: %v", err)
			return
		}
		waiting <- res
	}()

	close(g.drainBlock)
	wg.Wait()

	select {
	case res := <-waiting:
		if res.State != StateStarted {
			t.Fatalf("queued start = %#v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the queued start never completed")
	}
}

func TestAwaitDuringAbortingRun(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, nil)
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

	done := make(chan RunStatus, 1)
	go func() {
		status, err := c.Await(ctx, start.RunID)
		if err != nil {
			t.Errorf("Await: %v", err)
			return
		}
		done <- status
	}()

	if _, err := c.AbortRun(ctx, start.RunID, ReasonHubAbort); err != nil {
		t.Fatalf("AbortRun: %v", err)
	}
	close(g.drainBlock)

	select {
	case status := <-done:
		if status.State != StateAborted || status.Reason != ReasonHubAbort {
			t.Fatalf("Await returned %#v, want an aborted run", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await never woke up")
	}
}

func TestDetachedWorkerHandlesAreReaped(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, func(o *Options) { o.Budgets.AbortJoin = 30 * time.Millisecond })
	g := newFakeGeneration("http")
	g.drainBlock = make(chan struct{})
	g.drainIgnoresCtx = true
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
		t.Fatal("expected a detached worker")
	}
	close(g.drainBlock)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, _, _, releases := g.counts(); releases > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the reaper never reclaimed the detached worker's handles")
}

func TestGenerationPhaseBudgetExhaustion(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, func(o *Options) {
		o.Budgets.PhaseBoundary = 60 * time.Millisecond
		o.Budgets.PerCollectorGeneration = 50 * time.Millisecond
	})
	fast := newFakeGeneration("fast")
	for _, name := range []string{"slow-1", "slow-2"} {
		slow := newFakeGeneration(name)
		slow.beginDelay = 200 * time.Millisecond
		if err := c.RegisterGeneration(Registration{Name: name}, slow); err != nil {
			t.Fatalf("RegisterGeneration: %v", err)
		}
	}
	if err := c.RegisterGeneration(Registration{Name: "fast", Required: true}, fast); err != nil {
		t.Fatalf("RegisterGeneration: %v", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.Validity != ValidityInvalid || start.State != StateAborted {
		t.Fatalf("start = %s/%s, want aborted/invalid once a required collector is skipped", start.State, start.Validity)
	}
	assertBoundary(t, start.Collectors, "fast", PhaseStartBoundary, CodeNotCaptured, false, true)
	if begins, _, _, _, _ := fast.counts(); begins != 0 {
		t.Fatalf("the skipped collector was called %d times, want 0", begins)
	}
}

func TestBaselinePhaseBudgetSkipsLaterSerialCollectors(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, func(o *Options) {
		o.Budgets.PhaseBaseline = 60 * time.Millisecond
		o.Budgets.PerCollectorBaseline = 50 * time.Millisecond
	})
	skipped := newFakeBaseline("skipped")
	for _, name := range []string{"slow-1", "slow-2"} {
		slow := newFakeBaseline(name)
		slow.captureDelay = 200 * time.Millisecond
		if err := c.RegisterBaseline(Registration{Name: name, SerialOnly: true}, slow); err != nil {
			t.Fatalf("RegisterBaseline: %v", err)
		}
	}
	if err := c.RegisterBaseline(Registration{Name: "skipped", SerialOnly: true, Required: true}, skipped); err != nil {
		t.Fatalf("RegisterBaseline: %v", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	assertBoundary(t, start.Collectors, "skipped", PhaseStartBaseline, CodeNotCaptured, false, true)
	if captures, _, _ := skipped.counts(); captures != 0 {
		t.Fatalf("the skipped collector sampled %d times, want 0", captures)
	}
	if start.Validity != ValidityInvalid {
		t.Fatalf("validity = %s, want invalid once a required baseline is skipped", start.Validity)
	}
}

func TestControllerWithoutHealthRecorder(t *testing.T) {
	ctx := context.Background()
	c, err := New(Options{Budgets: testBudgets(), DisableWatchdog: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	g := newFakeGeneration("http")
	g.beginCommitted = false // provokes a health report that has nowhere to go
	if err := c.RegisterGeneration(Registration{Name: "http"}, g); err != nil {
		t.Fatalf("RegisterGeneration: %v", err)
	}
	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun must not depend on health reporting: %v", err)
	}
	if start.Validity != ValidityInvalid {
		t.Fatalf("validity = %s, want invalid", start.Validity)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, err := New(Options{Budgets: testBudgets()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Close()
	c.Close()
}
