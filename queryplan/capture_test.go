package queryplan

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
)

func TestCaptureExplainsTheHottestSelect(t *testing.T) {
	rows, queriers, _ := oneTarget("SELECT id FROM posts WHERE user_id = 1")
	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers), Top: 10})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	target, ok := findTarget(section, "db1")
	if !ok {
		t.Fatalf("no section for db1: %+v", section.Targets)
	}
	if !target.Explained || target.Code != "" {
		t.Fatalf("target = %+v, want an explained target with no reason", target)
	}
	if len(target.Plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(target.Plans))
	}
	plan := target.Plans[0]
	switch {
	case plan.Err != nil:
		t.Fatalf("plan error = %+v, want none", *plan.Err)
	case plan.Freshness != FreshnessFresh || plan.FreshReason != FreshInInterval:
		t.Fatalf("freshness = %s/%s, want fresh/in_interval", plan.Freshness, plan.FreshReason)
	case plan.Digest != "d1" || plan.Query != "SELECT ? FROM posts WHERE id = ?":
		t.Fatalf("plan identity = %q / %q, want the interval's digest and normalized text", plan.Digest, plan.Query)
	case !plan.SampleSeen.Equal(baseTime.Add(15 * time.Second)):
		t.Fatalf("sample_seen = %v, want the database's own reading", plan.SampleSeen)
	case len(plan.Rows) != 1:
		t.Fatalf("rows = %+v, want one", plan.Rows)
	}
	row := plan.Rows[0]
	if row.Type == nil || *row.Type != "ALL" {
		t.Fatalf("type = %v, want ALL", row.Type)
	}
	if row.Rows == nil || *row.Rows != 12345 {
		t.Fatalf("rows estimate = %v, want 12345", row.Rows)
	}
	if row.Extra == nil || *row.Extra != "Using filesort" {
		t.Fatalf("extra = %v, want the filesort marker", row.Extra)
	}
	if row.Key != nil || row.PossibleKeys != nil {
		t.Fatalf("a NULL key must stay absent, got key=%v possible=%v", row.Key, row.PossibleKeys)
	}
	if len(section.Health) != 0 {
		t.Fatalf("health = %+v, want none", section.Health)
	}
	if section.Top != 10 {
		t.Fatalf("top = %d, want the configured ceiling", section.Top)
	}
}

// TestSessionSequence pins the statement order. Two constraints are load
// bearing: the privilege check runs before anything else touches the server,
// and de-instrumentation is verified before USE, so a session that fails the
// check never had a default database and its statements were filed under a
// NULL schema instead of the measured one.
func TestSessionSequence(t *testing.T) {
	rows, queriers, _ := oneTarget("SELECT 1")
	if _, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	want := []string{
		sessionInit,
		stmtSetRoleNone,
		stmtCurrentRole,
		stmtShowGrants,
		stmtDeinstrument,
		stmtInstrumented,
		stmtSampleColumn,
		stmtMaxSQLTextLength,
		"USE `isuconp`",
		samplePrefix,
		explainPrefix,
	}
	got := queriers["db1"].statements()
	if len(got) != len(want) {
		t.Fatalf("statements = %#v,\nwant %d statements: %#v", got, len(want), want)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(got[i], prefix) {
			t.Fatalf("statement %d = %q, want it to start with %q", i, got[i], prefix)
		}
	}
	if indexOfPrefix(got, stmtInstrumented) > indexOfPrefix(got, "USE ") {
		t.Fatal("USE must not run before the session is known to be uninstrumented")
	}
}

// TestSampleReadBindsSchemaAndDigest pins the pair condition. The digest
// table's primary key is (SCHEMA_NAME, DIGEST): a condition on the digest
// alone can return another schema's row, whose literals would then be
// explained against this schema.
func TestSampleReadBindsSchemaAndDigest(t *testing.T) {
	rows, queriers, _ := oneTarget("SELECT 1")
	if _, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	statement, ok := queriers["db1"].firstWithPrefix(samplePrefix)
	if !ok {
		t.Fatal("no sample read was issued")
	}
	for _, want := range []string{"SCHEMA_NAME = ?", "DIGEST IN (?)", "QUERY_SAMPLE_TEXT", "QUERY_SAMPLE_SEEN"} {
		if !strings.Contains(statement, want) {
			t.Fatalf("sample read %q does not contain %q", statement, want)
		}
	}
	args := queriers["db1"].argsFor(samplePrefix)
	if len(args) != 2 || args[0] != "isuconp" || args[1] != "d1" {
		t.Fatalf("sample read args = %#v, want the schema first and the digest second", args)
	}
	// Both halves of the key are bound parameters, never interpolated.
	if strings.Contains(statement, "isuconp") || strings.Contains(statement, "d1") {
		t.Fatalf("the schema or digest was interpolated into %q", statement)
	}
}

