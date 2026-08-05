package queryplan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// No test in this package talks to a database. Targets are driven either
// through the scripted Querier below — which records every statement, so the
// statement sequence itself can be asserted — or, in registry_test.go, through
// a driver registered with database/sql that returns canned rows, which is
// what exercises the real scan conversions.

// sessionInit is the statement the registry issues on every pinned
// connection. The fake Inspect records it so the statement sequence a test
// sees is the sequence the database would see.
const sessionInit = `SET time_zone = '+00:00'`

// canned is one statement's scripted answer.
type canned struct {
	columns []string
	rows    [][]any
	err     error
}

// fakeQuerier is a sqlstats.Querier that answers from a script and records
// what it was asked, including the deadline it was asked under.
type fakeQuerier struct {
	mu       sync.Mutex
	stmts    []string
	args     [][]any
	answers  map[string]canned
	execErrs map[string]error
	inspects atomic.Int64
	deadline time.Time
	hasDeadl bool
}

func newQuerier() *fakeQuerier {
	return &fakeQuerier{answers: map[string]canned{}, execErrs: map[string]error{}}
}

// answer scripts a successful reply with no column names.
func (q *fakeQuerier) answer(query string, rows ...[]any) *fakeQuerier {
	q.answers[query] = canned{rows: rows}
	return q
}

// answerCols scripts a reply whose column names matter, i.e. EXPLAIN.
func (q *fakeQuerier) answerCols(query string, columns []string, rows ...[]any) *fakeQuerier {
	q.answers[query] = canned{columns: columns, rows: rows}
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

// argsFor returns the arguments of the first statement with the given prefix.
func (q *fakeQuerier) argsFor(prefix string) []any {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, stmt := range q.stmts {
		if strings.HasPrefix(stmt, prefix) {
			return q.args[i]
		}
	}
	return nil
}

// firstWithPrefix returns the first recorded statement with the given prefix.
func (q *fakeQuerier) firstWithPrefix(prefix string) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, stmt := range q.stmts {
		if strings.HasPrefix(stmt, prefix) {
			return stmt, true
		}
	}
	return "", false
}

// countWithPrefix counts recorded statements with the given prefix.
func (q *fakeQuerier) countWithPrefix(prefix string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, stmt := range q.stmts {
		if strings.HasPrefix(stmt, prefix) {
			n++
		}
	}
	return n
}

// lookup resolves an answer: exact match first, then the longest scripted
// prefix. The longest wins because SHOW GRANTS FOR CURRENT_USER() is itself a
// prefix of the USING form.
func (q *fakeQuerier) lookup(query string) (canned, bool) {
	if answer, ok := q.answers[query]; ok {
		return answer, true
	}
	best, found := "", false
	for scripted := range q.answers {
		if strings.HasPrefix(query, scripted) && len(scripted) > len(best) {
			best, found = scripted, true
		}
	}
	if !found {
		return canned{}, false
	}
	return q.answers[best], true
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
	return &fakeRows{columns: answer.columns, rows: answer.rows}, nil
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
		return fakeRow{err: sql.ErrNoRows}
	}
	return fakeRow{values: answer.rows[0]}
}

func (q *fakeQuerier) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	q.record(query, args)
	if err := q.execErrs[query]; err != nil {
		return nil, err
	}
	return execResult{}, nil
}

type execResult struct{}

func (execResult) LastInsertId() (int64, error) { return 0, nil }
func (execResult) RowsAffected() (int64, error) { return 0, nil }

type fakeRows struct {
	columns []string
	rows    [][]any
	pos     int
	closed  bool
}

func (r *fakeRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error { return assignAll(dest, r.rows[r.pos-1]) }

func (r *fakeRows) Columns() ([]string, error) { return r.columns, nil }
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

// assignAll copies driver values into scan destinations, emulating the
// conversions database/sql performs for the two destination shapes this
// package uses.
func assignAll(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("fake: scan wants %d values, row has %d", len(dest), len(values))
	}
	for i, d := range dest {
		switch target := d.(type) {
		case *any:
			*target = values[i]
		case *sql.NullString:
			if values[i] == nil {
				*target = sql.NullString{}
				continue
			}
			*target = sql.NullString{String: driverString(values[i]), Valid: true}
		default:
			return fmt.Errorf("fake: unsupported scan destination %T", d)
		}
	}
	return nil
}

// driverString renders a driver value the way database/sql renders it into a
// string destination.
func driverString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	case int64:
		return strconv.FormatInt(s, 10)
	case uint64:
		return strconv.FormatUint(s, 10)
	default:
		return fmt.Sprint(v)
	}
}

