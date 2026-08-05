package dbpool

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// TestCollectDelta checks every reported field on a four-shard setup: point
// values come from the closing sample, cumulative counters come out as
// interval deltas, and the rows are ordered by TargetID.
func TestCollectDelta(t *testing.T) {
	c, clock := newTestCollector()
	shards := []struct {
		id    string
		base  sql.DBStats
		final sql.DBStats
	}{
		{
			id: "shard-a",
			base: sql.DBStats{
				MaxOpenConnections: 10, OpenConnections: 2, InUse: 1, Idle: 1,
				WaitCount: 5, WaitDuration: 500 * time.Millisecond,
				MaxIdleClosed: 3, MaxIdleTimeClosed: 2, MaxLifetimeClosed: 1,
			},
			final: sql.DBStats{
				MaxOpenConnections: 10, OpenConnections: 10, InUse: 9, Idle: 1,
				WaitCount: 125, WaitDuration: 12500 * time.Millisecond,
				MaxIdleClosed: 33, MaxIdleTimeClosed: 12, MaxLifetimeClosed: 7,
			},
		},
		{
			id:    "shard-b",
			base:  sql.DBStats{MaxOpenConnections: 4, OpenConnections: 1, InUse: 0, Idle: 1},
			final: sql.DBStats{MaxOpenConnections: 4, OpenConnections: 4, InUse: 4, Idle: 0, WaitCount: 2, WaitDuration: time.Second},
		},
		{
			id:    "shard-c",
			base:  sql.DBStats{MaxOpenConnections: 0, OpenConnections: 3, InUse: 3, Idle: 0, MaxLifetimeClosed: 100},
			final: sql.DBStats{MaxOpenConnections: 0, OpenConnections: 6, InUse: 2, Idle: 4, MaxLifetimeClosed: 140},
		},
		{
			id:    "shard-d",
			base:  sql.DBStats{MaxOpenConnections: 2, OpenConnections: 2, InUse: 2, Idle: 0, MaxIdleClosed: 9, MaxIdleTimeClosed: 4},
			final: sql.DBStats{MaxOpenConnections: 2, OpenConnections: 2, InUse: 0, Idle: 2, MaxIdleClosed: 11, MaxIdleTimeClosed: 4},
		},
	}
	for _, s := range shards {
		if err := c.watchStats(s.id, "tcp(127.0.0.1:3306)/"+s.id, newScript(s.base, s.final).stats); err != nil {
			t.Fatalf("watchStats(%q) = %v, want nil", s.id, err)
		}
	}

	baselineAt := clock.Now()
	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)
	clock.advance(90 * time.Second)
	finalAt := clock.Now()
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)
	entries := mustCollect(t, c, base, final)

	if got, want := strings.Join(targetIDs(entries), ","), "shard-a,shard-b,shard-c,shard-d"; got != want {
		t.Fatalf("report order = %s, want %s", got, want)
	}

	want := []Entry{
		{
			TargetID: "shard-a", Display: "tcp(127.0.0.1:3306)/shard-a",
			MaxOpen: 10, Open: 10, InUse: 9, Idle: 1,
			WaitCount: 120, WaitDuration: 12 * time.Second,
			MaxIdleClosed: 30, MaxIdleTimeClosed: 10, MaxLifetimeClosed: 6,
		},
		{
			TargetID: "shard-b", Display: "tcp(127.0.0.1:3306)/shard-b",
			MaxOpen: 4, Open: 4, InUse: 4, Idle: 0,
			WaitCount: 2, WaitDuration: time.Second,
		},
		{
			TargetID: "shard-c", Display: "tcp(127.0.0.1:3306)/shard-c",
			MaxOpen: 0, Open: 6, InUse: 2, Idle: 4,
			MaxLifetimeClosed: 40,
		},
		{
			TargetID: "shard-d", Display: "tcp(127.0.0.1:3306)/shard-d",
			MaxOpen: 2, Open: 2, InUse: 0, Idle: 2,
			MaxIdleClosed: 2,
		},
	}
	for i, w := range want {
		w.BaselineAt, w.FinalAt = baselineAt, finalAt
		if entries[i] != w {
			t.Fatalf("entry %d =\n%+v\nwant\n%+v", i, entries[i], w)
		}
		if entries[i].Interval() != 90*time.Second {
			t.Fatalf("entry %d interval = %v, want 90s", i, entries[i].Interval())
		}
	}

	if got := entries[0].AverageWait(); got != 100*time.Millisecond {
		t.Fatalf("shard-a AverageWait = %v, want 100ms", got)
	}
	if got := entries[2].AverageWait(); got != 0 {
		t.Fatalf("shard-c AverageWait = %v, want 0 when nobody waited", got)
	}
}

