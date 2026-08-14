package flowstats

import (
	"context"
	"errors"
	"testing"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

func collectFlow(t *testing.T, collector *GenerationCollector, handle runctl.GenerationHandle) Snapshot {
	t.Helper()
	if err := collector.Drain(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	value, err := collector.Collect(handle)
	if err != nil {
		t.Fatal(err)
	}
	return value.(Frozen).Snapshot
}

func TestGenerationSeparatesRunInterval(t *testing.T) {
	registry := NewRegistry()
	collector := NewGenerationCollector(registry)
	registry.Observe("before", "", "GET", "/before")
	start, _ := collector.BeginBoundary(context.Background(), "run", 1)
	registry.Observe("run", "browse", "GET", "/")
	registry.Observe("run", "browse", "GET", "/posts")
	finish, _ := collector.Freeze(context.Background(), "run", 1)
	registry.Observe("after", "", "GET", "/after")
	if got := collectFlow(t, collector, start.Handle); len(got.Stories) != 0 {
		t.Fatalf("pre-run = %#v", got)
	}
	if got := collectFlow(t, collector, finish.Handle); len(got.Stories) != 1 || len(got.Flows) != 1 {
		t.Fatalf("run = %#v", got)
	}
	if got := registry.Snapshot(); len(got.Flows) != 0 {
		t.Fatalf("post-run = %#v", got)
	}
}

func TestGenerationLifecycleErrors(t *testing.T) {
	c := NewGenerationCollector(NewRegistry())
	result, _ := c.BeginBoundary(context.Background(), "run", 2)
	if _, err := c.Collect(result.Handle); !errors.Is(err, ErrNotDrained) {
		t.Fatalf("collect before drain = %v", err)
	}
	if _, err := c.BeginBoundary(context.Background(), "old", 1); !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("stale = %v", err)
	}
	c.Release(result.Handle)
	if _, err := c.Collect(result.Handle); !errors.Is(err, ErrHandleReleased) {
		t.Fatalf("released = %v", err)
	}
	other := NewGenerationCollector(NewRegistry())
	if err := other.Drain(context.Background(), result.Handle); !errors.Is(err, ErrForeignHandle) {
		t.Fatalf("foreign = %v", err)
	}
}