// baseTime is a fixed instant so clock assertions never depend on wall time.
var baseTime = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// leastPrivilegeGrants is the grant set the documented setup produces: SELECT
// on the measured schema and on performance_schema, plus the UPDATE that lets
// the session turn its own instrumentation off. No roles.
var leastPrivilegeGrants = []string{
	"GRANT USAGE ON *.* TO `isutools_explain`@`%`",
	"GRANT SELECT ON `isuconp`.* TO `isutools_explain`@`%`",
	"GRANT SELECT ON `performance_schema`.* TO `isutools_explain`@`%`",
	"GRANT UPDATE ON `performance_schema`.`threads` TO `isutools_explain`@`%`",
}

// explainColumns is MySQL 8's EXPLAIN result-set shape.
var explainColumns = []string{
	"id", "select_type", "table", "partitions", "type",
	"possible_keys", "key", "key_len", "ref", "rows", "filtered", "Extra",
}

// fullScanRow is one EXPLAIN row for a table scan.
func fullScanRow(table string, rows int64, extra any) []any {
	return []any{
		int64(1), "SIMPLE", table, nil, "ALL",
		nil, nil, nil, nil, rows, 100.0, extra,
	}
}

// fakeServer scripts one target's whole session.
type fakeServer struct {
	// currentRole is the value CURRENT_ROLE() answers with. It is an any
	// rather than a string because nil — a NULL column — is one of the
	// answers that has to be covered.
	currentRole any
	setRoleErr  error
	grants      []string
	grantsUsing []string
	// grantsUsingChain scripts the role expansion by exact statement, which is
	// what lets a nested role graph answer differently on each round of the
	// fixpoint. Keys take precedence over grantsUsing, which answers by prefix.
	grantsUsingChain map[string][]string
	grantsErr        error
	// grantsUsingErr fails the expansion alone, leaving the direct read
	// intact.
	grantsUsingErr  error
	instrumented    string
	hasSampleColumn bool
	maxTextLength   int64
	// maxLengthErr, useErr and sampleErr fail one statement of the sequence.
	maxLengthErr error
	useErr       error
	sampleErr    error
	// samples maps a digest to its recorded text and QUERY_SAMPLE_SEEN.
	samples map[string]sample
	// explainRows is the canned EXPLAIN result, and explainErr the failure to
	// return instead.
	explainRows [][]any
	explainErr  error
}

// newServer returns a correctly configured MySQL 8 target: roles neutralised,
// least-privilege grants, uninstrumented session, QUERY_SAMPLE_TEXT present.
func newServer() *fakeServer {
	return &fakeServer{
		currentRole:     "NONE",
		grants:          leastPrivilegeGrants,
		instrumented:    "NO",
		hasSampleColumn: true,
		maxTextLength:   1024,
		samples:         map[string]sample{},
		explainRows:     [][]any{fullScanRow("posts", 12345, "Using filesort")},
	}
}

// withSample records one digest's sample text and time.
func (s *fakeServer) withSample(digest, text string, seen time.Time) *fakeServer {
	s.samples[digest] = sample{text: text, seen: seen}
	return s
}

// querier turns the server description into a scripted Querier.
func (s *fakeServer) querier() *fakeQuerier {
	q := newQuerier()
	if s.setRoleErr != nil {
		q.execErrs[stmtSetRoleNone] = s.setRoleErr
	}
	q.answer(stmtCurrentRole, []any{s.currentRole})
	if s.grantsErr != nil {
		q.fail(stmtShowGrants, s.grantsErr)
	} else {
		q.answer(stmtShowGrants, singleColumn(s.grants)...)
	}
	if s.grantsUsingErr != nil {
		q.fail(stmtShowGrantsUsing, s.grantsUsingErr)
	} else {
		q.answer(stmtShowGrantsUsing, singleColumn(s.grantsUsing)...)
	}
	for statement, lines := range s.grantsUsingChain {
		q.answer(statement, singleColumn(lines)...)
	}
	q.answer(stmtDeinstrument)
	q.answer(stmtInstrumented, []any{s.instrumented})
	if s.hasSampleColumn {
		q.answer(stmtSampleColumn, []any{"QUERY_SAMPLE_TEXT"})
	} else {
		q.answer(stmtSampleColumn)
	}
	if s.maxLengthErr != nil {
		q.fail(stmtMaxSQLTextLength, s.maxLengthErr)
	} else {
		q.answer(stmtMaxSQLTextLength, []any{s.maxTextLength})
	}
	if s.useErr != nil {
		q.fail("USE ", s.useErr)
	} else {
		q.answer("USE ")
	}
	if s.sampleErr != nil {
		q.fail(samplePrefix, s.sampleErr)
	} else {
		q.answer(samplePrefix, s.sampleRows()...)
	}
	if s.explainErr != nil {
		q.fail(explainPrefix, s.explainErr)
	} else {
		q.answerCols(explainPrefix, explainColumns, s.explainRows...)
	}
	return q
}