// TestCollectCounterRewind covers a pool recreated under the same id: a delta
// across that boundary would be nonsense, so the entry reports the final
// absolute values and says so.
func TestCollectCounterRewind(t *testing.T) {
	tests := []struct {
		name  string
		base  sql.DBStats
		final sql.DBStats
	}{
		{name: "wait count", base: sql.DBStats{WaitCount: 50}, final: sql.DBStats{WaitCount: 3}},
		{name: "wait duration", base: sql.DBStats{WaitDuration: time.Minute}, final: sql.DBStats{WaitDuration: time.Second}},
		{name: "max idle closed", base: sql.DBStats{MaxIdleClosed: 20}, final: sql.DBStats{MaxIdleClosed: 1}},
		{name: "max idle time closed", base: sql.DBStats{MaxIdleTimeClosed: 20}, final: sql.DBStats{MaxIdleTimeClosed: 1}},
		{name: "max lifetime closed", base: sql.DBStats{MaxLifetimeClosed: 20}, final: sql.DBStats{MaxLifetimeClosed: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCollector()
			if err := c.watchStats("db1", appDisplay, newScript(tc.base, tc.final).stats); err != nil {
				t.Fatalf("watchStats = %v, want nil", err)
			}
			entry := entryByID(t, collectEntries(t, c, "run-1", 1), "db1")
			if !entry.Partial || entry.Code != CodeCounterRewind {
				t.Fatalf("entry = %+v, want partial with code %q", entry, CodeCounterRewind)
			}
			// The displayed numbers are the final absolute values, not deltas.
			if entry.WaitCount != tc.final.WaitCount ||
				entry.WaitDuration != tc.final.WaitDuration ||
				entry.MaxIdleClosed != tc.final.MaxIdleClosed ||
				entry.MaxIdleTimeClosed != tc.final.MaxIdleTimeClosed ||
				entry.MaxLifetimeClosed != tc.final.MaxLifetimeClosed {
				t.Fatalf("entry = %+v, want the final absolute values %+v", entry, tc.final)
			}
		})
	}
}

// TestWatchMidRunNotInCurrentRun pins deferred activation: a pool that joined
// after the opening boundary reports nothing for that run, the pools that were
// there report a full interval, and the reason is recorded.
func TestWatchMidRunNotInCurrentRun(t *testing.T) {
	c, clock := newTestCollector()
	early := newScript(sql.DBStats{WaitCount: 1}, sql.DBStats{WaitCount: 11})
	late := newScript(sql.DBStats{WaitCount: 100}, sql.DBStats{WaitCount: 400})
	if err := c.watchStats("db1", appDisplay, early.stats); err != nil {
		t.Fatalf("watchStats(db1) = %v, want nil", err)
	}

	baselineAt := clock.Now()
	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)
	clock.advance(30 * time.Second)
	if err := c.watchStats("db2", appDisplay, late.stats); err != nil {
		t.Fatalf("watchStats(db2) mid-run = %v, want nil", err)
	}
	clock.advance(30 * time.Second)
	finalAt := clock.Now()
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)
	entries := mustCollect(t, c, base, final)

	if got := targetIDs(entries); len(got) != 1 || got[0] != "db1" {
		t.Fatalf("entries = %v, want only db1", got)
	}
	db1 := entryByID(t, entries, "db1")
	if db1.Partial || db1.Code != "" {
		t.Fatalf("db1 = %+v, want a complete entry", db1)
	}
	if !db1.BaselineAt.Equal(baselineAt) || !db1.FinalAt.Equal(finalAt) {
		t.Fatalf("db1 interval = %v..%v, want %v..%v", db1.BaselineAt, db1.FinalAt, baselineAt, finalAt)
	}
	if late.callCount() != 0 {
		t.Fatalf("the mid-run pool was sampled %d times, want 0", late.callCount())
	}
	if !hasNote(c.Notes(), HealthRegisteredMidRun) {
		t.Fatalf("notes = %v, want one mentioning %s", c.Notes(), HealthRegisteredMidRun)
	}
}

