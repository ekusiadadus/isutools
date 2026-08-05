package queryplan

import (
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
)

func TestWindowForAndJudge(t *testing.T) {
	clock := goodClock() // [baseTime, baseTime+30s], monotonic
	tests := []struct {
		name       string
		clock      sqlrows.DBClock
		validity   runctl.Validity
		seen       time.Time
		wantState  FreshnessState
		wantReason FreshReason
	}{
		{
			name: "middle of the interval", clock: clock, validity: runctl.ValidityValid,
			seen: baseTime.Add(15 * time.Second), wantState: FreshnessFresh, wantReason: FreshInInterval,
		},
		{
			// Half a second before BaselineAfter: inside the raw interval, but
			// the second of guard is what keeps a truncated timestamp from
			// looking fresh.
			name: "just before the opening boundary", clock: clock, validity: runctl.ValidityValid,
			seen: baseTime.Add(-500 * time.Millisecond), wantState: FreshnessStale, wantReason: FreshBeforeInterval,
		},
		{
			name: "just after the closing boundary", clock: clock, validity: runctl.ValidityValid,
			seen: baseTime.Add(30*time.Second + 500*time.Millisecond), wantState: FreshnessStale, wantReason: FreshAfterInterval,
		},
		{
			name: "exactly on the narrowed lower edge", clock: clock, validity: runctl.ValidityValid,
			seen: baseTime.Add(time.Second), wantState: FreshnessFresh, wantReason: FreshInInterval,
		},
		{
			name: "exactly on the narrowed upper edge", clock: clock, validity: runctl.ValidityValid,
			seen: baseTime.Add(29 * time.Second), wantState: FreshnessFresh, wantReason: FreshInInterval,
		},
		{
			name: "one tick inside the upper guard", clock: clock, validity: runctl.ValidityValid,
			seen: baseTime.Add(29*time.Second + time.Microsecond), wantState: FreshnessStale, wantReason: FreshAfterInterval,
		},
		{
			name: "clock stepped backwards inside the interval",
			clock: sqlrows.DBClock{
				BaselineBefore: baseTime, BaselineAfter: baseTime.Add(10 * time.Second),
				FinalBefore: baseTime.Add(5 * time.Second), FinalAfter: baseTime.Add(20 * time.Second),
				Monotonic: false, Anomaly: sqlrows.AnomalyBackwardsInterval,
			},
			validity: runctl.ValidityPartial, seen: baseTime.Add(7 * time.Second),
			wantState: FreshnessUnknown, wantReason: FreshClockAnomaly,
		},
		{
			name: "a reading was never taken",
			clock: sqlrows.DBClock{
				FinalBefore: baseTime.Add(30 * time.Second), FinalAfter: baseTime.Add(30 * time.Second),
				Monotonic: false, Anomaly: sqlrows.AnomalyMissing,
			},
			validity: runctl.ValidityPartial, seen: baseTime.Add(15 * time.Second),
			wantState: FreshnessUnknown, wantReason: FreshClockAnomaly,
		},
		{
			// No DBClock at all is not the same thing as a DBClock with a
			// verdict: nothing judged the readings, so nothing is known.
			name: "no database clock", clock: sqlrows.DBClock{}, validity: runctl.ValidityValid,
			seen: baseTime.Add(15 * time.Second), wantState: FreshnessUnknown, wantReason: FreshClockMissing,
		},
		{
			name: "partial run", clock: clock, validity: runctl.ValidityPartial,
			seen: baseTime.Add(15 * time.Second), wantState: FreshnessUnknown, wantReason: FreshRunPartial,
		},
		{
			name: "invalid run", clock: clock, validity: runctl.ValidityInvalid,
			seen: baseTime.Add(15 * time.Second), wantState: FreshnessUnknown, wantReason: FreshRunPartial,
		},
		{
			name: "an interval too short to survive the guard",
			clock: sqlrows.DBClock{
				BaselineBefore: baseTime, BaselineAfter: baseTime,
				FinalBefore: baseTime.Add(time.Second), FinalAfter: baseTime.Add(time.Second),
				Monotonic: true,
			},
			validity: runctl.ValidityValid, seen: baseTime.Add(500 * time.Millisecond),
			wantState: FreshnessUnknown, wantReason: FreshIntervalShort,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			window, reason, ok := windowFor(tc.clock, tc.validity)
			if !ok {
				if tc.wantState != FreshnessUnknown || reason != tc.wantReason {
					t.Fatalf("windowFor = unknown/%s, want %s/%s", reason, tc.wantState, tc.wantReason)
				}
				return
			}
			state, judged := window.judge(tc.seen)
			if state != tc.wantState || judged != tc.wantReason {
				t.Fatalf("judge = %s/%s, want %s/%s", state, judged, tc.wantState, tc.wantReason)
			}
		})
	}
}

