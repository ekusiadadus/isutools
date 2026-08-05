package sqlstats

import (
	"context"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/generation"
	"github.com/ekusiadadus/isutools/internal/runctl"
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

// TestRotateDrainBudgetIsTheSharedDrainBudget pins the rotation's bound to the
// one authority for it. The compatibility shim under Rotate used to carry its
// own invented 30s constant — three times the run controller's drain budget —
// so a wedged query was worth waiting three times as long through /reset as
// through a run boundary. Asserting the identity rather than a duration is what
// makes a future divergence fail here.
func TestRotateDrainBudgetIsTheSharedDrainBudget(t *testing.T) {
	if RotateDrainBudget != runctl.DrainBudget {
		t.Fatalf("RotateDrainBudget = %v, want runctl.DrainBudget (%v)", RotateDrainBudget, runctl.DrainBudget)
	}
	if generation.DefaultCompatWait != runctl.DrainBudget {
		t.Fatalf("generation.DefaultCompatWait = %v, want runctl.DrainBudget (%v)",
			generation.DefaultCompatWait, runctl.DrainBudget)
	}
}

// TestRotateReportsCutShortWhenAQueryNeverReturns is the regression test for
// the silent truncation: the shim discarded its wait outcome and Frozen carried
// no flag, so a SQL table missing the rows of a still-running query reached
// /reset looking exactly like a complete one.
func TestRotateReportsCutShortWhenAQueryNeverReturns(t *testing.T) {
	s := NewStore(100)
	s.SetRotateDrainBudget(20 * time.Millisecond)

	// A measurement that is never finished models a query that never returns.
	stuck := s.begin()
	t.Cleanup(func() { s.discard(stuck) })
	s.Observe("SELECT done", time.Millisecond)

	frozen := s.Rotate()
	if !frozen.CutShort {
		t.Fatalf("frozen = %#v, want CutShort=true: a query was still running when the rotation gave up", frozen)
	}
	if frozen.Generation != 1 {
		t.Fatalf("frozen generation = %d, want 1", frozen.Generation)
	}
	// Fail-open: the partial table is still returned, and the rotation still
	// happened, so later queries are counted in the new generation.
	if len(frozen.Entries) != 1 || frozen.Entries[0].Key != "SELECT done" {
		t.Fatalf("frozen entries = %#v, want the one query that did finish", frozen.Entries)
	}
	if got := s.CurrentGeneration(); got != 2 {
		t.Fatalf("current generation = %d, want 2", got)
	}
}

// TestRotateDoesNotReportCutShortWhenEveryQueryFinished is the other half of
// the contract: a healthy rotation must never be labelled partial, or the flag
// is worthless to the caller deciding whether to mark the SQL section partial.
func TestRotateDoesNotReportCutShortWhenEveryQueryFinished(t *testing.T) {
	s := NewStore(100)
	s.SetRotateDrainBudget(20 * time.Millisecond)
	s.Observe("SELECT 1", time.Millisecond)

	frozen := s.Rotate()
	if frozen.CutShort {
		t.Fatalf("frozen = %#v, want CutShort=false: no query was in flight", frozen)
	}
	if frozen.Generation != 1 || len(frozen.Entries) != 1 {
		t.Fatalf("frozen = %#v, want generation 1 with one entry", frozen)
	}
	// An empty generation is not a truncated one either.
	if empty := s.Rotate(); empty.CutShort {
		t.Fatalf("frozen = %#v, want CutShort=false for an empty generation", empty)
	}
}

// TestRotateContextHonoursTheCallersDeadline pins the other half of the defect:
// the shim always waited on context.Background(), so /reset kept the reset lock
// and the operation slot for the whole budget even after the request that asked
// for the rotation was gone, head-of-line-blocking every other admin endpoint.
func TestRotateContextHonoursTheCallersDeadline(t *testing.T) {
	s := NewStore(100)
	// Left at the production budget on purpose: the caller's context, not the
	// store's budget, is what has to end this wait.
	stuck := s.begin()
	t.Cleanup(func() { s.discard(stuck) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan Frozen, 1)
	start := time.Now()
	go func() { result <- s.RotateContext(ctx) }()
	select {
	case frozen := <-result:
		if elapsed := time.Since(start); elapsed >= RotateDrainBudget {
			t.Fatalf("rotation took %v, the caller's 20ms deadline was ignored", elapsed)
		}
		if !frozen.CutShort {
			t.Fatalf("frozen = %#v, want CutShort=true", frozen)
		}
		if frozen.Generation != 1 {
			t.Fatalf("frozen generation = %d, want 1", frozen.Generation)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RotateContext ignored the caller's deadline: still waiting after 2s")
	}

	// A nil context is a caller mistake measurement must survive rather than
	// panic on: it falls back to the budget alone.
	s.SetRotateDrainBudget(20 * time.Millisecond)
	if frozen := s.RotateContext(nil); frozen.Generation != 2 {
		t.Fatalf("frozen = %#v, want generation 2", frozen)
	}
}

// TestSetRotateDrainBudgetRestoresTheDefault pins the documented meaning of a
// non-positive budget: fall back to RotateDrainBudget rather than to zero, so a
// caller cannot accidentally turn every rotation into an instant cut-short one.
func TestSetRotateDrainBudgetRestoresTheDefault(t *testing.T) {
	s := NewStore(100)
	s.SetRotateDrainBudget(time.Millisecond)
	s.SetRotateDrainBudget(0)

	// With a zero budget meaning "no wait", this rotation would report a
	// cut-short generation despite there being nothing in flight to wait for.
	done := s.begin()
	release := make(chan struct{})
	go func() {
		<-release
		s.finish(done, "SELECT slow", time.Millisecond, false)
	}()
	result := make(chan Frozen, 1)
	go func() { result <- s.Rotate() }()
	close(release)
	select {
	case frozen := <-result:
		if frozen.CutShort {
			t.Fatalf("frozen = %#v, want CutShort=false: a zero budget must restore the default, not disable the wait", frozen)
		}
		if len(frozen.Entries) != 1 || frozen.Entries[0].Key != "SELECT slow" {
			t.Fatalf("frozen entries = %#v, want the query that finished inside the budget", frozen.Entries)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not return after the query finished")
	}
}