// TestWatchMidRunIncludedInNextRun is the other half of deferred activation:
// the pool is measured from the next reset onwards.
func TestWatchMidRunIncludedInNextRun(t *testing.T) {
	c, _ := newTestCollector()
	if err := c.watchStats("db1", appDisplay, newScript(sql.DBStats{}).stats); err != nil {
		t.Fatalf("watchStats(db1) = %v, want nil", err)
	}
	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)
	db2 := newScript(
		sql.DBStats{WaitCount: 100},
		sql.DBStats{WaitCount: 175},
	)
	if err := c.watchStats("db2", appDisplay, db2.stats); err != nil {
		t.Fatalf("watchStats(db2) mid-run = %v, want nil", err)
	}
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)
	if got := targetIDs(mustCollect(t, c, base, final)); len(got) != 1 {
		t.Fatalf("run-1 entries = %v, want only db1", got)
	}
	if db2.callCount() != 0 {
		t.Fatalf("db2 sampled %d times during run-1, want 0", db2.callCount())
	}

	entries := collectEntries(t, c, "run-2", 2)
	if got, want := strings.Join(targetIDs(entries), ","), "db1,db2"; got != want {
		t.Fatalf("run-2 entries = %s, want %s", got, want)
	}
	if got := entryByID(t, entries, "db2").WaitCount; got != 75 {
		t.Fatalf("db2 WaitCount = %d, want 75", got)
	}
}

// TestUnwatchMidRunTruncatedEntry pins the farewell sample: the pool that left
// mid-run still has a row, its interval ends where the pool did, and the entry
// declares that itself instead of leaving the reader to assume it covered the
// whole run.
func TestUnwatchMidRunTruncatedEntry(t *testing.T) {
	c, clock := newTestCollector()
	script := newScript(
		sql.DBStats{WaitCount: 4, WaitDuration: time.Second, OpenConnections: 3, MaxOpenConnections: 8},
		sql.DBStats{WaitCount: 24, WaitDuration: 6 * time.Second, OpenConnections: 7, InUse: 5, Idle: 2, MaxOpenConnections: 8},
	)
	if err := c.watchStats("db1", appDisplay, script.stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}
	if err := c.watchStats("db2", appDisplay, newScript(sql.DBStats{}).stats); err != nil {
		t.Fatalf("watchStats(db2) = %v, want nil", err)
	}

	baselineAt := clock.Now()
	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)

	clock.advance(20 * time.Second)
	unwatchedAt := clock.Now()
	if !c.unwatchStats("db1") {
		t.Fatal("unwatchStats(db1) = false, want true")
	}
	clock.advance(40 * time.Second)
	finalAt := clock.Now()
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)
	entries := mustCollect(t, c, base, final)

	if got, want := strings.Join(targetIDs(entries), ","), "db1,db2"; got != want {
		t.Fatalf("entries = %s, want %s", got, want)
	}
	db1 := entryByID(t, entries, "db1")
	if !db1.Partial || db1.Code != CodeUnwatchedMidRun {
		t.Fatalf("db1 = %+v, want partial with code %q", db1, CodeUnwatchedMidRun)
	}
	if !db1.FinalAt.Equal(unwatchedAt) {
		t.Fatalf("db1 FinalAt = %v, want the farewell time %v", db1.FinalAt, unwatchedAt)
	}
	if !db1.FinalAt.Before(finalAt) {
		t.Fatalf("db1 FinalAt = %v, want it strictly before the closing boundary %v", db1.FinalAt, finalAt)
	}
	if !db1.BaselineAt.Equal(baselineAt) {
		t.Fatalf("db1 BaselineAt = %v, want %v", db1.BaselineAt, baselineAt)
	}
	if db1.Interval() != 20*time.Second {
		t.Fatalf("db1 interval = %v, want 20s", db1.Interval())
	}
	// The farewell sample supplies both the deltas and the point values.
	if db1.WaitCount != 20 || db1.WaitDuration != 5*time.Second {
		t.Fatalf("db1 waits = (%d, %v), want (20, 5s)", db1.WaitCount, db1.WaitDuration)
	}
	if db1.Open != 7 || db1.InUse != 5 || db1.Idle != 2 || db1.MaxOpen != 8 {
		t.Fatalf("db1 point values = %+v, want the farewell sample's", db1)
	}
	if got := script.callCount(); got != 2 {
		t.Fatalf("db1 sampled %d times, want 2 (baseline and farewell)", got)
	}
	if db2 := entryByID(t, entries, "db2"); db2.Partial {
		t.Fatalf("db2 = %+v, want an unaffected complete entry", db2)
	}
	if !hasNote(c.Notes(), HealthUnwatchedMidRun) {
		t.Fatalf("notes = %v, want one mentioning %s", c.Notes(), HealthUnwatchedMidRun)
	}
}

