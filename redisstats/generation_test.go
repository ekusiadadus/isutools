package redisstats

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

func collectRedis(t *testing.T, collector *GenerationCollector, handle runctl.GenerationHandle) Frozen {
	t.Helper()
	if err := collector.Drain(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	value, err := collector.Collect(handle)
	if err != nil {
		t.Fatal(err)
	}
	return value.(Frozen)
}

func TestGenerationSeparatesRunInterval(t *testing.T) {
	registry := NewRegistry(10)
	collector := NewGenerationCollector(registry)
	registry.Observe("PING", time.Millisecond, nil)
	start, _ := collector.BeginBoundary(context.Background(), "run", 1)
	registry.Observe("GET secret", 2*time.Millisecond, nil)
	finish, _ := collector.Freeze(context.Background(), "run", 1)
	registry.Observe("SET secret value", 3*time.Millisecond, nil)
	if got := collectRedis(t, collector, start.Handle); len(got.Entries) != 1 || got.Entries[0].Command != "PING" {
		t.Fatalf("pre-run = %#v", got)
	}
	if got := collectRedis(t, collector, finish.Handle); len(got.Entries) != 1 || got.Entries[0].Command != "GET" {
		t.Fatalf("run = %#v", got)
	}
	if got := registry.Snapshot(); len(got) != 1 || got[0].Command != "SET" {
		t.Fatalf("post-run = %#v", got)
	}
}

func TestGenerationLifecycleErrors(t *testing.T) {
	c := NewGenerationCollector(NewRegistry(1))
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
	other := NewGenerationCollector(NewRegistry(1))
	if err := other.Drain(context.Background(), result.Handle); !errors.Is(err, ErrForeignHandle) {
		t.Fatalf("foreign = %v", err)
	}
}
