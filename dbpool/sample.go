package dbpool

import (
	"database/sql"
	"time"
)

// PoolSample is one pool's frozen observation at a boundary.
//
// sql.DBStats is a flat struct of numbers, so copying it copies it deeply and
// a caller holding this value can never observe the pool changing underneath.
type PoolSample struct {
	// Stats is the value (*sql.DB).Stats returned at At.
	Stats sql.DBStats
	// At is when this particular pool was read. It is per pool rather than per
	// boundary so that a farewell sample can carry the moment the pool left
	// the watch set.
	At time.Time
	// Display is carried in the sample, not looked up later, so that
	// Collect can build a complete Entry from frozen values alone.
	Display string
	// Unwatched marks a farewell sample: the pool was unwatched mid-run and
	// this is the last observation that will ever exist for it.
	Unwatched bool
}

// Sample is the frozen value a runctl.BaselineHandle carries for this
// collector: one PoolSample per watched TargetID.
//
// The plan sketched this as map[string]sql.DBStats. That type cannot work,
// because the same plan requires an entry to report Display and to end its
// interval at the moment of a mid-run UnwatchDBPool rather than at the run's
// closing boundary. Both are per pool, and neither is expressible in a bare
// sql.DBStats. Keeping them in the sample — instead of reading them back out
// of the collector during Collect — is what makes Collect a pure function of
// its two handles, which is the property the contract actually cares about.
type Sample map[string]PoolSample

// safeStats calls stats and turns a panicking implementation into a miss.
// (*sql.DB).Stats cannot panic, but the watchStats hook accepts any function,
// and measurement is never allowed to take the measured application down.
func safeStats(stats func() sql.DBStats) (s sql.DBStats, ok bool) {
	defer func() {
		if recover() != nil {
			s, ok = sql.DBStats{}, false
		}
	}()
	if stats == nil {
		return sql.DBStats{}, false
	}
	return stats(), true
}

// rewound reports whether any cumulative counter went backwards between two
// samples. Detection is best effort by construction: a pool recreated while a
// counter happens to be lower than before is caught, one recreated at exactly
// the right moment is not. UnwatchDBPool followed by WatchDBPool is the
// reliable way to mark such a boundary.
func rewound(base, final sql.DBStats) bool {
	return final.WaitCount < base.WaitCount ||
		final.WaitDuration < base.WaitDuration ||
		final.MaxIdleClosed < base.MaxIdleClosed ||
		final.MaxIdleTimeClosed < base.MaxIdleTimeClosed ||
		final.MaxLifetimeClosed < base.MaxLifetimeClosed
}