// TestTargetValidityIsPerTarget pins what freshness is judged against.
//
// sqlrows publishes one section-wide verdict that is partial as soon as any one
// target is degraded. Reading that verdict per target would let a database
// restart behind db4 rule out db1, db2 and db3 as well; every input this
// package needs is carried on the target itself.
func TestTargetValidityIsPerTarget(t *testing.T) {
	tests := []struct {
		name   string
		target sqlrows.TargetSection
		want   runctl.Validity
	}{
		{
			name:   "a measurable target",
			target: usableTarget("db1", selectStat("d1", "SELECT ?", 100)),
			want:   runctl.ValidityValid,
		},
		{
			name:   "an unusable target",
			target: sqlrows.TargetSection{TargetID: "db1", Schema: "isuconp", Code: "db-restart"},
			want:   runctl.ValidityPartial,
		},
		{
			name: "a target carrying its own reason code",
			target: sqlrows.TargetSection{TargetID: "db1", Schema: "isuconp", Usable: true,
				Code: "counter-reset", DBClock: goodClock()},
			want: runctl.ValidityPartial,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetValidity(tc.target); got != tc.want {
				t.Fatalf("targetValidity = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFreshnessIgnoresTheOuterReadings pins that the ordering verdict is
// consumed rather than recomputed. sqlrows owns BaselineBefore and FinalAfter
// for its own ordering check; if this package compared them too it could
// disagree with the anomaly code shown next to the numbers.
func TestFreshnessIgnoresTheOuterReadings(t *testing.T) {
	poisoned := goodClock()
	poisoned.BaselineBefore = baseTime.Add(365 * 24 * time.Hour) // long after FinalAfter
	poisoned.FinalAfter = baseTime.Add(-365 * 24 * time.Hour)    // long before BaselineBefore

	window, _, ok := windowFor(poisoned, runctl.ValidityValid)
	if !ok {
		t.Fatal("a monotonic clock must still produce a window: the outer readings are not this package's business")
	}
	if state, reason := window.judge(baseTime.Add(15 * time.Second)); state != FreshnessFresh || reason != FreshInInterval {
		t.Fatalf("judge = %s/%s, want fresh/in_interval", state, reason)
	}

	// The mirror image: with Monotonic false, no arrangement of the outer
	// readings may turn the verdict into anything but the anomaly.
	broken := goodClock()
	broken.Monotonic = false
	broken.Anomaly = sqlrows.AnomalyBackwardsFinal
	if _, reason, ok := windowFor(broken, runctl.ValidityValid); ok || reason != FreshClockAnomaly {
		t.Fatalf("windowFor = %v/%s, want no window and %s", ok, reason, FreshClockAnomaly)
	}
}

func TestSecondRounding(t *testing.T) {
	whole := time.Date(2026, 8, 4, 12, 0, 5, 0, time.UTC)
	fraction := whole.Add(400 * time.Millisecond)
	if got := ceilSecond(whole); !got.Equal(whole) {
		t.Fatalf("ceilSecond(whole second) = %v, want it unchanged", got)
	}
	if got := ceilSecond(fraction); !got.Equal(whole.Add(time.Second)) {
		t.Fatalf("ceilSecond(%v) = %v", fraction, got)
	}
	if got := floorSecond(fraction); !got.Equal(whole) {
		t.Fatalf("floorSecond(%v) = %v", fraction, got)
	}
}

// TestBudgetsFitInsideRunctl pins this package's share of a target's second
// against runctl's constants. runctl is the single authority for the
// hierarchy; the numbers below are a subdivision of PerTargetBudget, and an
// inverted one would show up as targets timing out mid-EXPLAIN.
func TestBudgetsFitInsideRunctl(t *testing.T) {
	if startupBudget != SessionBudget+SampleBudget {
		t.Fatalf("startupBudget = %v, want session plus sample", startupBudget)
	}
	if startupBudget >= runctl.PerTargetBudget {
		t.Fatalf("a target that cannot fit %v of setup inside %v could never explain anything",
			startupBudget, runctl.PerTargetBudget)
	}
	if startupBudget+PerDigestBudget > runctl.PerTargetBudget {
		t.Fatalf("setup (%v) plus one EXPLAIN (%v) exceeds PerTargetBudget (%v)",
			startupBudget, PerDigestBudget, runctl.PerTargetBudget)
	}
	if runctl.PerTargetBudget >= runctl.EnrichBudget {
		t.Fatalf("PerTargetBudget (%v) must stay inside EnrichBudget (%v)",
			runctl.PerTargetBudget, runctl.EnrichBudget)
	}
	// Sixteen targets over the fan-out width is two waves, and two waves of
	// one target-budget each is exactly the enrich budget.
	waves := (sqlstats.MaxTargets + runctl.BaselineConcurrency - 1) / runctl.BaselineConcurrency
	if time.Duration(waves)*runctl.PerTargetBudget > runctl.EnrichBudget {
		t.Fatalf("%d waves of %v do not fit in %v", waves, runctl.PerTargetBudget, runctl.EnrichBudget)
	}
}
