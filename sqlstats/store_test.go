package sqlstats

import (
	"testing"
	"time"
)

func TestStoreObserveAndSnapshot(t *testing.T) {
	s := NewStore(100)
	s.Observe("SELECT 1", 3*time.Millisecond)
	got := s.Snapshot()
	if len(got) != 1 || got[0].Key != "SELECT 1" || got[0].Count != 1 {
		t.Fatalf("snapshot = %#v", got)
	}
	if s.CurrentGeneration() != 1 {
		t.Fatalf("generation = %d, want 1", s.CurrentGeneration())
	}
}

func TestStoreObserveResultCountsErrors(t *testing.T) {
	s := NewStore(100)
	s.ObserveResult("SELECT 1", time.Millisecond, true)
	got := s.Snapshot()
	if len(got) != 1 || got[0].ErrorCount != 1 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestStoreRotationKeepsInflightObservationInOldGeneration(t *testing.T) {
	s := NewStore(100)
	old := s.begin()

	result := make(chan Frozen, 1)
	go func() { result <- s.Rotate() }()
	deadline := time.Now().Add(time.Second)
	for s.CurrentGeneration() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.CurrentGeneration() != 2 {
		t.Fatal("rotation did not publish generation 2")
	}

	s.Observe("SELECT new", 2*time.Millisecond)
	select {
	case <-result:
		t.Fatal("rotation returned before old observation completed")
	default:
	}

	s.finish(old, "SELECT old", time.Millisecond, false)
	frozen := <-result
	if frozen.Generation != 1 || len(frozen.Entries) != 1 || frozen.Entries[0].Key != "SELECT old" {
		t.Fatalf("frozen = %#v", frozen)
	}
	current := s.Snapshot()
	if len(current) != 1 || current[0].Key != "SELECT new" {
		t.Fatalf("current = %#v", current)
	}
}

func TestStoreFinishIsIdempotentAndReleasesLease(t *testing.T) {
	s := NewStore(100)
	measurement := s.begin()
	s.finish(measurement, "SELECT 1", time.Millisecond, false)
	// A duplicate finish must not hold or corrupt the generation.
	s.finish(measurement, "SELECT 1", time.Millisecond, false)
	frozen := s.Rotate()
	if frozen.Generation != 1 || len(frozen.Entries) != 1 || frozen.Entries[0].Count != 1 {
		t.Fatalf("frozen = %#v", frozen)
	}
}
