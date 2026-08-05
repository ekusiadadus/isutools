package queryplan

import (
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlrows"
)

// FreshnessState says whether a sample can be trusted to describe this run.
//
// Three values rather than two: when the database's clock is not trustworthy,
// "stale" would be a claim the evidence does not support. Only fresh plans are
// advisor input; stale and unknown are displayed greyed out with their reason.
type FreshnessState string

const (
	// FreshnessFresh means the sample was recorded inside the measured
	// interval.
	FreshnessFresh FreshnessState = "fresh"
	// FreshnessStale means it was recorded outside it.
	FreshnessStale FreshnessState = "stale"
	// FreshnessUnknown means the question could not be answered.
	FreshnessUnknown FreshnessState = "unknown"
)

// FreshReason is the closed set of reasons behind a FreshnessState.
type FreshReason string

const (
	// FreshInInterval: the sample time is inside the conservative window.
	FreshInInterval FreshReason = "in_interval"
	// FreshBeforeInterval: it precedes the window, so the plan describes an
	// execution from before this run.
	FreshBeforeInterval FreshReason = "before_interval"
	// FreshAfterInterval: it follows the window.
	FreshAfterInterval FreshReason = "after_interval"
	// FreshClockAnomaly: sqlrows reported a non-monotonic database clock.
	FreshClockAnomaly FreshReason = "db_clock_anomaly"
	// FreshClockMissing: the target carries no database clock at all.
	FreshClockMissing FreshReason = "db_clock_missing"
	// FreshRunPartial: the run's interval is partial, so the window it would
	// be judged against is not the run.
	FreshRunPartial FreshReason = "run_partial"
	// FreshIntervalShort: the window closed after narrowing, i.e. the run was
	// shorter than the rounding this package applies.
	FreshIntervalShort FreshReason = "interval_too_short"
)

// clockGuard is how far each edge of the interval is pulled inwards.
//
// QUERY_SAMPLE_SEEN's fractional-second precision depends on server settings,
// and a value truncated to the second can look up to a second older than it
// is. Narrowing both edges by that second only ever turns a fresh sample into
// a stale one, never the reverse: a plan wrongly shown as stale costs an
// advisor hint, while a plan wrongly shown as fresh is advice about a
// statement this run never ran.
const clockGuard = time.Second

// freshWindow is the interval a sample must fall into.
type freshWindow struct {
	lo time.Time
	hi time.Time
}

// targetValidity is one target's own verdict, which is what freshness is
// judged against.
//
// sqlrows publishes a single section-wide Validity, and it degrades to partial
// as soon as any one target does: a restarted database behind db4 makes the
// whole section partial. Everything this package needs, though, is per target —
// the interval rows, the usability flag, the reason code and the DBClock all
// belong to one target, and plan 09's own table of judged-unusable conditions
// is written per target. Judging db1's samples against the section-wide verdict
// would turn db4's restart into unknown/run_partial on db1, db2 and db3, and
// issue no EXPLAIN at all for a fault that happened elsewhere.
//
// For a single-target deployment the two are the same verdict, since the only
// things that make sqlrows' section partial are a target being unusable and a
// target's clock being non-monotonic — the first is this function, the second
// is windowFor's own first check.
func targetValidity(target sqlrows.TargetSection) runctl.Validity {
	if !target.Usable || target.Code != "" {
		return runctl.ValidityPartial
	}
	return runctl.ValidityValid
}

// windowFor derives the freshness window of one target, or the reason no
// verdict can be given.
//
// The order of the checks is fixed and matters. A non-monotonic clock also
// degrades sqlrows' section to partial, so testing partial first would report
// every clock anomaly as a partial run and hide the actual fault.
//
// Nothing here re-derives the ordering of the four clock readings: sqlrows has
// already done that and published the verdict as Monotonic and Anomaly.
// Comparing the timestamps again would risk disagreeing with the anomaly code
// shown next to the numbers. BaselineBefore and FinalAfter exist for that
// verdict alone and are never read here.
func windowFor(clock sqlrows.DBClock, validity runctl.Validity) (freshWindow, FreshReason, bool) {
	switch {
	case !clock.Monotonic && clock.Anomaly == "":
		return freshWindow{}, FreshClockMissing, false
	case !clock.Monotonic:
		return freshWindow{}, FreshClockAnomaly, false
	case validity != runctl.ValidityValid:
		return freshWindow{}, FreshRunPartial, false
	}
	window := freshWindow{
		lo: ceilSecond(clock.BaselineAfter).Add(clockGuard),
		hi: floorSecond(clock.FinalBefore).Add(-clockGuard),
	}
	if window.lo.After(window.hi) {
		return freshWindow{}, FreshIntervalShort, false
	}
	return window, FreshInInterval, true
}

// judge places one sample time in the window.
func (w freshWindow) judge(seen time.Time) (FreshnessState, FreshReason) {
	switch {
	case seen.Before(w.lo):
		return FreshnessStale, FreshBeforeInterval
	case seen.After(w.hi):
		return FreshnessStale, FreshAfterInterval
	default:
		return FreshnessFresh, FreshInInterval
	}
}

// ceilSecond rounds up to the next whole second, leaving an already-whole
// second alone.
func ceilSecond(t time.Time) time.Time {
	truncated := t.Truncate(time.Second)
	if truncated.Equal(t) {
		return truncated
	}
	return truncated.Add(time.Second)
}

// floorSecond rounds down to the whole second.
func floorSecond(t time.Time) time.Time { return t.Truncate(time.Second) }
