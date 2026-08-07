package runctl

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type recordingLifecycleObserver struct {
	mu     sync.Mutex
	events []RunTerminationEvent
	hook   func(RunTerminationEvent)
}

func (o *recordingLifecycleObserver) OnRunTermination(event RunTerminationEvent) {
	if o.hook != nil {
		o.hook(event)
	}
	o.mu.Lock()
	o.events = append(o.events, event)
	o.mu.Unlock()
}

func (o *recordingLifecycleObserver) snapshot() []RunTerminationEvent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]RunTerminationEvent(nil), o.events...)
}

func TestLifecycleObserverRequiredFailurePrecedesStartReturn(t *testing.T) {
	t.Parallel()

	observer := &recordingLifecycleObserver{}
	c, _ := newTestController(t, func(o *Options) { o.LifecycleObserver = observer })
	g := newFakeGeneration("required")
	g.beginErr = errors.New("boom")
	if err := c.RegisterGeneration(Registration{Name: "required", Required: true}, g); err != nil {
		t.Fatal(err)
	}

	start, err := c.StartRun(context.Background(), StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.State != StateAborted {
		t.Fatalf("state = %s, want aborted", start.State)
	}
	events := observer.snapshot()
	if len(events) != 1 {
		t.Fatalf("events at StartRun return = %#v, want one", events)
	}
	assertTerminationEvent(t, events[0], start.RunID, start.Epoch, StateAborted, ValidityInvalid, ReasonRequiredFailed)
}

func TestLifecycleObserverFinishAcceptedPrecedesFinishReturn(t *testing.T) {
	t.Parallel()

	observer := &recordingLifecycleObserver{}
	c, _ := newTestController(t, func(o *Options) { o.LifecycleObserver = observer })
	start, err := c.StartRun(context.Background(), StartRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := c.FinishRun(context.Background(), start.RunID)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	events := observer.snapshot()
	if len(events) != 1 {
		t.Fatalf("events at FinishRun return = %#v, want one", events)
	}
	assertTerminationEvent(t, events[0], start.RunID, start.Epoch, StateFinishing, accepted.Validity, ReasonFinishAccepted)
	wantBoundary := accepted.GenerationWindow.Max
	if wantBoundary.IsZero() {
		wantBoundary = accepted.AcceptedAt
	}
	if !events[0].BoundaryAt.Equal(wantBoundary) {
		t.Fatalf("boundary_at = %v, want generation max %v", events[0].BoundaryAt, wantBoundary)
	}
}

func TestLifecycleObserverFinishCarriesWholeRunValidity(t *testing.T) {
	t.Parallel()

	observer := &recordingLifecycleObserver{}
	c, _ := newTestController(t, func(o *Options) { o.LifecycleObserver = observer })
	g := newFakeGeneration("optional")
	g.beginErr = errors.New("opening profile unavailable")
	if err := c.RegisterGeneration(Registration{Name: "optional"}, g); err != nil {
		t.Fatal(err)
	}
	start, err := c.StartRun(context.Background(), StartRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if start.Validity != ValidityPartial {
		t.Fatalf("start validity = %s, want partial", start.Validity)
	}
	if _, err := c.FinishRun(context.Background(), start.RunID); err != nil {
		t.Fatal(err)
	}
	events := observer.snapshot()
	if len(events) != 1 || events[0].Validity != ValidityPartial {
		t.Fatalf("events = %#v, want finish with whole-run partial validity", events)
	}
}

func TestLifecycleObserverAbortFenceReasons(t *testing.T) {
	t.Parallel()

	for _, reason := range []string{ReasonExplicit, ReasonHubAbort} {
		reason := reason
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			observer := &recordingLifecycleObserver{}
			var c *Controller
			observer.hook = func(event RunTerminationEvent) {
				status, ok := c.Status(event.RunID)
				if !ok || status.State != StateAborting {
					t.Errorf("observer status = %#v ok=%v, want aborting", status, ok)
				}
			}
			c, _ = newTestController(t, func(o *Options) { o.LifecycleObserver = observer })
			start, err := c.StartRun(context.Background(), StartRunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.AbortRun(context.Background(), start.RunID, reason); err != nil {
				t.Fatal(err)
			}
			events := observer.snapshot()
			if len(events) != 1 {
				t.Fatalf("events = %#v, want one", events)
			}
			assertTerminationEvent(t, events[0], start.RunID, start.Epoch, StateAborting, ValidityInvalid, reason)
		})
	}
}

func TestLifecycleObserverPreemptAndStartedTTL(t *testing.T) {
	t.Parallel()

	t.Run("preempt", func(t *testing.T) {
		t.Parallel()
		observer := &recordingLifecycleObserver{}
		c, _ := newTestController(t, func(o *Options) { o.LifecycleObserver = observer })
		first, err := c.StartRun(context.Background(), StartRunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.StartRun(context.Background(), StartRunOptions{Preempt: true})
		if err != nil {
			t.Fatal(err)
		}
		events := observer.snapshot()
		if len(events) != 1 {
			t.Fatalf("events = %#v, want preempt event", events)
		}
		assertTerminationEvent(t, events[0], first.RunID, first.Epoch, StateAborting, ValidityInvalid, ReasonPreemptedBy+second.RunID)
	})

	t.Run("started-ttl", func(t *testing.T) {
		t.Parallel()
		clock := newFakeClock()
		observer := &recordingLifecycleObserver{}
		c, _ := newTestController(t, func(o *Options) {
			o.Now = clock.Now
			o.LifecycleObserver = observer
		})
		start, err := c.StartRun(context.Background(), StartRunOptions{})
		if err != nil {
			t.Fatal(err)
		}
		clock.advance(testBudgets().StartedTTL + time.Second)
		c.Sweep()
		events := observer.snapshot()
		if len(events) != 1 {
			t.Fatalf("events = %#v, want ttl event", events)
		}
		assertTerminationEvent(t, events[0], start.RunID, start.Epoch, StateAborting, ValidityInvalid, ReasonStartedTTL)
	})
}

func TestLifecycleObserverPanicCannotBreakRunLifecycle(t *testing.T) {
	t.Parallel()

	observer := &recordingLifecycleObserver{hook: func(RunTerminationEvent) { panic("observer bug") }}
	c, _ := newTestController(t, func(o *Options) { o.LifecycleObserver = observer })
	start, err := c.StartRun(context.Background(), StartRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AbortRun(context.Background(), start.RunID, ReasonExplicit); err != nil {
		t.Fatalf("AbortRun after observer panic: %v", err)
	}
	status, ok := c.Status(start.RunID)
	if !ok || status.State != StateAborted {
		t.Fatalf("status = %#v ok=%v, want aborted", status, ok)
	}
}

func TestLifecycleObserverCannotBlockRunLifecycle(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	observer := &recordingLifecycleObserver{hook: func(RunTerminationEvent) { <-release }}
	c, _ := newTestController(t, func(o *Options) { o.LifecycleObserver = observer })
	start, err := c.StartRun(context.Background(), StartRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() {
		_, err := c.AbortRun(context.Background(), start.RunID, ReasonExplicit)
		returned <- err
	}()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("AbortRun: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(release)
		t.Fatal("blocking observer blocked AbortRun")
	}
	close(release)
}

func assertTerminationEvent(t *testing.T, event RunTerminationEvent, runID string, epoch Epoch, state RunState, validity Validity, reason string) {
	t.Helper()
	if event.RunID != runID || event.Epoch != epoch || event.State != state || event.Validity != validity || event.Reason != reason || event.BoundaryAt.IsZero() {
		t.Fatalf("event = %#v, want run=%s epoch=%d state=%s validity=%s reason=%s with boundary", event, runID, epoch, state, validity, reason)
	}
}
