package sqlrows

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/sqlstats"
)

// No test in this package talks to a database. Targets are driven either
// through the fake Querier below — which records every statement, so the
// per-boundary statement count can be asserted — or, for the registry contract
// test, through a driver registered with database/sql that returns canned
// rows.

// canned is one statement's scripted answer.
type canned struct {
	rows [][]any
	err  error
}

// fakeQuerier is a sqlstats.Querier that answers from a script and records
// what it was asked.
type fakeQuerier struct {
	mu      sync.Mutex
	stmts   []string
	args    [][]any
	answers map[string]canned
}

func newQuerier() *fakeQuerier {
	return &fakeQuerier{answers: map[string]canned{}}
}

// answer scripts a successful reply.
func (q *fakeQuerier) answer(query string, rows ...[]any) *fakeQuerier {
	q.answers[query] = canned{rows: rows}
	return q
}

// fail scripts a failure.
func (q *fakeQuerier) fail(query string, err error) *fakeQuerier {
	q.answers[query] = canned{err: err}
	return q
}

func (q *fakeQuerier) record(query string, args []any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stmts = append(q.stmts, query)
	q.args = append(q.args, args)
}

// statements returns a copy of everything issued so far.
func (q *fakeQuerier) statements() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.stmts...)
}

// argsFor returns the arguments of the first statement equal to query.
func (q *fakeQuerier) argsFor(query string) []any {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, stmt := range q.stmts {
		if stmt == query {
			return q.args[i]
		}
	}
	return nil
}

// reset clears the recording, so one boundary's statements can be counted
// without the previous boundary's.
func (q *fakeQuerier) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stmts, q.args = nil, nil
}

// lookup resolves an answer, matching the digest-text statement by prefix
// because its placeholder count varies.
func (q *fakeQuerier) lookup(query string) (canned, bool) {
	if answer, ok := q.answers[query]; ok {
		return answer, true
	}
	for scripted, answer := range q.answers {
		if strings.HasPrefix(query, scripted) {
			return answer, true
		}
	}
	return canned{}, false
}

func (q *fakeQuerier) QueryContext(_ context.Context, query string, args ...any) (sqlstats.Rows, error) {
	q.record(query, args)
	answer, ok := q.lookup(query)
	switch {
	case !ok:
		return nil, fmt.Errorf("fake: unscripted statement %q", query)
	case answer.err != nil:
		return nil, answer.err
	}
	return &fakeRows{rows: answer.rows}, nil
}

func (q *fakeQuerier) QueryRowContext(_ context.Context, query string, args ...any) sqlstats.Row {
	q.record(query, args)
	answer, ok := q.lookup(query)
	switch {
	case !ok:
		return fakeRow{err: fmt.Errorf("fake: unscripted statement %q", query)}
	case answer.err != nil:
		return fakeRow{err: answer.err}
	case len(answer.rows) == 0:
		return fakeRow{err: errors.New("fake: no rows")}
	}
	return fakeRow{values: answer.rows[0]}
}

func (q *fakeQuerier) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	q.record(query, args)
	return sqlResult{}, nil
}

// sqlResult satisfies database/sql's Result without pulling in a driver.
type sqlResult struct{}

func (sqlResult) LastInsertId() (int64, error) { return 0, nil }
func (sqlResult) RowsAffected() (int64, error) { return 0, nil }

type fakeRows struct {
	rows   [][]any
	pos    int
	closed bool
}

func (r *fakeRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error { return assignAll(dest, r.rows[r.pos-1]) }

func (r *fakeRows) Columns() ([]string, error) { return nil, nil }
func (r *fakeRows) Err() error                 { return nil }
func (r *fakeRows) Close() error               { r.closed = true; return nil }

type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assignAll(dest, r.values)
}

func (r fakeRow) Err() error { return r.err }

// assignAll copies driver values into scan destinations. The package only ever
// scans into *any, so anything else is a bug in the code under test.
func assignAll(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("fake: scan wants %d values, row has %d", len(dest), len(values))
	}
	for i, d := range dest {
		target, ok := d.(*any)
		if !ok {
			return fmt.Errorf("fake: unsupported scan destination %T", d)
		}
		*target = values[i]
	}
	return nil
}

// fakeServer scripts one MySQL-shaped target.
type fakeServer struct {
	uuid           string
	uptime         int64
	before         time.Time
	after          time.Time
	digests        [][]any
	texts          [][]any
	performance    any
	consumer       any
	consumerAbsent bool
	columns        []string
	uptimeFails    bool
	probeErr       error
	// defaultSchema is what performance_schema.threads reports as the
	// connection's default database. nil — a NULL PROCESSLIST_DB — is the
	// hygienic answer every healthy target gives.
	defaultSchema any
	// defaultSchemaAbsent removes the threads row entirely, which is the
	// server declining to answer the hygiene question.
	defaultSchemaAbsent bool
}

