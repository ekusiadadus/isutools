package sqlrows

import (
	"sort"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// Health keys this package reports. The set is closed at seven: a new
// condition reuses one of these with a different message rather than adding a
// key, so a health snapshot stays readable.
const (
	// HealthSkip reports a target ruled out by capability probing.
	HealthSkip = "sqlrows-skip"
	// HealthNoSchema reports a target with no schema to bind.
	HealthNoSchema = "sqlrows-no-schema"
	// HealthOverflow reports statements aggregated into the digest table's
	// overflow row, i.e. incomplete coverage.
	HealthOverflow = "sqlrows-overflow"
	// HealthDBRestart reports a server that restarted or changed identity
	// between the boundaries.
	HealthDBRestart = "sqlrows-db-restart"
	// HealthCounterReset reports counters that were truncated or rewound.
	HealthCounterReset = "sqlrows-counter-reset"
	// HealthClockAnomaly reports a database clock that stepped backwards.
	HealthClockAnomaly = "sqlrows-clock-anomaly"
	// HealthTargetDropped reports targets that produced no interval.
	HealthTargetDropped = "sqlrows-target-dropped"
)

// MissingQueryText stands in for a digest whose text was not fetched — it fell
// outside the fetched top, or the opening boundary was never taken. It is
// never classified as a statement kind, because guessing one from a
// placeholder would put a fabricated SELECT in front of a user.
const MissingQueryText = "(digest text unavailable)"

// StatementKind separates the statement families a row can belong to. The
// examined/sent ratio only means something for SELECT, and DML is read through
// its affected-row count instead.
type StatementKind string

const (
	// KindSelect covers SELECT, including a WITH ... SELECT common table
	// expression.
	KindSelect StatementKind = "select"
	// KindDML covers INSERT, UPDATE, DELETE and REPLACE.
	KindDML StatementKind = "dml"
	// KindOther covers everything else, including digests whose text is
	// unavailable.
	KindOther StatementKind = "other"
)

// Section is the snapshot section sqlrows contributes.
type Section struct {
	// Targets is ordered by TargetID so two snapshots diff cleanly.
	Targets []TargetSection `json:"targets"`
	// Health carries this section's degradation notes, already grouped by
	// key and reason.
	Health []HealthNote `json:"health,omitempty"`
	// Validity is the verdict this section contributes to the run. sqlrows is
	// an optional collector, so it degrades a run to partial and never
	// invalidates it: a database without performance_schema is a normal
	// deployment, not a broken measurement.
	Validity runctl.Validity `json:"validity"`
	// Limit is the number of rows a target may show, recorded so a reader can
	// tell a truncated table from a short one.
	Limit int `json:"limit"`
}

// TargetSection is one target's interval.
type TargetSection struct {
	TargetID string `json:"target_id"`
	Schema   string `json:"schema,omitempty"`
	// Usable reports that Digests holds real interval values. When false the
	// target contributes no numbers, and consumers that enrich rows — plan
	// 09's EXPLAIN capture in particular — must skip this target entirely.
	Usable bool `json:"usable"`
	// Code and Reason explain an unusable target: probe-skip, no-schema,
	// budget-exhausted, query-error, unpaired-boundary, db-restart or
	// counter-reset.
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Digests holds the leading rows of the interval, ordered by total time.
	Digests []DigestStat `json:"digests,omitempty"`
	// Shown, Total and Dropped make truncation explicit. Total counts every
	// digest that ran during the interval, not every digest in the table.
	Shown   int `json:"shown"`
	Total   int `json:"total"`
	Dropped int `json:"dropped"`
	// Overflow describes the digest table's overflow row for this target.
	Overflow OverflowStat `json:"overflow"`
	// DBClock carries the database-side interval. Consumers must not judge
	// freshness when Monotonic is false.
	DBClock DBClock `json:"db_clock"`
}

// DigestStat is one digest's interval value.
type DigestStat struct {
	Digest string `json:"digest"`
	// Query is the truncated DIGEST_TEXT, or MissingQueryText.
	Query string        `json:"query"`
	Kind  StatementKind `json:"kind"`
	Count uint64        `json:"count"`
	// TimerWaitPicos is the raw SUM_TIMER_WAIT delta; TotalTime is the same
	// value as a duration, kept because picoseconds are unreadable and the
	// raw number is needed to reproduce the ordering.
	TimerWaitPicos uint64        `json:"timer_wait_picos"`
	TotalTime      time.Duration `json:"total_time"`
	RowsExamined   uint64        `json:"rows_examined"`
	RowsSent       uint64        `json:"rows_sent"`
	RowsAffected   uint64        `json:"rows_affected"`
	// ExaminedPerSent is only defined for SELECT statements that returned at
	// least one row. HasRatio distinguishes "not applicable" from "zero":
	// treating a SELECT that sent nothing as a ratio of RowsExamined would
	// invent the worst possible score for a query that simply found nothing.
	ExaminedPerSent float64 `json:"examined_per_sent,omitempty"`
	HasRatio        bool    `json:"has_ratio"`
	// Index and sort quality signals, all interval values.
	NoIndexUsed          uint64 `json:"no_index_used"`
	NoGoodIndexUsed      uint64 `json:"no_good_index_used"`
	CreatedTmpDiskTables uint64 `json:"created_tmp_disk_tables"`
	SortMergePasses      uint64 `json:"sort_merge_passes"`
}

// OverflowStat describes the digest table's overflow row.
//
// That row is instance-global, so several targets on one server would
// otherwise report the same overflow several times. Detected is therefore set
// on the first target of a server only, and the others point at it.
type OverflowStat struct {
	Detected  bool   `json:"detected"`
	CountStar uint64 `json:"count_star,omitempty"`
	// ReportedBy names the target carrying this server's overflow when this
	// target is not the one reporting it.
	ReportedBy string `json:"reported_by,omitempty"`
}

// HealthNote is one grouped degradation message.
type HealthNote struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// noteKey groups targets that degraded for the same reason, so sixteen
// budget-dropped targets produce one line instead of sixteen.
type noteKey struct {
	key    string
	reason string
}

// noteSet accumulates health notes in a deterministic order.
type noteSet struct {
	order   []noteKey
	targets map[noteKey][]string
}

func newNoteSet() *noteSet {
	return &noteSet{targets: map[noteKey][]string{}}
}

// add records that target degraded under key for reason.
func (n *noteSet) add(key, reason, target string) {
	k := noteKey{key: key, reason: reason}
	if _, seen := n.targets[k]; !seen {
		n.order = append(n.order, k)
	}
	n.targets[k] = append(n.targets[k], target)
}

// notes renders the accumulated groups, sorted by key then reason. Target
// lists keep the order they were added in, which is TargetID order.
func (n *noteSet) notes() []HealthNote {
	if len(n.order) == 0 {
		return nil
	}
	keys := append([]noteKey(nil), n.order...)
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].key != keys[j].key {
			return keys[i].key < keys[j].key
		}
		return keys[i].reason < keys[j].reason
	})
	out := make([]HealthNote, 0, len(keys))
	for _, k := range keys {
		message := strings.Join(n.targets[k], ", ")
		if k.reason != "" {
			message += " (" + k.reason + ")"
		}
		out = append(out, HealthNote{Key: k.key, Message: message})
	}
	return out
}