func TestSampleQueryGrowsWithTheDigestCount(t *testing.T) {
	if got := sampleQuery(3); !strings.HasSuffix(got, "DIGEST IN (?, ?, ?)") {
		t.Fatalf("sampleQuery(3) = %q", got)
	}
}

// TestInspectIsCalledOncePerTarget pins that the whole sequence happens inside
// one Inspect. Session state — neutralised roles, the instrumentation flag,
// the default database — belongs to the pinned connection, so splitting the
// digests across calls would explain them on a session none of it applies to.
func TestInspectIsCalledOncePerTarget(t *testing.T) {
	server := newServer().
		withSample("d1", "SELECT 1", baseTime.Add(10*time.Second)).
		withSample("d2", "SELECT 2", baseTime.Add(11*time.Second)).
		withSample("d3", "SELECT 3", baseTime.Add(12*time.Second))
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1",
		selectStat("d1", "SELECT ?", 300),
		selectStat("d2", "SELECT ?", 200),
		selectStat("d3", "SELECT ?", 100),
	))
	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got := queriers["db1"].inspects.Load(); got != 1 {
		t.Fatalf("Inspect was called %d times, want exactly one per target per run", got)
	}
	if got := queriers["db1"].countWithPrefix(explainPrefix); got != 3 {
		t.Fatalf("%d EXPLAIN statements, want one per selected digest", got)
	}
	target, _ := findTarget(section, "db1")
	if len(target.Plans) != 3 {
		t.Fatalf("plans = %d, want 3", len(target.Plans))
	}
}

func TestTopBoundsTheSelection(t *testing.T) {
	server := newServer()
	stats := make([]sqlrows.DigestStat, 0, 5)
	for i, digest := range []string{"d1", "d2", "d3", "d4", "d5"} {
		server.withSample(digest, "SELECT "+digest, baseTime.Add(10*time.Second))
		stats = append(stats, selectStat(digest, "SELECT ?", uint64(500-i)))
	}
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	section, err := Capture(context.Background(), Input{
		Rows: interval(usableTarget("db1", stats...)), Inspect: fakeInspect(queriers), Top: 2,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	if len(target.Plans) != 2 {
		t.Fatalf("plans = %d, want the top 2", len(target.Plans))
	}
	if target.Plans[0].Digest != "d1" || target.Plans[1].Digest != "d2" {
		t.Fatalf("plans = %q, %q, want the interval's own ranking order",
			target.Plans[0].Digest, target.Plans[1].Digest)
	}
}

// TestOnlySelectsAreExplained: this credential has no privilege on the
// application's tables beyond SELECT, so explaining an UPDATE would produce a
// permission error for every DML digest in the interval.
func TestOnlySelectsAreExplained(t *testing.T) {
	server := newServer().withSample("d2", "SELECT 2", baseTime.Add(10*time.Second))
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	dml := sqlrows.DigestStat{Digest: "d1", Query: "UPDATE posts SET ...", Kind: sqlrows.KindDML, TimerWaitPicos: 900}
	other := sqlrows.DigestStat{Digest: "d3", Query: "COMMIT", Kind: sqlrows.KindOther, TimerWaitPicos: 800}
	rows := interval(usableTarget("db1", dml, selectStat("d2", "SELECT ?", 700), other))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	if len(target.Plans) != 1 || target.Plans[0].Digest != "d2" {
		t.Fatalf("plans = %+v, want only the SELECT digest", target.Plans)
	}
}

func TestTargetsWithNothingToExplain(t *testing.T) {
	tests := []struct {
		name     string
		target   sqlrows.TargetSection
		wantCode string
		wantNote bool
	}{
		{
			name: "unusable interval",
			target: sqlrows.TargetSection{TargetID: "db1", Schema: "isuconp", Usable: false,
				Code: "db-restart", Reason: "server uuid changed"},
			wantCode: CodeNoInterval,
		},
		{
			name:     "no schema to bind",
			target:   sqlrows.TargetSection{TargetID: "db1", Usable: true, DBClock: goodClock()},
			wantCode: CodeNoSchema, wantNote: true,
		},
		{
			name: "a schema that cannot be quoted",
			target: sqlrows.TargetSection{TargetID: "db1", Schema: "isu`conp", Usable: true,
				DBClock: goodClock()},
			wantCode: CodeNoSchema, wantNote: true,
		},
		{
			name:     "no select digests",
			target:   usableTarget("db1"),
			wantCode: CodeNoDigests,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			queriers := map[string]*fakeQuerier{"db1": newServer().querier()}
			section, err := Capture(context.Background(), Input{
				Rows: interval(tc.target), Inspect: fakeInspect(queriers),
			})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			target, ok := findTarget(section, "db1")
			if !ok {
				t.Fatal("a target that produced nothing must still be reported")
			}
			if target.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", target.Code, tc.wantCode)
			}
			if target.Reason != reasons[tc.wantCode] {
				t.Fatalf("reason = %q, want the fixed sentence for %q", target.Reason, tc.wantCode)
			}
			if len(target.Plans) != 0 || target.Explained {
				t.Fatalf("target = %+v, want no plans", target)
			}
			if _, ok := noteFor(section, tc.wantCode); ok != tc.wantNote {
				t.Fatalf("health note for %q = %v, want %v", tc.wantCode, ok, tc.wantNote)
			}
			// Nothing may be issued against a target that was ruled out.
			if got := queriers["db1"].inspects.Load(); got != 0 {
				t.Fatalf("Inspect was called %d times for a target with nothing to explain", got)
			}
		})
	}
}

