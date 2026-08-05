package sqlrows

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// buildSection derives the whole section from two frozen samples. It is pure:
// given the same two samples it returns the same section, which is what lets a
// snapshot be rebuilt long after the run.
func buildSection(base, final *Sample) *Section {
	section := &Section{Validity: runctl.ValidityValid, Limit: DigestTextFetchLimit}
	notes := newNoteSet()
	// overflowBy maps a server UUID to the target already reporting its
	// overflow row, which is instance-global rather than per schema.
	overflowBy := map[string]string{}

	for _, id := range unionIDs(base, final) {
		b, bok := base.target(id)
		f, fok := final.target(id)

		if !bok || !fok || !b.Captured || !f.Captured {
			target := droppedTarget(id, b, bok, f, fok)
			notes.add(healthKeyFor(target.Code), healthReason(target.Code, target.Reason), id)
			section.Targets = append(section.Targets, target)
			section.Validity = runctl.ValidityPartial
			continue
		}

		eval := evaluateInterval(b, f)
		target := TargetSection{
			TargetID: id,
			Schema:   f.Schema,
			DBClock:  eval.clock,
		}
		if eval.code != "" {
			target.Code, target.Reason = eval.code, eval.reason
			notes.add(healthKeyFor(eval.code), healthReason(eval.code, eval.reason), id)
			section.Validity = runctl.ValidityPartial
			section.Targets = append(section.Targets, target)
			continue
		}

		target.Usable = true
		target.Digests = eval.stats
		target.Shown = len(eval.stats)
		target.Total = eval.total
		target.Dropped = eval.total - len(eval.stats)
		target.Overflow = overflowStat(f.ServerUUID, id, eval.overflow, overflowBy)
		if target.Overflow.Detected {
			notes.add(HealthOverflow,
				fmt.Sprintf("%d statements fell into the overflow row; raise performance_schema_digests_size",
					eval.overflow.CountStar), id)
		}
		if !eval.clock.Monotonic {
			// The counters survive a clock step — they do not depend on the
			// wall clock — but freshness judgements based on this interval do
			// not, so the run is degraded and consumers are told to abstain.
			notes.add(HealthClockAnomaly, clockReason(eval.clock), id)
			section.Validity = runctl.ValidityPartial
		}
		section.Targets = append(section.Targets, target)
	}

	section.Health = notes.notes()
	return section
}

// intervalEval is one target's verdict plus, when the verdict allows it, its
// interval values.
type intervalEval struct {
	clock DBClock
	// code is empty when the interval holds; otherwise it is db-restart or
	// counter-reset and no numbers are published.
	code   string
	reason string

	stats    []DigestStat
	total    int
	overflow DigestRow
}

// evaluateInterval applies the interval-validity rules in a fixed order and,
// when they pass, subtracts the two readings.
//
// The rules exist because two readings of a cumulative counter only form an
// interval if they share an origin. A restarted server, a truncated digest
// table and a rewound counter all break that assumption in ways a plain
// subtraction would silently turn into plausible-looking numbers.
func evaluateInterval(b, f *TargetSample) intervalEval {
	eval := intervalEval{clock: newDBClock(b, f)}

	switch {
	case b.ServerUUID != "" && f.ServerUUID != "" && b.ServerUUID != f.ServerUUID:
		eval.code = CodeDBRestart
		eval.reason = "server uuid changed between the boundaries"
		return eval
	case f.UptimeSec < b.UptimeSec && eval.clock.Monotonic:
		// Uptime is "now minus server start", so a backwards clock lowers it
		// without any restart. Only a decrease that the clock cannot explain
		// is read as a restart; server_uuid survives one, so it cannot rule
		// this case out on its own.
		eval.code = CodeDBRestart
		eval.reason = fmt.Sprintf("uptime fell from %ds to %ds", b.UptimeSec, f.UptimeSec)
		return eval
	}

	if lost := lostDigest(b.Digests, f.Digests); lost != "" {
		eval.code = CodeCounterReset
		eval.reason = "baseline digest " + shortDigest(lost) + " is gone from the final reading"
		return eval
	}
	if rewound := rewoundDigest(b, f); rewound != "" {
		eval.code = CodeCounterReset
		eval.reason = "counters went backwards for " + rewound
		return eval
	}

	deltas := intervalDigests(b.Digests, f.Digests)
	ordered := activeDigests(deltas)
	eval.total = len(ordered)
	if len(ordered) > DigestTextFetchLimit {
		ordered = ordered[:DigestTextFetchLimit]
	}
	eval.stats = make([]DigestStat, 0, len(ordered))
	for _, digest := range ordered {
		eval.stats = append(eval.stats, digestStat(digest, deltas[digest], f.Texts))
	}
	eval.overflow = overflowDelta(b, f)
	return eval
}

