package dbpool

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// TestCaptureIdempotent pins the (runID, epoch, phase) idempotency the
// Controller relies on when it retries a boundary: the replay must be the same
// value, timestamp included, and must not sample the pool again.
func TestCaptureIdempotent(t *testing.T) {
	c, clock := newTestCollector()
	script := newScript(
		sql.DBStats{WaitCount: 1},
		sql.DBStats{WaitCount: 2},
	)
	if err := c.watchStats("db1", appDisplay, script.stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}

	first := mustCapture(t, c.CaptureBaseline, "run-1", 1)
	clock.advance(time.Second)
	second := mustCapture(t, c.CaptureBaseline, "run-1", 1)

	if !first.At.Equal(second.At) {
		t.Fatalf("replayed At = %v, want %v", second.At, first.At)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("replayed SampleResult = %+v, want %+v", second, first)
	}
	if got := script.callCount(); got != 1 {
		t.Fatalf("sampler calls = %d, want 1", got)
	}

	firstFinal := mustCapture(t, c.CaptureFinal, "run-1", 1)
	clock.advance(time.Second)
	secondFinal := mustCapture(t, c.CaptureFinal, "run-1", 1)
	if !reflect.DeepEqual(firstFinal, secondFinal) {
		t.Fatalf("replayed final = %+v, want %+v", secondFinal, firstFinal)
	}
	if got := script.callCount(); got != 2 {
		t.Fatalf("sampler calls = %d, want 2", got)
	}
}

// TestCaptureStaleEpoch checks the fence: a boundary belonging to a run the
// Controller has already moved past is refused rather than silently
// overwriting the current one.
func TestCaptureStaleEpoch(t *testing.T) {
	c, _ := newTestCollector()
	if err := c.watchStats("db1", appDisplay, newScript().stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}
	mustCapture(t, c.CaptureBaseline, "run-2", 7)

	tests := []struct {
		name    string
		capture captureFunc
	}{
		{name: "baseline", capture: c.CaptureBaseline},
		{name: "final", capture: c.CaptureFinal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.capture(context.Background(), "run-1", 6)
			if !errors.Is(err, runctl.ErrStaleEpoch) {
				t.Fatalf("capture at a stale epoch = %v, want ErrStaleEpoch", err)
			}
			if res.Committed {
				t.Fatal("Committed = true, want false")
			}
			if res.Handle.Zero() {
				t.Fatal("handle is the zero value; the Controller cannot attribute it")
			}
			if res.Handle.Collector != Name || res.Handle.RunID != "run-1" {
				t.Fatalf("handle = %+v, want it addressed to run-1/%s", res.Handle, Name)
			}
		})
	}
}