// TestUnknownFreshnessNeverConnects: with an untrustworthy clock no plan could
// be used even if it were fetched, so the target keeps its rows — with the
// reason — and costs the enrich budget nothing.
func TestUnknownFreshnessNeverConnects(t *testing.T) {
	target := usableTarget("db1", selectStat("d1", "SELECT ?", 100))
	target.DBClock = sqlrows.DBClock{
		BaselineBefore: baseTime, BaselineAfter: baseTime.Add(10 * time.Second),
		FinalBefore: baseTime.Add(5 * time.Second), FinalAfter: baseTime.Add(20 * time.Second),
		Anomaly: sqlrows.AnomalyBackwardsInterval,
	}
	rows := interval(target)
	rows.Validity = runctl.ValidityPartial
	queriers := map[string]*fakeQuerier{"db1": newServer().querier()}

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	got, _ := findTarget(section, "db1")
	if len(got.Plans) != 1 {
		t.Fatalf("plans = %+v, want the digest recorded with its reason", got.Plans)
	}
	plan := got.Plans[0]
	if plan.Freshness != FreshnessUnknown || plan.FreshReason != FreshClockAnomaly {
		t.Fatalf("freshness = %s/%s, want unknown/db_clock_anomaly", plan.Freshness, plan.FreshReason)
	}
	if !plan.SampleSeen.IsZero() || plan.Err != nil || len(plan.Rows) != 0 {
		t.Fatalf("plan = %+v, want no plan rows and no sample time", plan)
	}
	if got := queriers["db1"].inspects.Load(); got != 0 {
		t.Fatalf("Inspect was called %d times for a target whose clock cannot date a sample", got)
	}
}

