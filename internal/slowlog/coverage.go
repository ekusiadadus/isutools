// Package slowlog parses bounded MySQL slow-query logs and can invoke a pinned
// pt-query-digest binary after a run. Raw SQL is never included in its portable
// summary.
package slowlog

import "time"

type FileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type CapturePoint struct {
	Identity FileIdentity `json:"identity"`
	Offset   uint64       `json:"offset"`
	DBClock  time.Time    `json:"db_clock,omitzero"`
}

type Coverage struct {
	Complete    bool         `json:"complete"`
	Reason      string       `json:"reason,omitempty"`
	Identity    FileIdentity `json:"identity"`
	StartOffset uint64       `json:"start_offset"`
	EndOffset   uint64       `json:"end_offset"`
	DBStartedAt time.Time    `json:"db_started_at,omitzero"`
	DBEndedAt   time.Time    `json:"db_ended_at,omitzero"`
}

func EvaluateCoverage(start, end CapturePoint) Coverage {
	coverage := Coverage{
		Complete: true, Identity: start.Identity, StartOffset: start.Offset, EndOffset: end.Offset,
		DBStartedAt: start.DBClock.UTC(), DBEndedAt: end.DBClock.UTC(),
	}
	switch {
	case start.Identity.Device != end.Identity.Device || start.Identity.Inode != end.Identity.Inode:
		coverage.Complete, coverage.Reason = false, "log-rotated"
	case end.Offset < start.Offset:
		coverage.Complete, coverage.Reason = false, "log-truncated"
	case !start.DBClock.IsZero() && !end.DBClock.IsZero() && end.DBClock.Before(start.DBClock):
		coverage.Complete, coverage.Reason = false, "db-clock-backwards"
	}
	return coverage
}
