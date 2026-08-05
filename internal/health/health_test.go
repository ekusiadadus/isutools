package health

import (
	"sync"
	"testing"
)

func TestRegistrySnapshotIsSortedAndPartial(t *testing.T) {
	r := NewRegistry()
	r.Set("sql", StatusOK, "")
	r.Set("accesslog", StatusDegraded, "malformed line")
	r.AddDropped("accesslog", 2)

	got, partial := r.Snapshot()
	if !partial {
		t.Fatal("partial = false, want true for degraded collector")
	}
	if len(got) != 2 {
		t.Fatalf("len(snapshot) = %d, want 2", len(got))
	}
	if got[0].Collector != "accesslog" || got[1].Collector != "sql" {
		t.Fatalf("collectors = %q, %q; want sorted", got[0].Collector, got[1].Collector)
	}
	if got[0].Dropped != 2 || got[0].Message != "malformed line" {
		t.Fatalf("accesslog health = %#v", got[0])
	}
}

func TestDisabledDoesNotMakeSnapshotPartial(t *testing.T) {
	r := NewRegistry()
	r.Set("proc", StatusDisabled, "not configured")
	got, partial := r.Snapshot()
	if partial {
		t.Fatal("disabled collector must not make an intentional configuration partial")
	}
	if len(got) != 1 || got[0].Status != StatusDisabled {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestInfoDoesNotMakeSnapshotPartial(t *testing.T) {
	r := NewRegistry()
	r.Set("dbpool", StatusInfo, "WatchDBPool was not called")
	got, partial := r.Snapshot()
	if partial {
		t.Fatal("informational health must not make the measurement partial")
	}
	if len(got) != 1 || got[0].Status != StatusInfo {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestDroppedMakesOKCollectorPartialAndCanReset(t *testing.T) {
	r := NewRegistry()
	r.Set("http", StatusOK, "")
	r.AddDropped("http", 3)
	got, partial := r.Snapshot()
	if !partial || got[0].Dropped != 3 {
		t.Fatalf("snapshot = %#v, partial = %v", got, partial)
	}

	r.ResetDropped()
	got, partial = r.Snapshot()
	if partial || got[0].Dropped != 0 {
		t.Fatalf("after reset snapshot = %#v, partial = %v", got, partial)
	}
}

func TestRegistryConcurrentUpdates(t *testing.T) {
	r := NewRegistry()
	const workers = 16
	const increments = 100
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < increments; j++ {
				r.AddDropped("sql", 1)
			}
		}()
	}
	wg.Wait()
	got, partial := r.Snapshot()
	if !partial || len(got) != 1 || got[0].Dropped != workers*increments {
		t.Fatalf("snapshot = %#v, partial = %v", got, partial)
	}
}