// TestOneDegradedTargetDoesNotSuppressTheOthers is the multi-target case.
//
// sqlrows degrades its whole section to partial as soon as one target is
// unusable, so db2's restart makes Section.Validity partial while db1 is
// perfectly measurable: its own interval, its own monotonic clock, its own
// digests. Judging db1's freshness on the section-wide verdict would turn every
// one of its plans into unknown/run_partial and issue no EXPLAIN at all, losing
// a healthy target's measurement to a fault on another host.
func TestOneDegradedTargetDoesNotSuppressTheOthers(t *testing.T) {
	server := newServer().withSample("d1", "SELECT id FROM posts", baseTime.Add(15*time.Second))
	queriers := map[string]*fakeQuerier{"db1": server.querier(), "db2": newServer().querier()}
	rows := interval(
		usableTarget("db1", selectStat("d1", "SELECT ? FROM posts", 900)),
		sqlrows.TargetSection{TargetID: "db2", Schema: "isuconp", Usable: false,
			Code: "db-restart", Reason: "server uuid changed between the boundaries"},
	)
	// The verdict sqlrows publishes for a section holding db2.
	rows.Validity = runctl.ValidityPartial

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	healthy, _ := findTarget(section, "db1")
	if healthy.Code != "" || !healthy.Explained {
		t.Fatalf("db1 = %+v, want it explained despite db2's restart", healthy)
	}
	if len(healthy.Plans) != 1 {
		t.Fatalf("db1 plans = %+v, want one", healthy.Plans)
	}
	plan := healthy.Plans[0]
	if plan.Freshness != FreshnessFresh || plan.FreshReason != FreshInInterval {
		t.Fatalf("db1 freshness = %s/%s, want fresh/in_interval: db1's own clock is monotonic",
			plan.Freshness, plan.FreshReason)
	}
	if len(plan.Rows) != 1 {
		t.Fatalf("db1 plan rows = %+v, want the EXPLAIN output", plan.Rows)
	}
	if got := queriers["db1"].countWithPrefix(explainPrefix); got != 1 {
		t.Fatalf("%d EXPLAIN statements on db1, want one", got)
	}
	// db2 is still skipped, on its own reason and without a connection.
	degraded, _ := findTarget(section, "db2")
	if degraded.Code != CodeNoInterval {
		t.Fatalf("db2 code = %q, want %q", degraded.Code, CodeNoInterval)
	}
	if got := queriers["db2"].inspects.Load(); got != 0 {
		t.Fatalf("db2 was connected to %d times despite having no interval", got)
	}
}

