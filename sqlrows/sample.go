package sqlrows

import "time"

// Reason codes for a target that carries no numbers. They are internal detail
// rather than wire values of runctl: a dropped boundary is reported to runctl
// as runctl.CodeNotCaptured, and these codes explain *why* inside the section.
const (
	// CodeProbeSkip means capability probing ruled the target out
	// (performance_schema off, digest consumer disabled, columns missing).
	CodeProbeSkip = "probe-skip"
	// CodeNoSchema means TargetInfo.Schema is empty, so there is no value to
	// bind to WHERE SCHEMA_NAME = ?. Guessing one would measure a different
	// database.
	CodeNoSchema = "no-schema"
	// CodeInspectorDefaultDB means the connection handed to this collector
	// could not be proven free of a default database, so its own statements
	// would be recorded as digests of the schema it is measuring. The target is
	// skipped: contaminated numbers that look plausible are worse than none,
	// and the operator has to be told which target to re-register.
	CodeInspectorDefaultDB = "inspector-default-db"
	// CodeBudgetExhausted means the target's wave could not start inside the
	// boundary budget. Recorded rather than dropped: a silently missing target
	// is indistinguishable from a target with no traffic.
	CodeBudgetExhausted = "budget-exhausted"
	// CodeQueryError means a statement against the target failed.
	CodeQueryError = "query-error"
	// CodeUnpairedBoundary means only one of the two boundaries has the
	// target, so no interval exists for it.
	CodeUnpairedBoundary = "unpaired-boundary"
	// CodeDBRestart means the server changed identity or restarted between the
	// boundaries, so the counters do not share an origin.
	CodeDBRestart = "db-restart"
	// CodeCounterReset means the digest table was truncated or the counters
	// rewound between the boundaries.
	CodeCounterReset = "counter-reset"
)

// DBClock anomaly codes. The order below is the evaluation order: the first
// matching condition wins, so the value is deterministic when several hold.
const (
	// AnomalyMissing means at least one of the four timestamps was not taken.
	AnomalyMissing = "clock-missing"
	// AnomalyBackwardsBaseline means the opening boundary's own two readings
	// are out of order.
	AnomalyBackwardsBaseline = "clock-backwards-baseline"
	// AnomalyBackwardsFinal means the closing boundary's two readings are out
	// of order.
	AnomalyBackwardsFinal = "clock-backwards-final"
	// AnomalyBackwardsInterval means the closing boundary started before the
	// opening boundary ended, which makes the measured interval empty or
	// inverted.
	AnomalyBackwardsInterval = "clock-backwards-interval"
)

// Sample is one boundary's frozen reading of every target. It is built once
// and never mutated afterwards: handles are copied and shared, so a later
// mutation would silently change an interval that was supposed to be fixed.
type Sample struct {
	Targets map[string]*TargetSample `json:"targets"`
}

// target looks up a target, tolerating a nil sample so Collect can treat a
// missing side as an unpaired boundary instead of crashing.
func (s *Sample) target(id string) (*TargetSample, bool) {
	if s == nil || s.Targets == nil {
		return nil, false
	}
	t, ok := s.Targets[id]
	return t, ok && t != nil
}

// ids returns the target IDs present in the sample.
func (s *Sample) ids() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Targets))
	for id := range s.Targets {
		out = append(out, id)
	}
	return out
}

// TargetSample is one target's reading at one boundary.
type TargetSample struct {
	TargetID string `json:"target_id"`
	// Schema is the value bound to WHERE SCHEMA_NAME = ?. It is recorded so a
	// snapshot shows which database the numbers describe.
	Schema string `json:"schema"`
	// ServerUUID and UptimeSec detect a server that restarted between the two
	// boundaries, which would make the counters incomparable.
	ServerUUID string `json:"server_uuid,omitempty"`
	UptimeSec  int64  `json:"uptime_sec,omitempty"`
	// UTCBefore and UTCAfter bracket the digest read with the database's own
	// clock. Plan 09 compares query-sample timestamps against this interval,
	// so both edges are needed, not just one.
	UTCBefore time.Time `json:"utc_before"`
	UTCAfter  time.Time `json:"utc_after"`
	// Digests holds the rows whose SCHEMA_NAME matched the bound schema, keyed
	// by hex DIGEST.
	Digests map[string]DigestRow `json:"digests,omitempty"`
	// Overflow is the row where SCHEMA_NAME and DIGEST are *both* NULL: the
	// bucket MySQL aggregates statements into once the digest table is full.
	// A NULL schema alone does not mean overflow — statements from any
	// connection without a default database, this collector's included, also
	// have a NULL schema but a real digest.
	Overflow    DigestRow `json:"overflow"`
	HasOverflow bool      `json:"has_overflow,omitempty"`
	// Texts maps DIGEST to its truncated DIGEST_TEXT. Only the closing
	// boundary fills it, and only for the digests that lead the interval.
	Texts map[string]string `json:"texts,omitempty"`
	// Captured reports that Digests is a real reading. When false, Code and
	// Err say why and the target contributes no numbers.
	Captured bool   `json:"captured"`
	Code     string `json:"code,omitempty"`
	Err      string `json:"err,omitempty"`
}

