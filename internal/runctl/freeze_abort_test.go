package runctl

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingBaseline parks inside CaptureFinal until the test releases it. It is
// the only way to hold a collector inside the *synchronous* part of FinishRun
// while an abort runs, which is precisely the window an abort must not report
// as a clean join.
type blockingBaseline struct {
	name      string
	entered   chan struct{}
	release   chan struct{}
	ignoreCtx bool

	enterOnce sync.Once
	inFinal   atomic.Bool
}

func newBlockingBaseline(name string, ignoreCtx bool) *blockingBaseline {
	return &blockingBaseline{
		name:      name,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		ignoreCtx: ignoreCtx,
	}
}

func (b *blockingBaseline) Name() string { return b.name }

func (b *blockingBaseline) CaptureBaseline(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	at := time.Now()
	return SampleResult{
		Handle:    NewBaselineHandle(runID, ep, b.name, PhaseStartBaseline, at, fakeSample{}),
		At:        at,
		Committed: true,
	}, nil
}

func (b *blockingBaseline) CaptureFinal(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	b.inFinal.Store(true)
	defer b.inFinal.Store(false)
	b.enterOnce.Do(func() { close(b.entered) })

	if b.ignoreCtx {
		<-b.release
	} else {
		select {
		case <-b.release:
		case <-ctx.Done():
			return SampleResult{}, ctx.Err()
		}
	}
	at := time.Now()
	return SampleResult{
		Handle:    NewBaselineHandle(runID, ep, b.name, PhaseFinishFinal, at, fakeSample{}),
		At:        at,
		Committed: true,
	}, nil
}

func (b *blockingBaseline) Collect(base, final BaselineHandle) (any, error) {
	return fakeSample{}, nil
}

func (b *blockingBaseline) Release(h BaselineHandle) {}

// TestAbortDuringFreeze_NeverReportsAFalseCleanJoin covers the window between
// "the run is marked finishing" and "the background worker exists": FinishRun
// runs the freeze phases itself, so an abort issued during them must either
// wait for that goroutine or say it gave up. Reporting Detached=false while a
// collector is still inside CaptureFinal would tell the caller the run is
// fully stopped when it demonstrably is not, and the next run's opening
// boundary would then overlap the previous run's closing one.
func TestAbortDuringFreeze_NeverReportsAFalseCleanJoin(t *testing.T) {
	tests := []struct {
		name      string
		ignoreCtx bool
	}{
		{name: "collector honours cancellation", ignoreCtx: false},
		{name: "collector ignores cancellation", ignoreCtx: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c, _ := newTestController(t, nil)
			b := newBlockingBaseline("proc", tt.ignoreCtx)
			if err := c.RegisterBaseline(Registration{Name: "proc", Required: true}, b); err != nil {
				t.Fatalf("RegisterBaseline: %v", err)
			}

			start, err := c.StartRun(ctx, StartRunOptions{})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}

			var (
				wg        sync.WaitGroup
				finishErr error
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, finishErr = c.FinishRun(ctx, start.RunID)
			}()

			select {
			case <-b.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("CaptureFinal was never entered")
			}

			result, err := c.AbortRun(ctx, start.RunID, ReasonExplicit)
			if err != nil {
				t.Fatalf("AbortRun: %v", err)
			}
			// The join channel closes only after the freeze goroutine has
			// retired its ownership, so a clean join happens-before this load.
			if !result.Detached && b.inFinal.Load() {
				t.Fatal("AbortRun reported a clean join while the collector was still inside CaptureFinal")
			}
			if tt.ignoreCtx && !result.Detached {
				t.Fatal("AbortRun joined a collector that cannot return, so the join was not real")
			}

			close(b.release)
			wg.Wait()

			if !errors.Is(finishErr, ErrRunAborted) {
				t.Fatalf("FinishRun = %v, want ErrRunAborted for a run aborted mid-freeze", finishErr)
			}
			status, _ := c.Status(start.RunID)
			if status.State != StateAborted {
				t.Fatalf("state = %s, want aborted", status.State)
			}
			if _, err := c.SnapshotOf(start.RunID); err == nil {
				t.Fatal("a run aborted mid-freeze published a snapshot")
			}
		})
	}
}