// sampleRows renders the sample read's result set in a deterministic order.
func (s *fakeServer) sampleRows() [][]any {
	digests := make([]string, 0, len(s.samples))
	for digest := range s.samples {
		digests = append(digests, digest)
	}
	sortStrings(digests)
	out := make([][]any, 0, len(digests))
	for _, digest := range digests {
		row := s.samples[digest]
		out = append(out, []any{digest, row.text, row.seen})
	}
	return out
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func singleColumn(values []string) [][]any {
	out := make([][]any, 0, len(values))
	for _, value := range values {
		out = append(out, []any{value})
	}
	return out
}

// fakeInspect emulates sqlstats.Inspect: it checks the purpose, hands out the
// target's scripted connection, and issues the session-init statement the
// registry issues on every pinned connection.
func fakeInspect(queriers map[string]*fakeQuerier) InspectFunc {
	return func(ctx context.Context, id string, purpose sqlstats.Purpose, fn func(context.Context, sqlstats.Querier) error) error {
		if purpose != sqlstats.PurposeExplain {
			return fmt.Errorf("fake: queryplan must inspect with %q, got %q", sqlstats.PurposeExplain, purpose)
		}
		q, ok := queriers[id]
		if !ok {
			return fmt.Errorf("%w: %q has no %q credential", sqlstats.ErrPurposeNotRegistered, id, purpose)
		}
		q.inspects.Add(1)
		q.mu.Lock()
		q.deadline, q.hasDeadl = ctx.Deadline()
		q.mu.Unlock()
		q.record(sessionInit, nil)
		return fn(ctx, q)
	}
}

// failingInspect returns an Inspect that always fails with err.
func failingInspect(err error) InspectFunc {
	return func(context.Context, string, sqlstats.Purpose, func(context.Context, sqlstats.Querier) error) error {
		return err
	}
}

// goodClock is a monotonic database clock spanning thirty seconds, which makes
// the freshness window [baseTime+1s, baseTime+29s].
func goodClock() sqlrows.DBClock {
	return sqlrows.DBClock{
		BaselineBefore: baseTime,
		BaselineAfter:  baseTime,
		FinalBefore:    baseTime.Add(30 * time.Second),
		FinalAfter:     baseTime.Add(30 * time.Second),
		Monotonic:      true,
	}
}

// selectStat builds one SELECT row of an interval.
func selectStat(digest, query string, picos uint64) sqlrows.DigestStat {
	return sqlrows.DigestStat{
		Digest:         digest,
		Query:          query,
		Kind:           sqlrows.KindSelect,
		Count:          10,
		TimerWaitPicos: picos,
		TotalTime:      time.Duration(picos/1000) * time.Nanosecond,
	}
}

// usableTarget builds one measurable target of an interval.
func usableTarget(id string, stats ...sqlrows.DigestStat) sqlrows.TargetSection {
	return sqlrows.TargetSection{
		TargetID: id,
		Schema:   "isuconp",
		Usable:   true,
		Digests:  stats,
		Shown:    len(stats),
		Total:    len(stats),
		DBClock:  goodClock(),
	}
}

// interval builds a valid sqlrows section around the given targets.
func interval(targets ...sqlrows.TargetSection) *sqlrows.Section {
	return &sqlrows.Section{
		Targets:  targets,
		Validity: runctl.ValidityValid,
		Limit:    sqlrows.DigestTextFetchLimit,
	}
}

// oneTarget is the common fixture: one target, one hot SELECT digest, one
// sample recorded in the middle of the interval.
func oneTarget(sampleText string) (*sqlrows.Section, map[string]*fakeQuerier, *fakeServer) {
	server := newServer().withSample("d1", sampleText, baseTime.Add(15*time.Second))
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ? FROM posts WHERE id = ?", 5_000_000_000)))
	return rows, queriers, server
}

// findTarget returns the section of one target.
func findTarget(section *Section, id string) (TargetSection, bool) {
	for _, target := range section.Targets {
		if target.TargetID == id {
			return target, true
		}
	}
	return TargetSection{}, false
}

// noteFor returns the health note recorded under a reason ID.
func noteFor(section *Section, key string) (HealthNote, bool) {
	for _, note := range section.Health {
		if note.Key == key {
			return note, true
		}
	}
	return HealthNote{}, false
}

// errIsPurposeUnregistered is the registry's own sentinel, wrapped the way the
// registry wraps it.
var errIsPurposeUnregistered = fmt.Errorf("%w: %q has no %q credential",
	sqlstats.ErrPurposeNotRegistered, "db1", sqlstats.PurposeExplain)

var errCanned = errors.New("canned failure")