// intervalDigests subtracts the opening reading from the closing one. A digest
// absent from the opening reading is new in this interval, so its whole
// counter is the interval value.
//
// A rewound counter is left as the raw closing value: this function is also
// used to rank digests for the text fetch, where a best-effort order is better
// than none, and a target with a rewind never publishes numbers anyway.
func intervalDigests(base, final map[string]DigestRow) map[string]DigestRow {
	out := make(map[string]DigestRow, len(final))
	for digest, current := range final {
		previous, seen := base[digest]
		if seen && current.advancedFrom(previous) {
			out[digest] = current.sub(previous)
			continue
		}
		out[digest] = current
	}
	return out
}

// activeDigests orders the digests that ran during the interval by total time
// descending, breaking ties on the digest itself so the order is total.
func activeDigests(deltas map[string]DigestRow) []string {
	out := make([]string, 0, len(deltas))
	for digest, row := range deltas {
		if row.CountStar == 0 {
			continue
		}
		out = append(out, digest)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := deltas[out[i]], deltas[out[j]]
		if left.TimerWait != right.TimerWait {
			return left.TimerWait > right.TimerWait
		}
		return out[i] < out[j]
	})
	return out
}

// rankDigests is activeDigests capped at limit.
func rankDigests(deltas map[string]DigestRow, limit int) []string {
	ordered := activeDigests(deltas)
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered
}

// digestStat renders one interval row.
func digestStat(digest string, row DigestRow, texts map[string]string) DigestStat {
	query := MissingQueryText
	kind := KindOther
	if text, ok := texts[digest]; ok && strings.TrimSpace(text) != "" {
		query = text
		kind = Classify(text)
	}
	stat := DigestStat{
		Digest:               digest,
		Query:                query,
		Kind:                 kind,
		Count:                row.CountStar,
		TimerWaitPicos:       row.TimerWait,
		TotalTime:            time.Duration(row.TimerWait/1000) * time.Nanosecond,
		RowsExamined:         row.RowsExamined,
		RowsSent:             row.RowsSent,
		RowsAffected:         row.RowsAffected,
		NoIndexUsed:          row.NoIndexUsed,
		NoGoodIndexUsed:      row.NoGoodIndexUsed,
		CreatedTmpDiskTables: row.CreatedTmpDiskTables,
		SortMergePasses:      row.SortMergePasses,
	}
	if kind == KindSelect && row.RowsSent > 0 {
		stat.ExaminedPerSent = float64(row.RowsExamined) / float64(row.RowsSent)
		stat.HasRatio = true
	}
	return stat
}

// lostDigest returns the smallest baseline digest missing from the final
// reading, or "" when none is. A digest cannot leave the table while the
// server keeps running, so its disappearance means the table was reset.
func lostDigest(base, final map[string]DigestRow) string {
	lost := ""
	for digest := range base {
		if _, ok := final[digest]; ok {
			continue
		}
		if lost == "" || digest < lost {
			lost = digest
		}
	}
	return lost
}

// rewoundDigest returns a description of the first counter that went
// backwards, or "" when none did. The overflow row is checked too: it is a
// counter like any other, and a truncate resets it as well.
func rewoundDigest(b, f *TargetSample) string {
	rewound := ""
	for digest, current := range f.Digests {
		previous, ok := b.Digests[digest]
		if !ok || current.advancedFrom(previous) {
			continue
		}
		if rewound == "" || digest < rewound {
			rewound = digest
		}
	}
	if rewound != "" {
		return shortDigest(rewound)
	}
	if b.HasOverflow && f.HasOverflow && !f.Overflow.advancedFrom(b.Overflow) {
		return "the overflow row"
	}
	return ""
}