// baseTime is a fixed instant so clock assertions do not depend on wall time.
var baseTime = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// newServer returns a healthy performance_schema-enabled server.
func newServer() *fakeServer {
	return &fakeServer{
		uuid:        "uuid-1",
		uptime:      1000,
		before:      baseTime,
		after:       baseTime.Add(10 * time.Millisecond),
		performance: int64(1),
		consumer:    "YES",
		columns:     append([]string(nil), append(requiredColumns, optionalQuerySampleColumn)...),
	}
}

// digestRow builds one row of the digest statement's result set.
func digestRow(schema, digest any, row DigestRow) []any {
	return []any{
		schema, digest,
		row.CountStar, row.TimerWait, row.RowsExamined, row.RowsSent, row.RowsAffected,
		row.CreatedTmpDiskTables, row.SortMergePasses, row.NoIndexUsed, row.NoGoodIndexUsed,
	}
}

// querier turns the server description into a scripted Querier.
func (s *fakeServer) querier() *fakeQuerier {
	q := newQuerier()
	if s.probeErr != nil {
		q.fail(probePerformanceSchema, s.probeErr)
	} else {
		q.answer(probePerformanceSchema, []any{s.performance})
	}
	if s.defaultSchemaAbsent {
		q.answer(probeDefaultSchema)
	} else {
		q.answer(probeDefaultSchema, []any{s.defaultSchema})
	}
	if s.consumerAbsent {
		q.answer(probeDigestConsumer)
	} else {
		q.answer(probeDigestConsumer, []any{s.consumer})
	}
	columnRows := make([][]any, 0, len(s.columns))
	for _, name := range s.columns {
		columnRows = append(columnRows, []any{name})
	}
	q.answer(probeColumns, columnRows...)
	if s.uptimeFails {
		q.fail(probeUptime, errors.New("performance_schema.global_status is unavailable"))
	} else {
		q.answer(probeUptime, []any{s.uptime})
	}
	q.answer(metaPFS, []any{s.uuid, s.uptime, s.before})
	q.answer(metaSHOW, []any{s.uuid, s.before})
	q.answer(uptimeSHOW, []any{"Uptime", s.uptime})
	q.answer(digestRows, s.digests...)
	q.answer(clockAfter, []any{s.after})
	q.answer(digestTextPrefix, s.texts...)
	return q
}

// fakeInspect emulates sqlstats.Inspect: it hands out the target's scripted
// connection and issues the session-init statement the registry issues, which
// plan 04 counts as part of every boundary.
func fakeInspect(queriers map[string]*fakeQuerier) InspectFunc {
	return func(ctx context.Context, id string, purpose sqlstats.Purpose, fn func(context.Context, sqlstats.Querier) error) error {
		if purpose != sqlstats.PurposeStats {
			return fmt.Errorf("fake: sqlrows must inspect with %q, got %q", sqlstats.PurposeStats, purpose)
		}
		q, ok := queriers[id]
		if !ok {
			return fmt.Errorf("%w: %q", sqlstats.ErrUnknownTarget, id)
		}
		if _, err := q.ExecContext(ctx, "SET time_zone = '+00:00'"); err != nil {
			return err
		}
		return fn(ctx, q)
	}
}

// targetInfos builds registry-shaped target descriptions.
func targetInfos(schema string, ids ...string) []sqlstats.TargetInfo {
	out := make([]sqlstats.TargetInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, sqlstats.TargetInfo{ID: id, Driver: "mysql", Schema: schema})
	}
	return out
}

// testCollector wires a collector onto scripted targets.
func testCollector(infos []sqlstats.TargetInfo, queriers map[string]*fakeQuerier) *Collector {
	c := New()
	c.targets = func() []sqlstats.TargetInfo { return infos }
	c.inspect = fakeInspect(queriers)
	return c
}

// stepClock returns a clock that advances by step on every reading, so budget
// exhaustion can be forced without sleeping.
func stepClock(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	calls := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now := start.Add(time.Duration(calls) * step)
		calls++
		return now
	}
}

// sampleWith builds a frozen sample for the pure Collect tests.
func sampleWith(targets ...*TargetSample) *Sample {
	sample := &Sample{Targets: map[string]*TargetSample{}}
	for _, target := range targets {
		sample.Targets[target.TargetID] = target
	}
	return sample
}

// capturedTarget builds a captured target sample. Both boundaries share one
// instant, which the ordering rule allows (it requires non-decreasing, not
// strictly increasing, readings): a test that wants a clock anomaly has to
// state it, so no fixture produces one by accident.
func capturedTarget(id string, digests map[string]DigestRow) *TargetSample {
	return &TargetSample{
		TargetID:   id,
		Schema:     "isuconp",
		ServerUUID: "uuid-1",
		UptimeSec:  1000,
		UTCBefore:  baseTime,
		UTCAfter:   baseTime,
		Digests:    digests,
		Captured:   true,
	}
}