// TestCaptureCommitted covers the Committed predicate on both paths: success
// commits, and an expired context fails without returning a zero value.
func TestCaptureCommitted(t *testing.T) {
	c, _ := newTestCollector()
	if err := c.watchStats("db1", appDisplay, newScript().stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil || !res.Committed {
		t.Fatalf("CaptureBaseline = (%+v, %v), want committed and nil", res, err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = c.CaptureFinal(cancelled, "run-1", 1)
	if err == nil {
		t.Fatal("CaptureFinal on a cancelled context = nil error, want one")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
	if res.Committed {
		t.Fatal("Committed = true, want false")
	}
	if res.Handle.Zero() {
		t.Fatal("handle is the zero value; the contract forbids it")
	}
	if res.At.IsZero() {
		t.Fatal("At is the zero time; the Controller records it in the boundary window")
	}
}

func TestEmptyWatchSetExplainsMissingSection(t *testing.T) {
	c, _ := newTestCollector()
	mustCapture(t, c.CaptureBaseline, "run-empty", 1)

	if notes := c.Notes(); !hasNote(notes, HealthNotRegistered) {
		t.Fatalf("notes = %v, want %s", notes, HealthNotRegistered)
	}
}

// TestCollectPerformsNoSampling is the purity conformance test: the report is
// derived from the two frozen handles, so the pool must not be read again
// while Collect runs. A snapshot is built after the run has closed, and a
// value read at that point would describe traffic the run never saw.
func TestCollectPerformsNoSampling(t *testing.T) {
	c, _ := newTestCollector()
	script := newScript(
		sql.DBStats{WaitCount: 1, MaxOpenConnections: 4},
		sql.DBStats{WaitCount: 9, MaxOpenConnections: 4},
	)
	if err := c.watchStats("db1", appDisplay, script.stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}
	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)

	before := script.callCount()
	entries := mustCollect(t, c, base, final)
	if after := script.callCount(); after != before {
		t.Fatalf("sampler calls during Collect: %d -> %d, want no change", before, after)
	}
	if got := entryByID(t, entries, "db1").WaitCount; got != 8 {
		t.Fatalf("WaitCount = %d, want 8", got)
	}

	// Repeating Collect from the same handles repeats the same report, even
	// after the live pool has moved on.
	script.stats()
	again := mustCollect(t, c, base, final)
	if !reflect.DeepEqual(entries, again) {
		t.Fatalf("second Collect = %+v, want %+v", again, entries)
	}
}

// TestCollectRejectsForeignSample checks that a handle carrying somebody
// else's sample costs one section and not the process.
func TestCollectRejectsForeignSample(t *testing.T) {
	c, _ := newTestCollector()
	good := runctl.NewBaselineHandle("run-1", 1, Name, runctl.PhaseStartBaseline, time.Now(), Sample{})
	foreign := runctl.NewBaselineHandle("run-1", 1, Name, runctl.PhaseFinishFinal, time.Now(), map[string]sql.DBStats{})

	if _, err := c.Collect(foreign, good); err == nil {
		t.Fatal("Collect with a foreign baseline sample = nil error, want one")
	}
	if _, err := c.Collect(good, foreign); err == nil {
		t.Fatal("Collect with a foreign final sample = nil error, want one")
	}
	if _, err := c.Collect(runctl.BaselineHandle{}, good); err == nil {
		t.Fatal("Collect with a zero handle = nil error, want one")
	}
}

// TestReleaseIdempotent checks that releasing twice, releasing a foreign
// handle and releasing a zero handle are all no-ops, and that a released
// handle still yields its report.
func TestReleaseIdempotent(t *testing.T) {
	c, _ := newTestCollector()
	if err := c.watchStats("db1", appDisplay, newScript(sql.DBStats{WaitCount: 3}).stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}
	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)

	c.Release(base.Handle)
	c.Release(base.Handle)
	c.Release(final.Handle)
	c.Release(final.Handle)
	c.Release(runctl.BaselineHandle{})
	c.Release(runctl.NewBaselineHandle("run-1", 1, "hoststats", runctl.PhaseStartBaseline, time.Now(), nil))
	c.Release(runctl.NewBaselineHandle("run-9", 9, Name, runctl.PhaseStartBaseline, time.Now(), Sample{}))

	entries := mustCollect(t, c, base, final)
	if len(entries) != 1 {
		t.Fatalf("entries after Release = %+v, want one", entries)
	}
}

// TestCaptureFinalWithoutBaseline covers the defensive path where a closing
// boundary arrives for an epoch that never opened one: it is well formed, and
// it reports nothing, because the baseline decides who took part.
func TestCaptureFinalWithoutBaseline(t *testing.T) {
	c, _ := newTestCollector()
	if err := c.watchStats("db1", appDisplay, newScript(sql.DBStats{WaitCount: 5}).stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)
	sample, err := sampleOf(final.Handle, "final")
	if err != nil {
		t.Fatalf("sampleOf = %v, want nil", err)
	}
	if len(sample) != 1 {
		t.Fatalf("final sample = %+v, want the currently watched pool", sample)
	}
	entries := mustCollect(t, c, runctl.SampleResult{
		Handle: runctl.NewBaselineHandle("run-1", 1, Name, runctl.PhaseStartBaseline, time.Now(), Sample{}),
	}, final)
	if len(entries) != 0 {
		t.Fatalf("entries = %+v, want none", entries)
	}
}

// TestCaptureNewRunResetsCache checks that a newer epoch starts a fresh run
// rather than replaying the previous one's samples.
func TestCaptureNewRunResetsCache(t *testing.T) {
	c, clock := newTestCollector()
	script := newScript(
		sql.DBStats{WaitCount: 1},
		sql.DBStats{WaitCount: 2},
		sql.DBStats{WaitCount: 10},
		sql.DBStats{WaitCount: 40},
	)
	if err := c.watchStats("db1", appDisplay, script.stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}

	first := collectEntries(t, c, "run-1", 1)
	if got := entryByID(t, first, "db1").WaitCount; got != 1 {
		t.Fatalf("run-1 WaitCount = %d, want 1", got)
	}
	clock.advance(time.Minute)
	second := collectEntries(t, c, "run-2", 2)
	if got := entryByID(t, second, "db1").WaitCount; got != 30 {
		t.Fatalf("run-2 WaitCount = %d, want 30", got)
	}
	if entryByID(t, second, "db1").BaselineAt.Equal(entryByID(t, first, "db1").BaselineAt) {
		t.Fatal("run-2 reused run-1's baseline timestamp")
	}
}
