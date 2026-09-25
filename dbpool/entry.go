package dbpool

import "time"

// Entry codes. An empty Code means the interval is a normal, complete one.
// The set is closed: transports and (future) advisors switch on these values,
// and they are dbpool's own codes, unrelated to runctl.CollectorBoundary.Code.
const (
	// CodeCounterRewind means a cumulative counter went backwards between the
	// two samples, which happens when the application replaces the *sql.DB
	// under the same target. A delta across that boundary would be a negative
	// or wildly inflated number, so the entry reports the final absolute values
	// instead and flags itself.
	CodeCounterRewind = "counter-rewind"
	// CodeUnwatchedMidRun means UnwatchDBPool was called while the run was in
	// progress. The entry survives with the farewell sample taken at that
	// moment, so its interval is shorter than the run's.
	CodeUnwatchedMidRun = "unwatched-mid-run"
)

// Entry is one pool's interval report. Point values describe the closing
// boundary; counters are deltas over the interval, because a cumulative
// counter that has been running since process start says nothing about the
// benchmark that just ran.
type Entry struct {
	// TargetID is the registry's TargetID. Every other collector keys on the
	// same value, byte for byte, which is what allows a reader (or the agent)
	// to line up pool waits with row counts and query plans for one database.
	TargetID string `json:"target_id"`
	// Display is the credential-free endpoint description taken from the
	// registry when the pool was watched.
	Display string `json:"display"`

	// MaxOpen is SetMaxOpenConns as observed at the closing boundary; 0 means
	// unlimited.
	MaxOpen int `json:"max_open"`
	// Open is the number of established connections at the closing boundary.
	Open int `json:"open"`
	// InUse is the number of connections held by a caller at that moment.
	InUse int `json:"in_use"`
	// Idle is the number of pooled, unused connections at that moment.
	Idle int `json:"idle"`

	// WaitCount is the change in the number of connection requests queued by
	// database/sql during the interval. A queued request may still be waiting
	// at the final boundary or may later be canceled.
	WaitCount int64 `json:"wait_count"`
	// WaitDuration is the change in accumulated wait time. Each wait is added
	// when it ends, so the delta can include time before the baseline and
	// excludes waits still in progress. It sums concurrent goroutines rather
	// than elapsed run time. Read it alongside WaitCount, whose update occurs
	// at wait start instead of wait completion.
	WaitDuration time.Duration `json:"wait_duration_ns"`
	// MaxIdleClosed counts connections closed during the interval under the
	// MaxIdleConns policy, including when the configured limit is reduced.
	MaxIdleClosed int64 `json:"max_idle_closed"`
	// MaxIdleTimeClosed counts connections closed during the interval by
	// SetConnMaxIdleTime.
	MaxIdleTimeClosed int64 `json:"max_idle_time_closed"`
	// MaxLifetimeClosed counts connections closed during the interval by
	// SetConnMaxLifetime. It does not measure reconnection time.
	MaxLifetimeClosed int64 `json:"max_lifetime_closed"`

	// BaselineAt and FinalAt are the measured ends of this entry's interval,
	// on the same clock as runctl's boundary windows. They are per entry, not
	// per run, because an entry unwatched mid-run ends early and has to be
	// able to say so.
	BaselineAt time.Time `json:"baseline_at"`
	FinalAt    time.Time `json:"final_at"`

	// Partial marks an entry whose interval or values need a caveat; Code says
	// which one.
	Partial bool   `json:"partial,omitempty"`
	Code    string `json:"code,omitempty"`
}

// Interval is the measured length of this entry's interval. It is shorter than
// the run for an entry unwatched mid-run.
func (e Entry) Interval() time.Duration {
	if e.BaselineAt.IsZero() || e.FinalAt.IsZero() {
		return 0
	}
	return e.FinalAt.Sub(e.BaselineAt)
}

// AverageWait preserves the historical WaitDuration/WaitCount calculation.
// This quotient is not necessarily the mean for one cohort of requests:
// WaitCount increases when a request begins waiting, while WaitDuration
// increases when it finishes. A request spanning a run boundary contributes
// to different intervals. It is zero when WaitCount is zero.
func (e Entry) AverageWait() time.Duration {
	if e.WaitCount <= 0 {
		return 0
	}
	return e.WaitDuration / time.Duration(e.WaitCount)
}

// WaitRatio returns the boundary-counter quotient when both deltas are
// positive and the entry is complete. The value is an estimate, not a precise
// per-request mean, because waits can cross either measurement boundary.
// Unavailable ratios are reported with ok=false rather than as zero latency.
func (e Entry) WaitRatio() (value time.Duration, ok bool) {
	if e.Partial || e.Code != "" || e.WaitCount <= 0 || e.WaitDuration <= 0 {
		return 0, false
	}
	return e.WaitDuration / time.Duration(e.WaitCount), true
}
