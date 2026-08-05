package runctl

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSerializeInitialize_NoOverlap(t *testing.T) {
	ctx := context.Background()

	var (
		mu       sync.Mutex
		inside   int
		maxSeen  int
		finished int
	)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := SerializeInitialize(ctx, func(context.Context) error {
				mu.Lock()
				inside++
				if inside > maxSeen {
					maxSeen = inside
				}
				mu.Unlock()

				time.Sleep(10 * time.Millisecond)

				mu.Lock()
				inside--
				finished++
				mu.Unlock()
				return nil
			})
			if err != nil {
				t.Errorf("SerializeInitialize: %v", err)
			}
		}()
	}
	wg.Wait()

	if maxSeen != 1 {
		t.Fatalf("%d initializes overlapped, want strictly one at a time", maxSeen)
	}
	if finished != 4 {
		t.Fatalf("%d initializes completed, want 4", finished)
	}
}

func TestSerializeInitialize_Busy(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	holding := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := SerializeInitialize(ctx, func(context.Context) error {
			close(holding)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("holder: %v", err)
		}
	}()
	<-holding

	err := SerializeInitializeWithBudget(ctx, 20*time.Millisecond, func(context.Context) error {
		t.Error("the second initialize must not run while the guard is held")
		return nil
	})
	if !errors.Is(err, ErrInitializeBusy) {
		t.Fatalf("SerializeInitializeWithBudget = %v, want ErrInitializeBusy", err)
	}

	close(release)
	wg.Wait()

	// The guard must be reusable once the holder is gone.
	var ran atomic.Bool
	if err := SerializeInitialize(ctx, func(context.Context) error { ran.Store(true); return nil }); err != nil {
		t.Fatalf("SerializeInitialize after release: %v", err)
	}
	if !ran.Load() {
		t.Fatal("the guard did not release")
	}
}

func TestSerializeInitialize_MarksTheContext(t *testing.T) {
	ctx := context.Background()
	if HasInitializeGuard(ctx) {
		t.Fatal("a bare context must not look like it came from the guard")
	}
	if HasInitializeGuard(context.TODO()) {
		t.Fatal("a placeholder context must not look like it came from the guard")
	}
	// A nil context is a caller mistake the guard must survive rather than
	// panic on, so the nil-safety branch stays under test. The nil travels in
	// a variable because a nil Context is the input under test here, not the
	// accidental nil argument SA1012 is meant to catch.
	var nilCtx context.Context
	if HasInitializeGuard(nilCtx) {
		t.Fatal("a nil context must not look like it came from the guard")
	}

	var inside bool
	err := SerializeInitialize(ctx, func(guarded context.Context) error {
		inside = HasInitializeGuard(guarded)
		// Values must survive derivation, since handlers wrap the context.
		derived, cancel := context.WithTimeout(guarded, time.Second)
		defer cancel()
		if !HasInitializeGuard(derived) {
			t.Error("the marker did not survive a derived context")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("SerializeInitialize: %v", err)
	}
	if !inside {
		t.Fatal("SerializeInitialize did not mark the context it passes to fn")
	}
}

func TestSerializeInitialize_PropagatesErrorsAndRejectsNil(t *testing.T) {
	ctx := context.Background()
	want := errors.New("rebuild failed")
	if err := SerializeInitialize(ctx, func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("SerializeInitialize = %v, want the caller's error", err)
	}
	if err := SerializeInitialize(ctx, nil); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("SerializeInitialize(nil) = %v, want ErrInvalidRegistration", err)
	}
}

func TestSerializeInitialize_RespectsCallerCancellation(t *testing.T) {
	release := make(chan struct{})
	holding := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := SerializeInitialize(context.Background(), func(context.Context) error {
			close(holding)
			<-release
			return nil
		}); err != nil {
			t.Errorf("holder: %v", err)
		}
	}()
	<-holding

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := SerializeInitialize(ctx, func(context.Context) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SerializeInitialize = %v, want the caller's deadline", err)
	}

	close(release)
	wg.Wait()
}

// TestSerializeInitializeWithPreemptingReset is the shape an initialize
// handler is expected to take: the whole rebuild plus the reset is serialized,
// and the reset preempts whatever run was still in flight so the last
// initialize is the one that owns the measurement.
func TestSerializeInitializeWithPreemptingReset(t *testing.T) {
	c, _, _, _ := startedController(t)

	first, err := c.StartRun(context.Background(), StartRunOptions{Reason: "api"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	var runID string
	err = SerializeInitialize(context.Background(), func(ctx context.Context) error {
		if !HasInitializeGuard(ctx) {
			t.Error("the guard marker is missing inside fn")
		}
		start, err := c.StartRun(ctx, StartRunOptions{Preempt: true, Reason: "initialize"})
		if err != nil {
			return err
		}
		runID = start.RunID
		return nil
	})
	if err != nil {
		t.Fatalf("SerializeInitialize: %v", err)
	}

	if runID == first.RunID {
		t.Fatal("the initialize reused the previous run")
	}
	status, _ := c.Status(first.RunID)
	if status.State != StateAborted || status.Validity != ValidityInvalid {
		t.Fatalf("the displaced run = %#v, want aborted/invalid", status)
	}
	current, ok := c.Status(runID)
	if !ok || current.State != StateStarted {
		t.Fatalf("the new run = %#v, want started", current)
	}
}