// overflowDelta is the interval value of the both-NULL overflow row.
func overflowDelta(b, f *TargetSample) DigestRow {
	if !f.HasOverflow {
		return DigestRow{}
	}
	if !b.HasOverflow {
		return f.Overflow
	}
	if !f.Overflow.advancedFrom(b.Overflow) {
		return DigestRow{}
	}
	return f.Overflow.sub(b.Overflow)
}

// overflowStat assigns the instance-global overflow row to exactly one target
// per server, so a two-schema deployment does not report it twice.
func overflowStat(serverUUID, targetID string, delta DigestRow, reportedBy map[string]string) OverflowStat {
	if delta.CountStar == 0 {
		return OverflowStat{}
	}
	if serverUUID == "" {
		// Without an identity the rows cannot be deduplicated; reporting is
		// the safer error, since overflow means incomplete coverage.
		return OverflowStat{Detected: true, CountStar: delta.CountStar}
	}
	if owner, seen := reportedBy[serverUUID]; seen {
		return OverflowStat{CountStar: delta.CountStar, ReportedBy: owner}
	}
	reportedBy[serverUUID] = targetID
	return OverflowStat{Detected: true, CountStar: delta.CountStar}
}

// droppedTarget renders a target that has no interval, preferring the most
// specific reason available on either side.
func droppedTarget(id string, b *TargetSample, bok bool, f *TargetSample, fok bool) TargetSection {
	target := TargetSection{TargetID: id, Code: CodeUnpairedBoundary}
	switch {
	case bok && fok:
		// Both boundaries saw the target but at least one could not read it.
		source := f
		if !b.Captured {
			source = b
		}
		target.Schema, target.Code, target.Reason = source.Schema, source.Code, source.Err
	case bok:
		target.Schema = b.Schema
		target.Reason = "the target was only present at the opening boundary"
	case fok:
		target.Schema = f.Schema
		target.Reason = "the target was only present at the closing boundary"
	}
	if target.Code == "" {
		target.Code = CodeUnpairedBoundary
	}
	if target.Reason == "" {
		target.Reason = target.Code
	}
	return target
}

// healthReason picks what a health line groups on. Codes that describe one
// situation exactly (a dropped target) group on the code, so sixteen dropped
// targets share one line; codes whose detail is the whole message (a probe
// verdict, a failed statement) keep their detail.
func healthReason(code, detail string) string {
	switch code {
	case CodeProbeSkip, CodeQueryError, CodeDBRestart, CodeCounterReset, CodeInspectorDefaultDB:
		if detail != "" {
			return detail
		}
	}
	return code
}

// healthKeyFor maps a reason code onto one of the seven health keys.
func healthKeyFor(code string) string {
	switch code {
	case CodeProbeSkip, CodeInspectorDefaultDB:
		// The key set is closed, so a target ruled out for connection hygiene
		// reuses the skip key; the reason carries what has to be fixed.
		return HealthSkip
	case CodeNoSchema:
		return HealthNoSchema
	case CodeDBRestart:
		return HealthDBRestart
	case CodeCounterReset:
		return HealthCounterReset
	default:
		return HealthTargetDropped
	}
}

// clockReason renders a clock anomaly with the size of the step, because
// "backwards" without a magnitude cannot be acted on.
func clockReason(clock DBClock) string {
	if skew := clock.skew(); skew != 0 {
		return fmt.Sprintf("%s, %s", clock.Anomaly, skew)
	}
	return clock.Anomaly
}

// shortDigest keeps a digest readable in a message. Digests are 64 hex
// characters and only their prefix is ever needed to correlate two lines.
func shortDigest(digest string) string {
	if len(digest) <= 16 {
		return digest
	}
	return digest[:16] + "…"
}

// unionIDs returns every target ID seen at either boundary, sorted.
func unionIDs(base, final *Sample) []string {
	seen := map[string]bool{}
	for _, id := range base.ids() {
		seen[id] = true
	}
	for _, id := range final.ids() {
		seen[id] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
