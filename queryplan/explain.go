package queryplan

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// maxPlanRows bounds how many EXPLAIN rows one plan keeps. A UNION of many
// branches produces one row per branch, and a snapshot is held in memory.
const maxPlanRows = 64

// maxPlanCell bounds one plan cell. EXPLAIN cells are identifiers and fixed
// vocabulary, so this only ever trims a pathological Extra.
const maxPlanCell = 256

// sample is one recorded statement plus the database's own reading of when it
// ran.
//
// The text field is the reason this type is unexported and never embedded in
// anything reachable from a Section: it holds the statement's literals. It
// lives for the length of one digest's EXPLAIN and is then dropped with the
// map that held it.
type sample struct {
	text string
	seen time.Time
}

// fetchSamples reads every selected digest's sample in one statement.
func fetchSamples(ctx context.Context, q sqlstats.Querier, schema string, digests []string) (map[string]sample, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(digests)+1)
	args = append(args, schema)
	for _, digest := range digests {
		args = append(args, digest)
	}
	rows, err := q.QueryContext(ctx, sampleQuery(len(digests)), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]sample, len(digests))
	for rows.Next() {
		var digest, text, seen any
		if err := rows.Scan(&digest, &text, &seen); err != nil {
			return nil, err
		}
		key := toString(digest)
		if key == "" {
			continue
		}
		stamp, _ := toTime(seen)
		out[key] = sample{text: toString(text), seen: stamp}
	}
	return out, rows.Err()
}

// explainDigests runs the digest loop of one target.
//
// It returns one Plan per selected digest, in ranking order, whatever
// happened: a digest that was skipped for staleness, for a truncated sample or
// for lack of budget keeps its row and says why. Dropping it instead would
// make "this statement has no plan" indistinguishable from "this statement was
// never selected".
func (r *runner) explainDigests(ctx context.Context, q sqlstats.Querier, c candidate, sess session, samples map[string]sample) []Plan {
	plans := make([]Plan, 0, len(c.digests))
	exhausted := false
	for _, stat := range c.digests {
		plan := Plan{Digest: stat.Digest, Query: stat.Query, Freshness: FreshnessUnknown}
		s, ok := samples[stat.Digest]
		if !ok || s.text == "" || s.seen.IsZero() {
			// No usable sample: nothing to explain, and nothing to date it by
			// either, so no freshness verdict is claimed.
			plan.Err = &PlanError{Class: PlanErrSampleUnavail}
			plans = append(plans, plan)
			continue
		}
		plan.SampleSeen = s.seen
		plan.Freshness, plan.FreshReason = c.window.judge(s.seen)
		switch {
		case plan.Freshness != FreshnessFresh:
			// A stale sample was produced by a different execution than the
			// one this run measured, and its literals may select a different
			// plan entirely. Explaining it would attach a plausible plan to
			// the wrong statement.
		case len(s.text) >= sess.maxSQLTextLength:
			// The server truncates at this length, so the text may end
			// mid-statement; EXPLAIN would report a syntax error that says
			// more about the truncation than about the query.
			plan.Err = &PlanError{Class: PlanErrSampleTruncated}
		case exhausted || !r.fitsDigest(ctx):
			exhausted = true
			plan.Err = &PlanError{Class: PlanErrBudgetExhausted}
		default:
			plan.Rows, plan.Err = r.explainOne(ctx, q, s.text)
		}
		plans = append(plans, plan)
	}
	return plans
}

// fitsDigest reports whether another EXPLAIN still fits in the target's
// budget. Once it does not, the remaining digests are recorded as
// budget-exhausted rather than raced against the deadline.
func (r *runner) fitsDigest(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return deadline.Sub(r.now()) >= PerDigestBudget
}

// explainOne runs EXPLAIN for one sample under its own slice of the budget.
//
// The statement is built, used and dropped inside this call. Nothing derived
// from text is returned: the rows are EXPLAIN output and the error is a
// classification.
func (r *runner) explainOne(ctx context.Context, q sqlstats.Querier, text string) ([]PlanRow, *PlanError) {
	digestCtx, cancel := context.WithTimeout(ctx, PerDigestBudget)
	defer cancel()
	return explainRows(digestCtx, q, explainPrefix+text)
}

// explainRows issues the statement and maps its result set onto PlanRow.
//
// Columns are matched by name rather than by position, because the set differs
// across server versions (partitions and filtered are absent on older ones),
// and every value is scanned as a nullable string because every column of
// EXPLAIN output can be NULL.
func explainRows(ctx context.Context, q sqlstats.Querier, statement string) ([]PlanRow, *PlanError) {
	rows, err := q.QueryContext(ctx, statement)
	if err != nil {
		return nil, classify(err)
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		return nil, classify(err)
	}
	if len(columns) == 0 {
		return nil, &PlanError{Class: PlanErrOther}
	}

	var out []PlanRow
	for rows.Next() {
		if len(out) >= maxPlanRows {
			break
		}
		values := make([]sql.NullString, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, classify(err)
		}
		out = append(out, planRow(columns, values))
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// planRow assembles one row from the columns this package displays. Unknown
// columns are ignored rather than rejected: a future server adding one must
// not turn a working plan into a parse failure.
func planRow(columns []string, values []sql.NullString) PlanRow {
	var row PlanRow
	for i, name := range columns {
		if i >= len(values) {
			break
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "select_type":
			row.SelectType = cell(values[i])
		case "table":
			row.Table = cell(values[i])
		case "type":
			row.Type = cell(values[i])
		case "key":
			row.Key = cell(values[i])
		case "possible_keys":
			row.PossibleKeys = cell(values[i])
		case "rows":
			row.Rows = countCell(values[i])
		case "extra":
			row.Extra = cell(values[i])
		}
	}
	return row
}

// cell renders one nullable text column, leaving a NULL as a nil pointer so
// the JSON omits it entirely.
func cell(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	out := truncateBytes(value.String, maxPlanCell)
	return &out
}

// countCell renders the estimated row count, which is a number in a column
// that can still be NULL.
func countCell(value sql.NullString) *int64 {
	if !value.Valid {
		return nil
	}
	out := parseInt(value.String)
	return &out
}

// selectDigests picks the digests worth explaining: SELECT statements only, in
// the interval's own ranking order (total time descending), capped at top.
//
// DML is excluded on purpose. EXPLAIN of an UPDATE or DELETE is legal on
// MySQL 8, but this credential has no such privilege on the application's
// schema, so every one of them would come back as permission_denied.
func selectDigests(stats []sqlrows.DigestStat, top int) []sqlrows.DigestStat {
	out := make([]sqlrows.DigestStat, 0, top)
	for _, stat := range stats {
		if stat.Kind != sqlrows.KindSelect {
			continue
		}
		out = append(out, stat)
		if len(out) >= top {
			break
		}
	}
	return out
}

// digestKeys lists the digest identifiers of the selected statements.
func digestKeys(stats []sqlrows.DigestStat) []string {
	out := make([]string, 0, len(stats))
	for _, stat := range stats {
		out = append(out, stat.Digest)
	}
	return out
}
