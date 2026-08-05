package counters

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

func collectFrozen(t *testing.T, c *GenerationCollector, h runctl.GenerationHandle) Frozen {
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

func countOf(frozen Frozen, name string) int64 {
	for _, entry := range frozen.Entries {
		if entry.Name == name {
			return entry.Count
		}
	}
	return 0
}

func TestCountersGenerationBoundaryFreezeDrainCollect(t *testing.T) {
	registry := NewRegistry()
	c := NewGenerationCollector(registry)
	ctx := context.Background()

	registry.Add("cache_hit", 3) // before the run

	start, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if !start.Committed || start.At.IsZero() {
		t.Fatalf("BeginBoundary = %+v, want a committed, timed boundary", start)
	}

	registry.Add("cache_hit", 5) // inside the run
	registry.Add("cache_miss", 2)

	final, err := c.Freeze(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if !final.Committed {
		t.Fatal("Freeze must commit the swap")
	}

	registry.Add("cache_hit", 11) // after the run

	before := collectFrozen(t, c, start.Handle)
	if got := countOf(before, "cache_hit"); got != 3 {
		t.Fatalf("pre-run cache_hit = %d, want 3", got)
	}
	run := collectFrozen(t, c, final.Handle)
	if got := countOf(run, "cache_hit"); got != 5 {
		t.Fatalf("run cache_hit = %d, want 5", got)
	}
	if got := countOf(run, "cache_miss"); got != 2 {
		t.Fatalf("run cache_miss = %d, want 2", got)
	}
	if len(run.Entries) != 2 || run.Entries[0].Name != "cache_hit" {
		t.Fatalf("entries = %#v, want cache_hit first", run.Entries)
	}
	// The count recorded after the freeze belongs to the generation that is
	// still open, not to the run that was just closed.
	current := registry.Snapshot()
	if len(current) != 1 || current[0].Name != "cache_hit" || current[0].Count != 11 {
		t.Fatalf("open generation = %#v, want only cache_hit=11", current)
	}
}

func TestCountersGenerationDrainHonoursContextCancellation(t *testing.T) {
	registry := NewRegistry()
	c := NewGenerationCollector(registry)

	registry.Add("cache_hit", 1)
	res, err := c.BeginBoundary(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err = c.Drain(ctx, res.Handle)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Drain error = %v, want a wrapped context.Canceled", err)
	}
	if elapsed > runctl.DrainCancelGrace {
		t.Fatalf("Drain took %v, want at most DrainCancelGrace (%v)", elapsed, runctl.DrainCancelGrace)
	}
	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrNotDrained) {
		t.Fatalf("Collect before Drain = %v, want ErrNotDrained", err)
	}

	// The same handle drains normally once it is given a live context.
	if got := countOf(collectFrozen(t, c, res.Handle), "cache_hit"); got != 1 {
		t.Fatalf("cache_hit after retrying Drain = %d, want 1", got)
	}
	// A settled generation has nothing to wait for, so a dead context no
	// longer fails it.
	if err := c.Drain(ctx, res.Handle); err != nil {
		t.Fatalf("Drain of a settled generation with a cancelled context: %v", err)
	}
}

func TestCountersGenerationReplaysSameRunAndEpoch(t *testing.T) {
	registry := NewRegistry()
	c := NewGenerationCollector(registry)
	ctx := context.Background()

	registry.Add("cache_hit", 4)
	first, err := c.BeginBoundary(ctx, "run-1", 9)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	registry.Add("cache_hit", 6)

	second, err := c.BeginBoundary(ctx, "run-1", 9)
	if err != nil {
		t.Fatalf("replayed BeginBoundary: %v", err)
	}
	if second.Handle != first.Handle || !second.At.Equal(first.At) || second.Committed != first.Committed {
		t.Fatalf("replay returned %+v, want %+v", second, first)
	}
	// A replay that rotated again would have stolen the six counts recorded
	// since the first call.
	if got := countOf(collectFrozen(t, c, first.Handle), "cache_hit"); got != 4 {
		t.Fatalf("replayed boundary froze cache_hit = %d, want 4", got)
	}
	if current := registry.Snapshot(); len(current) != 1 || current[0].Count != 6 {
		t.Fatalf("open generation = %#v, want cache_hit=6", current)
	}

	final, err := c.Freeze(ctx, "run-1", 9)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if final.Handle == first.Handle {
		t.Fatal("Freeze must close its own generation")
	}
	replayFinal, err := c.Freeze(ctx, "run-1", 9)
	if err != nil {
		t.Fatalf("replayed Freeze: %v", err)
	}
	if replayFinal.Handle != final.Handle || !replayFinal.At.Equal(final.At) {
		t.Fatalf("replayed Freeze returned %+v, want %+v", replayFinal, final)
	}
}

func TestCountersGenerationNameMatchesSection(t *testing.T) {
	c := NewGenerationCollector(nil)
	if c.Name() != SectionName {
		t.Fatalf("Name() = %q, want %q", c.Name(), SectionName)
	}
	if c.registry != Default {
		t.Fatal("a nil registry must fall back to the package default")
	}
}

func TestCountersGenerationRejectsStaleEpoch(t *testing.T) {
	c := NewGenerationCollector(NewRegistry())
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

func TestCountersGenerationReleaseIsIdempotent(t *testing.T) {
	registry := NewRegistry()
	c := NewGenerationCollector(registry)
	ctx := context.Background()

	registry.Add("cache_hit", 1)
	res, err := c.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
	if got := countOf(collectFrozen(t, c, res.Handle), "cache_hit"); got != 1 {
		t.Fatalf("cache_hit = %d, want 1", got)
	}

	c.Release(res.Handle)
	c.Release(res.Handle)
	c.Release(res.Handle)

	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrHandleReleased) {
		t.Fatalf("Collect after Release = %v, want ErrHandleReleased", err)
	}
	if err := c.Drain(ctx, res.Handle); err != nil {
		t.Fatalf("Drain after Release: %v", err)
	}
	if _, err := c.Collect(res.Handle); !errors.Is(err, ErrHandleReleased) {
		t.Fatalf("Release must stay released after a late Drain, got %v", err)
	}
}

func TestCountersGenerationRejectsForeignHandle(t *testing.T) {
	registry := NewRegistry()
	c := NewGenerationCollector(NewRegistry())
	other := NewGenerationCollector(registry)
	ctx := context.Background()

	registry.Add("cache_hit", 3)
	res, err := other.BeginBoundary(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("BeginBoundary: %v", err)
	}
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
	if got := countOf(collectFrozen(t, other, res.Handle), "cache_hit"); got != 3 {
		t.Fatalf("foreign Release freed another collector's generation: cache_hit = %d, want 3", got)
	}
}

// TestCountersGenerationBoundaryLosesNoCount is the reason the boundary swaps
// the map instead of calling Snapshot followed by Reset: an increment landing
// between those two calls would vanish.
func TestCountersGenerationBoundaryLosesNoCount(t *testing.T) {
	registry := NewRegistry()
	c := NewGenerationCollector(registry)
	ctx := context.Background()

	const writers = 8
	var (
		added atomic.Int64
		wg    sync.WaitGroup
		stop  = make(chan struct{})
	)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				registry.Add("cache_hit", 1)
				added.Add(1)
			}
		}()
	}

	var frozen int64
	for epoch := runctl.Epoch(1); epoch <= 200; epoch++ {
		res, err := c.BeginBoundary(ctx, "run-1", epoch)
		if err != nil {
			t.Fatalf("BeginBoundary: %v", err)
		}
		frozen += countOf(collectFrozen(t, c, res.Handle), "cache_hit")
		c.Release(res.Handle)
	}
	close(stop)
	wg.Wait()

	res, err := c.BeginBoundary(ctx, "run-1", 201)
	if err != nil {
		t.Fatalf("final BeginBoundary: %v", err)
	}
	frozen += countOf(collectFrozen(t, c, res.Handle), "cache_hit")
	if want := added.Load(); frozen != want {
		t.Fatalf("counts across generations = %d, want %d", frozen, want)
	}
}
