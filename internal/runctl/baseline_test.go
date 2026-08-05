package runctl

import (
	"context"
	"testing"
	"time"
)

// TestBaselineCollect_UsesFrozenSamplesOnly is the conformance test for the
// rule that makes a run's numbers reproducible: an interval is derived from
// the two frozen samples the handles carry, never from the collector's live
// state. Without it, "the snapshot was built from fixed values" is a comment
// rather than a property, and load applied after the freeze silently leaks
// into the measured interval.
func TestBaselineCollect_UsesFrozenSamplesOnly(t *testing.T) {
	t.Run("Collect ignores state changed after the boundary", func(t *testing.T) {
		ctx := context.Background()
		c, _ := newTestController(t, nil)
		b := newFakeBaseline("proc")
		b.setLive(1000)
		registerPair(t, c, nil, Registration{}, b, Registration{Name: "proc", Required: true})

		start, err := c.StartRun(ctx, StartRunOptions{})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		b.setLive(1250)

		if _, err := c.FinishRun(ctx, start.RunID); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
		// Load applied after the closing boundary. If Collect read live state,
		// this would land in the interval.
		b.setLive(999999)

		if _, err := c.Await(ctx, start.RunID); err != nil {
			t.Fatalf("Await: %v", err)
		}
		snap, err := c.SnapshotOf(start.RunID)
		if err != nil {
			t.Fatalf("SnapshotOf: %v", err)
		}
		if got := snap.Sections["proc"]; got != (fakeSample{Value: 250}) {
			t.Fatalf("proc interval = %#v, want the frozen 1250-1000 delta", got)
		}
	})

	t.Run("Collect performs no I/O", func(t *testing.T) {
		base := NewBaselineHandle("run-1", 1, "proc", PhaseStartBaseline, time.Now(), fakeSample{Value: 10})
		final := NewBaselineHandle("run-1", 1, "proc", PhaseFinishFinal, time.Now(), fakeSample{Value: 42})

		b := newFakeBaseline("proc")
		b.io.arm()
		value, err := b.Collect(base, final)
		if err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if value != (fakeSample{Value: 32}) {
			t.Fatalf("Collect = %#v, want the delta of the two frozen samples", value)
		}
		if got := b.io.violations(); got != 0 {
			t.Fatalf("Collect touched collector state %d times, want 0", got)
		}
	})

	t.Run("the conformance harness detects a collector that cheats", func(t *testing.T) {
		base := NewBaselineHandle("run-1", 1, "proc", PhaseStartBaseline, time.Now(), fakeSample{Value: 10})
		final := NewBaselineHandle("run-1", 1, "proc", PhaseFinishFinal, time.Now(), fakeSample{Value: 42})

		b := newFakeBaseline("proc")
		b.collectTouchesLive = true
		b.io.arm()
		if _, err := b.Collect(base, final); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if b.io.violations() == 0 {
			t.Fatal("the harness failed to notice a collector reading its own live state")
		}
	})

	t.Run("mutating the value from Sample does not reach other holders", func(t *testing.T) {
		original := fakeSample{Value: 7}
		handle := NewBaselineHandle("run-1", 1, "proc", PhaseStartBaseline, time.Now(), original)
		otherHolder := handle // handles are values and get copied around freely

		mutated, ok := handle.Sample().(fakeSample)
		if !ok {
			t.Fatalf("Sample() = %T, want fakeSample", handle.Sample())
		}
		mutated.Value = 9999

		if got := otherHolder.Sample().(fakeSample); got != original {
			t.Fatalf("another holder now sees %#v, want the frozen %#v", got, original)
		}
		if got := handle.Sample().(fakeSample); got != original {
			t.Fatalf("the handle itself now sees %#v, want the frozen %#v", got, original)
		}
	})

	t.Run("a type mismatch is an error, never a panic", func(t *testing.T) {
		b := newFakeBaseline("proc")
		wrong := NewBaselineHandle("run-1", 1, "proc", PhaseStartBaseline, time.Now(), "not a sample")
		right := NewBaselineHandle("run-1", 1, "proc", PhaseFinishFinal, time.Now(), fakeSample{Value: 1})

		if _, err := b.Collect(wrong, right); err == nil {
			t.Fatal("a mismatched sample type must be reported as an error")
		}
		if _, err := b.Collect(right, wrong); err == nil {
			t.Fatal("a mismatched final sample type must be reported as an error")
		}
	})
}

// TestBaselineHandlesAreReleased checks that the Controller frees what it
// pins, so a long benchmark session does not accumulate frozen samples. It
// deliberately asserts "at least one Release per captured handle" rather than
// exactly one: Release is idempotent by contract and overlapping owners may
// legitimately call it twice, so an exact count would pin an implementation
// detail instead of the contract.
func TestBaselineHandlesAreReleased(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, nil)
	b := newFakeBaseline("proc")
	registerPair(t, c, nil, Registration{}, b, Registration{Name: "proc", Required: true})

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

	captures, collects, releases := b.counts()
	if captures != 2 {
		t.Fatalf("captures = %d, want one per boundary", captures)
	}
	if collects != 1 {
		t.Fatalf("collects = %d, want 1", collects)
	}
	if releases < 2 {
		t.Fatalf("releases = %d, want at least one per captured handle", releases)
	}
}