// TestATargetWithItsOwnReasonCodeIsNotJudged: partial is read per target, and a
// target carrying a reason code of its own is one whose interval sqlrows could
// not stand behind. That verdict still rules its samples out.
func TestATargetWithItsOwnReasonCodeIsNotJudged(t *testing.T) {
	target := usableTarget("db1", selectStat("d1", "SELECT ?", 100))
	target.Code, target.Reason = "counter-reset", "counters went backwards"
	queriers := map[string]*fakeQuerier{"db1": newServer().querier()}

	section, err := Capture(context.Background(), Input{
		Rows: interval(target), Inspect: fakeInspect(queriers),
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plan := mustPlan(t, section, "db1", 0)
	if plan.Freshness != FreshnessUnknown || plan.FreshReason != FreshRunPartial {
		t.Fatalf("freshness = %s/%s, want unknown/run_partial", plan.Freshness, plan.FreshReason)
	}
	if got := queriers["db1"].inspects.Load(); got != 0 {
		t.Fatalf("Inspect was called %d times for a target whose interval carries a reason code", got)
	}
}

func TestStaleSampleIsRecordedButNotExplained(t *testing.T) {
	// Recorded half a second before the opening boundary: a different
	// execution, whose literals may well select a different plan.
	rows, queriers, _ := oneTarget("SELECT 1")
	server := newServer().withSample("d1", "SELECT 1", baseTime.Add(-500*time.Millisecond))
	queriers["db1"] = server.querier()

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	plan := target.Plans[0]
	if plan.Freshness != FreshnessStale || plan.FreshReason != FreshBeforeInterval {
		t.Fatalf("freshness = %s/%s, want stale/before_interval", plan.Freshness, plan.FreshReason)
	}
	if len(plan.Rows) != 0 || plan.Err != nil {
		t.Fatalf("plan = %+v, want a dated row with no plan", plan)
	}
	if got := queriers["db1"].countWithPrefix(explainPrefix); got != 0 {
		t.Fatalf("%d EXPLAIN statements were issued for a stale sample", got)
	}
}

func TestTruncatedSampleIsNotExplained(t *testing.T) {
	// A sample that reached performance_schema_max_sql_text_length may end
	// mid-statement, and EXPLAIN would report the truncation as a syntax
	// error of the application's query.
	server := newServer()
	server.maxTextLength = 32
	server.withSample("d1", strings.Repeat("a", 32), baseTime.Add(10*time.Second))
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	plan := target.Plans[0]
	if plan.Err == nil || plan.Err.Class != PlanErrSampleTruncated {
		t.Fatalf("plan error = %+v, want %q", plan.Err, PlanErrSampleTruncated)
	}
	if got := queriers["db1"].countWithPrefix(explainPrefix); got != 0 {
		t.Fatalf("%d EXPLAIN statements were issued for a possibly truncated sample", got)
	}
}

func TestMissingSampleRow(t *testing.T) {
	server := newServer() // no samples recorded at all
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	plan := target.Plans[0]
	if plan.Err == nil || plan.Err.Class != PlanErrSampleUnavail {
		t.Fatalf("plan error = %+v, want %q", plan.Err, PlanErrSampleUnavail)
	}
	if plan.Freshness != FreshnessUnknown {
		t.Fatalf("freshness = %q, want unknown: there is no sample to date", plan.Freshness)
	}
}

func TestAllNullExplainRowIsNotAFailure(t *testing.T) {
	// "Impossible WHERE noticed after reading const tables" produces a row
	// whose every column but Extra is NULL, and older servers return fewer
	// columns than newer ones.
	server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
	server.explainRows = [][]any{make([]any, len(explainColumns))}
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plan := mustPlan(t, section, "db1", 0)
	if plan.Err != nil {
		t.Fatalf("an all-NULL row must scan cleanly, got %+v", *plan.Err)
	}
	if len(plan.Rows) != 1 {
		t.Fatalf("rows = %+v, want the NULL row kept", plan.Rows)
	}
	row := plan.Rows[0]
	if row.SelectType != nil || row.Table != nil || row.Type != nil ||
		row.Key != nil || row.PossibleKeys != nil || row.Rows != nil || row.Extra != nil {
		t.Fatalf("row = %+v, want every column absent", row)
	}
}

func TestCaptureWithoutAnInterval(t *testing.T) {
	section, err := Capture(context.Background(), Input{})
	if !errors.Is(err, ErrNoInterval) {
		t.Fatalf("err = %v, want ErrNoInterval", err)
	}
	if section == nil || len(section.Targets) != 0 {
		t.Fatalf("section = %+v, want an empty one rather than nil", section)
	}
}

// TestBudgetExhaustionRecordsTheTargets: a wave that cannot fit session
// establishment and the sample read is not started, and the targets it would
// have held are recorded rather than dropped — a silently missing target is
// indistinguishable from a target with no traffic.
func TestBudgetExhaustionRecordsTheTargets(t *testing.T) {
	queriers := map[string]*fakeQuerier{
		"db1": newServer().querier(),
		"db2": newServer().querier(),
	}
	rows := interval(
		usableTarget("db1", selectStat("d1", "SELECT ?", 900)),
		usableTarget("db2", selectStat("d2", "SELECT ?", 100)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	section, err := Capture(ctx, Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	for _, id := range []string{"db1", "db2"} {
		target, ok := findTarget(section, id)
		if !ok {
			t.Fatalf("%s vanished from the section", id)
		}
		if target.Code != CodeBudgetExhausted {
			t.Fatalf("%s code = %q, want %q", id, target.Code, CodeBudgetExhausted)
		}
		if queriers[id].inspects.Load() != 0 {
			t.Fatalf("%s was connected to even though its wave could not start", id)
		}
	}
	note, ok := noteFor(section, CodeBudgetExhausted)
	if !ok {
		t.Fatalf("health = %+v, want a budget note", section.Health)
	}
	if !strings.Contains(note.Message, "db1, db2") {
		t.Fatalf("note = %q, want both targets grouped into one line", note.Message)
	}
}

// TestDigestBudgetExhaustionIsRecordedPerDigest drives the digest loop out of
// budget with an injected clock. The clock advances 100ms per reading, and the
// readings are: the wave check, the per-target budget, then one per digest —
// so the loop starts with room for an EXPLAIN and runs out during it.
func TestDigestBudgetExhaustionIsRecordedPerDigest(t *testing.T) {
	server := newServer()
	stats := make([]sqlrows.DigestStat, 0, 4)
	for i, digest := range []string{"d1", "d2", "d3", "d4"} {
		server.withSample(digest, "SELECT "+digest, baseTime.Add(10*time.Second))
		stats = append(stats, selectStat(digest, "SELECT ?", uint64(500-i)))
	}
	queriers := map[string]*fakeQuerier{"db1": server.querier()}

	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(600*time.Millisecond))
	defer cancel()

	section, err := Capture(ctx, Input{
		Rows:    interval(usableTarget("db1", stats...)),
		Inspect: fakeInspect(queriers),
		Now:     stepClock(start, 100*time.Millisecond),
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	if len(target.Plans) != 4 {
		t.Fatalf("plans = %d, want every selected digest recorded", len(target.Plans))
	}
	explained, exhausted := 0, 0
	for _, plan := range target.Plans {
		switch {
		case plan.Err == nil:
			explained++
		case plan.Err.Class == PlanErrBudgetExhausted:
			exhausted++
		default:
			t.Fatalf("unexpected plan error %+v", *plan.Err)
		}
	}
	if explained == 0 || exhausted == 0 {
		t.Fatalf("%d explained and %d recorded as out of budget, want both", explained, exhausted)
	}
	if got := queriers["db1"].countWithPrefix(explainPrefix); got != explained {
		t.Fatalf("%d EXPLAIN statements for %d explained plans", got, explained)
	}
}

// TestPerTargetContextIsTheSmallerOfTheTwoBudgets pins the budget arithmetic:
// the child context is min(PerTargetBudget, what is left of the enrich
// budget), never PerTargetBudget on its own.
func TestPerTargetContextIsTheSmallerOfTheTwoBudgets(t *testing.T) {
	tests := []struct {
		name   string
		parent time.Duration
		want   time.Duration
	}{
		{name: "plenty of enrich budget left", parent: 5 * time.Second, want: runctl.PerTargetBudget},
		{name: "less than a target budget left", parent: 450 * time.Millisecond, want: 450 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows, queriers, _ := oneTarget("SELECT 1")
			ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
			defer cancel()
			if _, err := Capture(ctx, Input{Rows: rows, Inspect: fakeInspect(queriers)}); err != nil {
				t.Fatalf("Capture: %v", err)
			}
			q := queriers["db1"]
			q.mu.Lock()
			deadline, ok := q.deadline, q.hasDeadl
			q.mu.Unlock()
			if !ok {
				t.Fatal("Inspect was called without a deadline")
			}
			if budget := time.Until(deadline); budget > tc.want {
				t.Fatalf("per-target budget = %v, want at most %v", budget, tc.want)
			}
		})
	}
}

func TestTargetsAreOrderedDeterministically(t *testing.T) {
	// Waves are assigned busiest target first so that a budget shortfall
	// always drops the same targets, and the section itself is ordered by
	// TargetID so two snapshots diff cleanly.
	queriers := map[string]*fakeQuerier{}
	targets := make([]sqlrows.TargetSection, 0, 3)
	for i, id := range []string{"db2", "db1", "db3"} {
		server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
		queriers[id] = server.querier()
		targets = append(targets, usableTarget(id, selectStat("d1", "SELECT ?", uint64(100*(i+1)))))
	}
	section, err := Capture(context.Background(), Input{Rows: interval(targets...), Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	var ids []string
	for _, target := range section.Targets {
		ids = append(ids, target.TargetID)
	}
	if strings.Join(ids, ",") != "db1,db2,db3" {
		t.Fatalf("targets = %v, want them ordered by id", ids)
	}
}

// TestPanicInOneTargetDoesNotEscape: measurement is fail-open. A panic inside
// enrichment must cost one target's plans, never the measured application.
func TestPanicInOneTargetDoesNotEscape(t *testing.T) {
	inspect := func(ctx context.Context, id string, purpose sqlstats.Purpose, fn func(context.Context, sqlstats.Querier) error) error {
		panic("driver exploded while explaining " + id)
	}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))
	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: inspect})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	if target.Code != CodeQueryError {
		t.Fatalf("code = %q, want %q", target.Code, CodeQueryError)
	}
}

// TestAStalledTargetDoesNotHoldTheHook is the availability guarantee.
//
// Capture runs in runctl's enrich hook, on the finish worker, inside
// EnrichBudget. A driver that ignores its context — a connection stuck in a
// syscall, a proxy that accepts and never answers — cannot be cancelled, so
// waiting for it unconditionally would hold the finish worker past the budget
// and past the lease, and the run's own watchdog would abort a run that was
// otherwise complete. One target's plans are an acceptable loss; a run is not.
func TestAStalledTargetDoesNotHoldTheHook(t *testing.T) {
	release := make(chan struct{})
	// The stalled worker outlives Capture on purpose; it is released when the
	// test ends so nothing is left running.
	t.Cleanup(func() { close(release) })

	queriers := map[string]*fakeQuerier{"db2": newServer().
		withSample("d2", "SELECT id FROM posts", baseTime.Add(15*time.Second)).querier()}
	healthy := fakeInspect(queriers)
	inspect := func(ctx context.Context, id string, purpose sqlstats.Purpose, fn func(context.Context, sqlstats.Querier) error) error {
		if id != "db1" {
			return healthy(ctx, id, purpose, fn)
		}
		<-release // ignores ctx entirely, which is the whole point
		return nil
	}

	rows := interval(
		usableTarget("db1", selectStat("d1", "SELECT ?", 900)),
		usableTarget("db2", selectStat("d2", "SELECT ? FROM posts", 100)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan *Section, 1)
	go func() {
		section, err := Capture(ctx, Input{Rows: rows, Inspect: inspect})
		if err != nil {
			t.Errorf("Capture: %v", err)
		}
		done <- section
	}()

	var section *Section
	select {
	case section = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Capture never returned: a driver that ignores its context held the enrich hook")
	}

	stalled, _ := findTarget(section, "db1")
	if stalled.Code != CodeTargetTimeout {
		t.Fatalf("db1 code = %q, want %q recorded rather than the target dropped",
			stalled.Code, CodeTargetTimeout)
	}
	if stalled.Reason != reasons[CodeTargetTimeout] {
		t.Fatalf("db1 reason = %q, want the fixed sentence", stalled.Reason)
	}
	if _, ok := noteFor(section, CodeTargetTimeout); !ok {
		t.Fatalf("health = %+v, want a note for the stalled target", section.Health)
	}
	// The other target of the same wave finished, and what it produced is kept.
	explained, _ := findTarget(section, "db2")
	if !explained.Explained || len(explained.Plans) != 1 || explained.Plans[0].Err != nil {
		t.Fatalf("db2 = %+v, want its plans kept: one stalled target is not the wave", explained)
	}
}

func TestInspectFailuresMapToReasonIDs(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "no explain credential", err: errIsPurposeUnregistered, want: CodePurposeUnregistered},
		{name: "unknown target", err: sqlstats.ErrUnknownTarget, want: CodeUnknownTarget},
		{name: "deadline", err: context.DeadlineExceeded, want: CodeBudgetExhausted},
		{name: "anything else", err: errCanned, want: CodeQueryError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))
			section, err := Capture(context.Background(), Input{Rows: rows, Inspect: failingInspect(tc.err)})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			target, _ := findTarget(section, "db1")
			if target.Code != tc.want {
				t.Fatalf("code = %q, want %q", target.Code, tc.want)
			}
			if len(target.Plans) != 0 {
				t.Fatalf("plans = %+v, want none", target.Plans)
			}
			if _, ok := noteFor(section, tc.want); !ok {
				t.Fatalf("health = %+v, want a note for %q", section.Health, tc.want)
			}
		})
	}
}

// indexOfPrefix returns the position of the first statement with the prefix.
func indexOfPrefix(statements []string, prefix string) int {
	for i, statement := range statements {
		if strings.HasPrefix(statement, prefix) {
			return i
		}
	}
	return -1
}

// mustPlan returns one plan of a target or fails the test.
func mustPlan(t *testing.T, section *Section, id string, index int) Plan {
	t.Helper()
	target, ok := findTarget(section, id)
	if !ok {
		t.Fatalf("no section for %s", id)
	}
	if index >= len(target.Plans) {
		t.Fatalf("target %s has %d plans, wanted plan %d (%+v)", id, len(target.Plans), index, target)
	}
	return target.Plans[index]
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