// TestUnwatchBeforeRunIsNotReported checks the ordinary case: a pool removed
// before the run started is simply not part of it, with no farewell and no
// note.
func TestUnwatchBeforeRunIsNotReported(t *testing.T) {
	c, _ := newTestCollector()
	script := newScript(sql.DBStats{WaitCount: 7})
	if err := c.watchStats("db1", appDisplay, script.stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}
	if !c.unwatchStats("db1") {
		t.Fatal("unwatchStats = false, want true")
	}
	if c.unwatchStats("db1") {
		t.Fatal("second unwatchStats = true, want false")
	}
	if entries := collectEntries(t, c, "run-1", 1); len(entries) != 0 {
		t.Fatalf("entries = %+v, want none", entries)
	}
	if script.callCount() != 0 {
		t.Fatalf("sampler calls = %d, want 0", script.callCount())
	}
	if notes := c.Notes(); len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
}

// TestFarewellWinsOverReplacementPool checks that re-watching an id mid-run
// does not splice two different pools into one interval: the run keeps the
// farewell, and the replacement is measured from the next run.
func TestFarewellWinsOverReplacementPool(t *testing.T) {
	c, clock := newTestCollector()
	original := newScript(sql.DBStats{WaitCount: 1}, sql.DBStats{WaitCount: 6})
	replacement := newScript(sql.DBStats{WaitCount: 900})
	if err := c.watchStats("db1", appDisplay, original.stats); err != nil {
		t.Fatalf("watchStats = %v, want nil", err)
	}

	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)
	clock.advance(10 * time.Second)
	c.unwatchStats("db1")
	if err := c.watchStats("db1", "tcp(127.0.0.1:3399)/isuconp", replacement.stats); err != nil {
		t.Fatalf("re-watchStats = %v, want nil", err)
	}
	clock.advance(10 * time.Second)
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)

	entry := entryByID(t, mustCollect(t, c, base, final), "db1")
	if entry.WaitCount != 5 {
		t.Fatalf("WaitCount = %d, want 5 from the farewell sample", entry.WaitCount)
	}
	if entry.Code != CodeUnwatchedMidRun {
		t.Fatalf("Code = %q, want %q", entry.Code, CodeUnwatchedMidRun)
	}
	if entry.Display != appDisplay {
		t.Fatalf("Display = %q, want the original pool's %q", entry.Display, appDisplay)
	}
	if replacement.callCount() != 0 {
		t.Fatalf("the replacement pool was sampled %d times, want 0", replacement.callCount())
	}
}

// TestSamplerPanicIsContained checks the fail-open rule at both boundaries: a
// sampler that panics costs its own row, never the measured process.
func TestSamplerPanicIsContained(t *testing.T) {
	t.Run("at the opening boundary", func(t *testing.T) {
		c, _ := newTestCollector()
		exploding := newScript()
		exploding.panicAfter = 0
		if err := c.watchStats("db1", appDisplay, exploding.stats); err != nil {
			t.Fatalf("watchStats = %v, want nil", err)
		}
		if err := c.watchStats("db2", appDisplay, newScript(sql.DBStats{WaitCount: 2}).stats); err != nil {
			t.Fatalf("watchStats(db2) = %v, want nil", err)
		}
		entries := collectEntries(t, c, "run-1", 1)
		if got := targetIDs(entries); len(got) != 1 || got[0] != "db2" {
			t.Fatalf("entries = %v, want only db2", got)
		}
		if !hasNote(c.Notes(), HealthSampleFailed) {
			t.Fatalf("notes = %v, want one mentioning %s", c.Notes(), HealthSampleFailed)
		}
	})

	t.Run("at the closing boundary", func(t *testing.T) {
		c, _ := newTestCollector()
		exploding := newScript(sql.DBStats{WaitCount: 1})
		exploding.panicAfter = 1
		if err := c.watchStats("db1", appDisplay, exploding.stats); err != nil {
			t.Fatalf("watchStats = %v, want nil", err)
		}
		if entries := collectEntries(t, c, "run-1", 1); len(entries) != 0 {
			t.Fatalf("entries = %+v, want none: the interval has no end", entries)
		}
		if !hasNote(c.Notes(), HealthSampleFailed) {
			t.Fatalf("notes = %v, want one mentioning %s", c.Notes(), HealthSampleFailed)
		}
	})

	t.Run("while taking a farewell sample", func(t *testing.T) {
		c, _ := newTestCollector()
		exploding := newScript(sql.DBStats{WaitCount: 1})
		exploding.panicAfter = 1
		if err := c.watchStats("db1", appDisplay, exploding.stats); err != nil {
			t.Fatalf("watchStats = %v, want nil", err)
		}
		base := mustCapture(t, c.CaptureBaseline, "run-1", 1)
		c.unwatchStats("db1")
		final := mustCapture(t, c.CaptureFinal, "run-1", 1)
		if entries := mustCollect(t, c, base, final); len(entries) != 0 {
			t.Fatalf("entries = %+v, want none", entries)
		}
		if !hasNote(c.Notes(), HealthSampleFailed) {
			t.Fatalf("notes = %v, want one mentioning %s", c.Notes(), HealthSampleFailed)
		}
	})
}