// DigestRow is one row of events_statements_summary_by_digest. Every field is
// a cumulative unsigned counter; the interval value is the difference between
// two readings.
type DigestRow struct {
	CountStar            uint64 `json:"count_star"`
	TimerWait            uint64 `json:"timer_wait"`
	RowsExamined         uint64 `json:"rows_examined"`
	RowsSent             uint64 `json:"rows_sent"`
	RowsAffected         uint64 `json:"rows_affected"`
	CreatedTmpDiskTables uint64 `json:"created_tmp_disk_tables"`
	SortMergePasses      uint64 `json:"sort_merge_passes"`
	NoIndexUsed          uint64 `json:"no_index_used"`
	NoGoodIndexUsed      uint64 `json:"no_good_index_used"`
}

// sub returns the interval between prev and r. Callers must have established
// with advancedFrom that no counter rewound; subtracting a rewound counter
// would wrap into an astronomically large unsigned value.
func (r DigestRow) sub(prev DigestRow) DigestRow {
	return DigestRow{
		CountStar:            r.CountStar - prev.CountStar,
		TimerWait:            r.TimerWait - prev.TimerWait,
		RowsExamined:         r.RowsExamined - prev.RowsExamined,
		RowsSent:             r.RowsSent - prev.RowsSent,
		RowsAffected:         r.RowsAffected - prev.RowsAffected,
		CreatedTmpDiskTables: r.CreatedTmpDiskTables - prev.CreatedTmpDiskTables,
		SortMergePasses:      r.SortMergePasses - prev.SortMergePasses,
		NoIndexUsed:          r.NoIndexUsed - prev.NoIndexUsed,
		NoGoodIndexUsed:      r.NoGoodIndexUsed - prev.NoGoodIndexUsed,
	}
}

// advancedFrom reports whether r is a later reading of the same counters than
// prev, i.e. no column went backwards. Every column is checked, not just
// COUNT_STAR: a TRUNCATE followed by re-execution can leave the call count
// plausible while the row counters have obviously restarted.
func (r DigestRow) advancedFrom(prev DigestRow) bool {
	return r.CountStar >= prev.CountStar &&
		r.TimerWait >= prev.TimerWait &&
		r.RowsExamined >= prev.RowsExamined &&
		r.RowsSent >= prev.RowsSent &&
		r.RowsAffected >= prev.RowsAffected &&
		r.CreatedTmpDiskTables >= prev.CreatedTmpDiskTables &&
		r.SortMergePasses >= prev.SortMergePasses &&
		r.NoIndexUsed >= prev.NoIndexUsed &&
		r.NoGoodIndexUsed >= prev.NoGoodIndexUsed
}

// DBClock carries the database's own UTC readings around both boundaries.
//
// Plan 09 decides query-sample freshness from this interval, so a database
// clock that stepped backwards (NTP, a virtualization host, a manual date)
// must be visible rather than silently producing an empty or inverted window
// in which every sample looks stale.
type DBClock struct {
	BaselineBefore time.Time `json:"baseline_before"`
	BaselineAfter  time.Time `json:"baseline_after"`
	FinalBefore    time.Time `json:"final_before"`
	FinalAfter     time.Time `json:"final_after"`
	// Monotonic reports BaselineBefore <= BaselineAfter <= FinalBefore <=
	// FinalAfter, all four readings present. When false, consumers must not
	// judge freshness at all — neither fresh nor stale.
	Monotonic bool `json:"monotonic"`
	// Anomaly is the stable code of the first violated condition, empty when
	// Monotonic is true.
	Anomaly string `json:"anomaly,omitempty"`
}

// newDBClock validates the four readings in the fixed order documented on the
// Anomaly constants, so the reported code does not depend on map iteration or
// on which check happens to run first.
func newDBClock(base, final *TargetSample) DBClock {
	clock := DBClock{
		BaselineBefore: base.UTCBefore,
		BaselineAfter:  base.UTCAfter,
		FinalBefore:    final.UTCBefore,
		FinalAfter:     final.UTCAfter,
	}
	switch {
	case clock.BaselineBefore.IsZero() || clock.BaselineAfter.IsZero() ||
		clock.FinalBefore.IsZero() || clock.FinalAfter.IsZero():
		clock.Anomaly = AnomalyMissing
	case clock.BaselineAfter.Before(clock.BaselineBefore):
		clock.Anomaly = AnomalyBackwardsBaseline
	case clock.FinalAfter.Before(clock.FinalBefore):
		clock.Anomaly = AnomalyBackwardsFinal
	case clock.FinalBefore.Before(clock.BaselineAfter):
		clock.Anomaly = AnomalyBackwardsInterval
	default:
		clock.Monotonic = true
	}
	return clock
}

// skew is the negative duration that explains the anomaly, for the health
// message. It is zero when nothing is out of order or when a reading is
// missing entirely.
func (c DBClock) skew() time.Duration {
	switch c.Anomaly {
	case AnomalyBackwardsBaseline:
		return c.BaselineAfter.Sub(c.BaselineBefore)
	case AnomalyBackwardsFinal:
		return c.FinalAfter.Sub(c.FinalBefore)
	case AnomalyBackwardsInterval:
		return c.FinalBefore.Sub(c.BaselineAfter)
	default:
		return 0
	}
}