// TestNotesAreDedupedAndBounded keeps the note buffer from becoming a log in
// the measured process.
func TestNotesAreDedupedAndBounded(t *testing.T) {
	c, _ := newTestCollector()
	c.mu.Lock()
	c.noteLocked("same note")
	c.noteLocked("same note")
	c.mu.Unlock()
	if got := c.Notes(); len(got) != 1 {
		t.Fatalf("notes = %v, want one", got)
	}

	c.mu.Lock()
	for i := 0; i < maxNotes*2; i++ {
		c.noteLocked(string(rune('a'+i%26)) + strings.Repeat("x", i))
	}
	c.mu.Unlock()
	if got := len(c.Notes()); got != maxNotes {
		t.Fatalf("notes = %d, want the buffer capped at %d", got, maxNotes)
	}
}

// TestEntryHelpersOnEmptyInterval covers the guards on the derived values.
func TestEntryHelpersOnEmptyInterval(t *testing.T) {
	var empty Entry
	if got := empty.Interval(); got != 0 {
		t.Fatalf("Interval() = %v, want 0", got)
	}
	if got := empty.AverageWait(); got != 0 {
		t.Fatalf("AverageWait() = %v, want 0", got)
	}
	half := Entry{BaselineAt: time.Now()}
	if got := half.Interval(); got != 0 {
		t.Fatalf("Interval() with no end = %v, want 0", got)
	}
}

// TestUnwatchMidRunPoolThatNeverJoined checks that removing a pool which was
// itself watched mid-run is a plain removal: it was never part of the run, so
// there is nothing to say farewell to.
func TestUnwatchMidRunPoolThatNeverJoined(t *testing.T) {
	c, _ := newTestCollector()
	if err := c.watchStats("db1", appDisplay, newScript(sql.DBStats{}).stats); err != nil {
		t.Fatalf("watchStats(db1) = %v, want nil", err)
	}
	base := mustCapture(t, c.CaptureBaseline, "run-1", 1)

	late := newScript(sql.DBStats{WaitCount: 3})
	if err := c.watchStats("db2", appDisplay, late.stats); err != nil {
		t.Fatalf("watchStats(db2) = %v, want nil", err)
	}
	if !c.unwatchStats("db2") {
		t.Fatal("unwatchStats(db2) = false, want true")
	}
	final := mustCapture(t, c.CaptureFinal, "run-1", 1)

	if got := targetIDs(mustCollect(t, c, base, final)); len(got) != 1 || got[0] != "db1" {
		t.Fatalf("entries = %v, want only db1", got)
	}
	if late.callCount() != 0 {
		t.Fatalf("db2 sampled %d times, want 0", late.callCount())
	}
	if hasNote(c.Notes(), HealthUnwatchedMidRun) {
		t.Fatalf("notes = %v, want no farewell note for a pool that never joined", c.Notes())
	}
}

// TestEntryDisplayFallsBackToBaseline covers a pool whose closing sample
// carries no display: the row still has to be identifiable.
func TestEntryDisplayFallsBackToBaseline(t *testing.T) {
	at := time.Now()
	entry := entryFor("db1",
		PoolSample{Stats: sql.DBStats{WaitCount: 2}, At: at, Display: appDisplay},
		PoolSample{Stats: sql.DBStats{WaitCount: 5}, At: at.Add(time.Minute)},
	)
	if entry.Display != appDisplay {
		t.Fatalf("Display = %q, want the baseline's %q", entry.Display, appDisplay)
	}
	if entry.WaitCount != 3 {
		t.Fatalf("WaitCount = %d, want 3", entry.WaitCount)
	}
}

// TestSafeStatsOnNilSampler covers the last fail-open guard: a missing sampler
// is a miss, not a nil dereference.
func TestSafeStatsOnNilSampler(t *testing.T) {
	if _, ok := safeStats(nil); ok {
		t.Fatal("safeStats(nil) reported success, want a miss")
	}
	stats, ok := safeStats(func() sql.DBStats { return sql.DBStats{WaitCount: 4} })
	if !ok || stats.WaitCount != 4 {
		t.Fatalf("safeStats = (%+v, %v), want the sampled value", stats, ok)
	}
}

// hasNote reports whether any note carries the health key.
func hasNote(notes []string, key string) bool {
	for _, note := range notes {
		if strings.Contains(note, key) {
			return true
		}
	}
	return false
}
