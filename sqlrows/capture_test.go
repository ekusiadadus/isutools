package sqlrows

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// sampleOfResult unwraps the frozen sample a boundary produced.
func sampleOfResult(t *testing.T, res runctl.SampleResult) *Sample {
	t.Helper()
	sample, err := sampleOf(res.Handle)
	if err != nil {
		t.Fatalf("handle does not carry a sample: %v", err)
	}
	return sample
}

// TestBoundaryStatementCount pins the number of statements a boundary issues,
// per route and per boundary. The count is a contract: it is what the ABBA
// overhead gate measures, and a silent extra statement per target per run is
// exactly the kind of regression that gate exists to catch.
func TestBoundaryStatementCount(t *testing.T) {
	cases := []struct {
		name string
		// useSHOW routes uptime through SHOW GLOBAL STATUS.
		useSHOW bool
		// final measures the closing boundary instead of the opening one.
		final bool
		// firstRun includes the once-per-process capability probe, which is
		// five statements: the four capability questions plus the connection
		// hygiene check that proves this collector's own statements cannot be
		// recorded under the schema it measures.
		firstRun bool
		// withTexts makes the interval non-empty, so digest texts are fetched.
		withTexts bool
		want      int
	}{
		{name: "opening boundary, first run", firstRun: true, want: 9},
		{name: "opening boundary, later run", want: 4},
		{name: "opening boundary, SHOW route, first run", useSHOW: true, firstRun: true, want: 10},
		{name: "opening boundary, SHOW route, later run", useSHOW: true, want: 5},
		{name: "closing boundary with digest texts", final: true, withTexts: true, want: 5},
		{name: "closing boundary without digest texts", final: true, want: 4},
		{name: "closing boundary, SHOW route, with digest texts", final: true, useSHOW: true, withTexts: true, want: 6},
		{name: "closing boundary, SHOW route, without digest texts", final: true, useSHOW: true, want: 5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer()
			server.uptimeFails = tc.useSHOW
			server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 10})}
			server.texts = [][]any{{"aaa", "SELECT ? FROM `posts`"}}
			q := server.querier()
			c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})
			ctx := context.Background()

			switch {
			case tc.final:
				if _, err := c.CaptureBaseline(ctx, "run-1", 1); err != nil {
					t.Fatalf("CaptureBaseline: %v", err)
				}
				if tc.withTexts {
					// Traffic during the interval is what makes a digest worth
					// a text lookup.
					q.answer(digestRows, digestRow("isuconp", "aaa", DigestRow{CountStar: 9, TimerWait: 90}))
				}
				q.reset()
				if _, err := c.CaptureFinal(ctx, "run-1", 1); err != nil {
					t.Fatalf("CaptureFinal: %v", err)
				}
			case tc.firstRun:
				q.reset()
				if _, err := c.CaptureBaseline(ctx, "run-1", 1); err != nil {
					t.Fatalf("CaptureBaseline: %v", err)
				}
			default:
				if _, err := c.CaptureBaseline(ctx, "run-1", 1); err != nil {
					t.Fatalf("CaptureBaseline: %v", err)
				}
				q.reset()
				if _, err := c.CaptureBaseline(ctx, "run-2", 2); err != nil {
					t.Fatalf("second CaptureBaseline: %v", err)
				}
			}

			stmts := q.statements()
			if len(stmts) != tc.want {
				t.Fatalf("boundary issued %d statements, want %d:\n%s",
					len(stmts), tc.want, strings.Join(stmts, "\n"))
			}
		})
	}
}

// TestBoundaryIssuesNoStatementsBetweenBoundaries fixes the other half of the
// overhead contract: nothing is measured while the benchmark runs.
func TestBoundaryIssuesNoStatementsBetweenBoundaries(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 10})}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	q.reset()
	// Nothing happens here: the collector holds no timer and no goroutine.
	time.Sleep(time.Millisecond)
	if stmts := q.statements(); len(stmts) != 0 {
		t.Fatalf("the collector issued %d statements between boundaries: %v", len(stmts), stmts)
	}
}

// TestStatementsBindSchemaAndNeverCallDATABASE is the self-contamination
// contract in unit form: the schema is always bound, never interpolated, and
// DATABASE() — which would attribute this collector's own statements to the
// application's schema — appears nowhere.
func TestStatementsBindSchemaAndNeverCallDATABASE(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 10})}
	server.texts = [][]any{{"aaa", "SELECT ? FROM `posts`"}}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	q.answer(digestRows, digestRow("isuconp", "aaa", DigestRow{CountStar: 9, TimerWait: 90}))
	if _, err := c.CaptureFinal(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}

	stmts := q.statements()
	if len(stmts) == 0 {
		t.Fatal("no statements were recorded")
	}
	sawDigestRead, sawTextRead := false, false
	for _, stmt := range stmts {
		if strings.Contains(strings.ToUpper(stmt), "DATABASE()") {
			t.Fatalf("statement calls DATABASE(): %q", stmt)
		}
		if strings.Contains(stmt, "isuconp") {
			t.Fatalf("the schema was interpolated into the statement text: %q", stmt)
		}
		if stmt == digestRows {
			sawDigestRead = true
			if !strings.Contains(stmt, "SCHEMA_NAME = ?") {
				t.Fatalf("digest read does not bind the schema: %q", stmt)
			}
			if !strings.Contains(stmt, "(SCHEMA_NAME IS NULL AND DIGEST IS NULL)") {
				t.Fatalf("digest read does not select the overflow row: %q", stmt)
			}
		}
		if strings.HasPrefix(stmt, digestTextPrefix) {
			sawTextRead = true
			if !strings.Contains(stmt, "SCHEMA_NAME = ?") {
				t.Fatalf("digest text read does not bind the schema: %q", stmt)
			}
		}
	}
	if !sawDigestRead || !sawTextRead {
		t.Fatalf("expected both the digest read (%v) and the digest text read (%v)", sawDigestRead, sawTextRead)
	}

	if args := q.argsFor(digestRows); len(args) != 1 || args[0] != "isuconp" {
		t.Fatalf("digest read arguments = %v, want the bound schema", args)
	}
	textArgs := q.argsFor(digestTextQuery(1))
	if len(textArgs) != 2 || textArgs[0] != "isuconp" || textArgs[1] != "aaa" {
		t.Fatalf("digest text arguments = %v, want [schema digest]", textArgs)
	}
}

// TestOverflowRequiresBothNull fixes the rule verified against a live MySQL:
// a NULL schema alone does not mean overflow. Statements from any connection
// without a default database — this collector's own first of all — are
// recorded with a NULL schema and a real digest, and must be discarded rather
// than counted as overflow or as application traffic.
func TestOverflowRequiresBothNull(t *testing.T) {
	collectorRow := func(count uint64) []any {
		// What the collector's own statements look like in the digest table.
		return digestRow(nil, "ccc", DigestRow{CountStar: count, TimerWait: count * 10})
	}

	cases := []struct {
		name string
		// baseOverflow and finalOverflow are the both-NULL row's call counts.
		baseOverflow, finalOverflow uint64
		wantDetected                bool
	}{
		{name: "overflow row unchanged", baseOverflow: 7, finalOverflow: 7},
		{name: "overflow row grew", baseOverflow: 7, finalOverflow: 19, wantDetected: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer()
			server.digests = [][]any{
				digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 10}),
				collectorRow(4),
				digestRow(nil, nil, DigestRow{CountStar: tc.baseOverflow}),
			}
			server.texts = [][]any{{"aaa", "SELECT ? FROM `posts`"}}
			q := server.querier()
			c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

			base, err := c.CaptureBaseline(context.Background(), "run-1", 1)
			if err != nil {
				t.Fatalf("CaptureBaseline: %v", err)
			}
			target := sampleOfResult(t, base).Targets["db1"]
			if len(target.Digests) != 1 {
				t.Fatalf("digests = %v, want only the schema's own row", target.Digests)
			}
			if _, ok := target.Digests["ccc"]; ok {
				t.Fatal("a NULL-schema row with a real digest was counted as application traffic")
			}
			if !target.HasOverflow || target.Overflow.CountStar != tc.baseOverflow {
				t.Fatalf("overflow = %+v (present=%v), want the both-NULL row",
					target.Overflow, target.HasOverflow)
			}

			// The collector's own digest grows a lot during the interval; the
			// overflow row may or may not.
			q.answer(digestRows,
				digestRow("isuconp", "aaa", DigestRow{CountStar: 9, TimerWait: 90}),
				collectorRow(400),
				digestRow(nil, nil, DigestRow{CountStar: tc.finalOverflow}),
			)
			final, err := c.CaptureFinal(context.Background(), "run-1", 1)
			if err != nil {
				t.Fatalf("CaptureFinal: %v", err)
			}

			value, err := c.Collect(base.Handle, final.Handle)
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			section := value.(*Section)
			shown := findTarget(t, section, "db1")
			for _, stat := range shown.Digests {
				if stat.Digest == "ccc" {
					t.Fatal("a NULL-schema digest reached the section")
				}
			}
			if shown.Overflow.Detected != tc.wantDetected {
				t.Fatalf("overflow detected = %v, want %v (%+v)",
					shown.Overflow.Detected, tc.wantDetected, shown.Overflow)
			}
			if _, ok := hasHealth(section, HealthOverflow); ok != tc.wantDetected {
				t.Fatalf("overflow health present = %v, want %v: %+v", ok, tc.wantDetected, section.Health)
			}
		})
	}
}

// TestForeignSchemaRowsAreDiscarded keeps the multi-schema guarantee: the
// digest table's primary key is (SCHEMA_NAME, DIGEST), so the same digest can
// exist for another database and must not be joined onto this target.
func TestForeignSchemaRowsAreDiscarded(t *testing.T) {
	server := newServer()
	server.digests = [][]any{
		digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 10}),
		digestRow("other", "aaa", DigestRow{CountStar: 999, TimerWait: 999}),
		digestRow("other", "bbb", DigestRow{CountStar: 999, TimerWait: 999}),
	}
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": server.querier()})

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	target := sampleOfResult(t, res).Targets["db1"]
	if len(target.Digests) != 1 {
		t.Fatalf("digests = %v, want only the bound schema's rows", target.Digests)
	}
	if got := target.Digests["aaa"].CountStar; got != 1 {
		t.Fatalf("aaa count = %d, want the bound schema's row, not another schema's", got)
	}
}

// TestDigestTextFetchIsRankedAndCapped covers what the closing boundary asks
// for: the leading digests of the interval, never more than the display limit.
func TestDigestTextFetchIsRankedAndCapped(t *testing.T) {
	const total = 250
	baseRows := make([][]any, 0, total)
	finalRows := make([][]any, 0, total)
	textRows := make([][]any, 0, total)
	for i := 0; i < total; i++ {
		digest := fmt.Sprintf("d%03d", i)
		baseRows = append(baseRows, digestRow("isuconp", digest, DigestRow{CountStar: 1, TimerWait: 1}))
		finalRows = append(finalRows, digestRow("isuconp", digest, DigestRow{CountStar: 2, TimerWait: uint64(1 + i*10)}))
		textRows = append(textRows, []any{digest, "SELECT ? FROM `t`"})
	}
	server := newServer()
	server.digests = baseRows
	server.texts = textRows
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	q.answer(digestRows, finalRows...)
	q.reset()
	final, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}

	args := q.argsFor(digestTextQuery(DigestTextFetchLimit))
	if len(args) != DigestTextFetchLimit+1 {
		t.Fatalf("digest text read asked for %d values, want the schema plus %d digests",
			len(args), DigestTextFetchLimit)
	}
	if args[0] != "isuconp" {
		t.Fatalf("first argument = %v, want the schema", args[0])
	}
	if args[1] != "d249" {
		t.Fatalf("first digest = %v, want the heaviest of the interval", args[1])
	}
	texts := sampleOfResult(t, final).Targets["db1"].Texts
	if len(texts) != total {
		// The scripted server answers with every text it knows; what matters
		// is that the collector asked for the capped, ranked set.
		t.Logf("server returned %d texts", len(texts))
	}
}

// TestDigestTextIsSkippedWithoutABaseline covers the preempted-run path: the
// closing boundary has nothing to rank against, so it must not guess.
func TestDigestTextIsSkippedWithoutABaseline(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 9, TimerWait: 90})}
	server.texts = [][]any{{"aaa", "SELECT ? FROM `posts`"}}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	final, err := c.CaptureFinal(context.Background(), "run-orphan", 1)
	if err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}
	for _, stmt := range q.statements() {
		if strings.HasPrefix(stmt, digestTextPrefix) {
			t.Fatalf("digest texts were fetched without a baseline: %q", stmt)
		}
	}
	if texts := sampleOfResult(t, final).Targets["db1"].Texts; len(texts) != 0 {
		t.Fatalf("texts = %v, want none", texts)
	}

	// The section then names the digest without inventing a statement for it.
	base := runctl.NewBaselineHandle("run-orphan", 1, Name, runctl.PhaseStartBaseline, baseTime,
		sampleWith(capturedTarget("db1", map[string]DigestRow{})))
	value, err := c.Collect(base, final.Handle)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	stat := findStat(t, findTarget(t, value.(*Section), "db1"), "aaa")
	if stat.Query != MissingQueryText || stat.Kind != KindOther {
		t.Fatalf("stat = %+v, want the placeholder text classified as other", stat)
	}
}

// TestTargetDropIsDeterministic pins which targets are lost when the boundary
// budget runs out: always the tail in TargetID order, never a different set
// each run.
func TestTargetDropIsDeterministic(t *testing.T) {
	const targets = 20
	ids := make([]string, 0, targets)
	queriers := map[string]*fakeQuerier{}
	for i := 1; i <= targets; i++ {
		id := fmt.Sprintf("db%02d", i)
		ids = append(ids, id)
		server := newServer()
		server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
		queriers[id] = server.querier()
	}
	infos := targetInfos("isuconp", ids...)

	var first []string
	for attempt := 0; attempt < 20; attempt++ {
		c := testCollector(infos, queriers)
		c.concurrency = 4
		c.perTarget = time.Second
		// Each wave check advances the clock by a whole per-target budget, so
		// exactly two waves fit into the deadline below.
		c.now = stepClock(time.Now(), time.Second)

		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		res, err := c.CaptureBaseline(ctx, fmt.Sprintf("run-%d", attempt), runctl.Epoch(attempt+1))
		cancel()
		if err != nil {
			t.Fatalf("CaptureBaseline: %v", err)
		}

		sample := sampleOfResult(t, res)
		var dropped []string
		for _, id := range ids {
			target := sample.Targets[id]
			if target == nil {
				t.Fatalf("target %q vanished from the sample entirely", id)
			}
			if !target.Captured {
				if target.Code != CodeBudgetExhausted {
					t.Fatalf("target %q was not captured for the wrong reason: %q", id, target.Code)
				}
				dropped = append(dropped, id)
			}
		}
		if len(dropped) != targets-8 {
			t.Fatalf("attempt %d dropped %d targets (%v), want the tail after two waves of four",
				attempt, len(dropped), dropped)
		}
		if first == nil {
			first = dropped
			continue
		}
		if strings.Join(first, ",") != strings.Join(dropped, ",") {
			t.Fatalf("dropped set changed between attempts: %v then %v", first, dropped)
		}
	}
	if first[0] != "db09" {
		t.Fatalf("drops start at %q, want the tail to start at db09", first[0])
	}
}

// TestTargetDropIsRecorded checks that a dropped target stays visible: in the
// sample, in the health notes, and in the run's verdict.
func TestTargetDropIsRecorded(t *testing.T) {
	ids := []string{"db1", "db2", "db3"}
	queriers := map[string]*fakeQuerier{}
	for _, id := range ids {
		server := newServer()
		server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
		queriers[id] = server.querier()
	}
	c := testCollector(targetInfos("isuconp", ids...), queriers)
	c.concurrency = 1
	c.perTarget = time.Second
	c.now = stepClock(time.Now(), time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	res, err := c.CaptureBaseline(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	if !res.Committed {
		t.Fatal("a partially sampled boundary must still commit")
	}
	sample := sampleOfResult(t, res)
	if sample.Targets["db1"] == nil || !sample.Targets["db1"].Captured {
		t.Fatal("the first wave was not captured")
	}
	for _, id := range []string{"db2", "db3"} {
		target := sample.Targets[id]
		if target == nil || target.Code != CodeBudgetExhausted {
			t.Fatalf("target %q = %+v, want a recorded budget-exhausted entry", id, target)
		}
	}

	section := buildSection(sample, sample)
	if section.Validity != runctl.ValidityPartial {
		t.Fatalf("validity = %q, want %q", section.Validity, runctl.ValidityPartial)
	}
	message, ok := hasHealth(section, HealthTargetDropped)
	if !ok || message != "db2, db3 ("+CodeBudgetExhausted+")" {
		t.Fatalf("dropped health = %q (present=%v)", message, ok)
	}
}

// TestTargetWithoutSchemaIsSkippedWithoutConnecting keeps the registry's rule:
// with no schema there is nothing to bind, and guessing one would measure a
// different database.
func TestTargetWithoutSchemaIsSkippedWithoutConnecting(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	infos := []sqlstats.TargetInfo{
		{ID: "db1", Driver: "mysql", Schema: "isuconp"},
		{ID: "db2", Driver: "mysql"},
	}
	c := testCollector(infos, queriers)

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	// db2 has no scripted connection at all: reaching for one would fail the
	// fake, which is the point.
	target := sampleOfResult(t, res).Targets["db2"]
	if target == nil || target.Code != CodeNoSchema || target.Captured {
		t.Fatalf("db2 = %+v, want a recorded no-schema skip", target)
	}
	if !res.Committed {
		t.Fatal("one measurable target is enough to commit the boundary")
	}
}

// TestInspectFailureIsRecordedAsAQueryError covers an unreachable target.
func TestInspectFailureIsRecordedAsAQueryError(t *testing.T) {
	c := New()
	c.targets = func() []sqlstats.TargetInfo { return targetInfos("isuconp", "db1") }
	c.inspect = func(context.Context, string, sqlstats.Purpose, func(context.Context, sqlstats.Querier) error) error {
		return errors.New("dial tcp 127.0.0.1:3306: connect: connection refused")
	}

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if !errors.Is(err, ErrNoTargetCaptured) {
		t.Fatalf("err = %v, want %v", err, ErrNoTargetCaptured)
	}
	if res.Committed {
		t.Fatal("a boundary that sampled nothing must not commit")
	}
	target := sampleOfResult(t, res).Targets["db1"]
	if target.Code != CodeQueryError || !strings.Contains(target.Err, "connection refused") {
		t.Fatalf("db1 = %+v, want the connection error recorded", target)
	}
}

// TestDigestReadFailureIsRecorded covers a target that connects but cannot be
// read.
func TestDigestReadFailureIsRecorded(t *testing.T) {
	server := newServer()
	q := server.querier()
	q.fail(digestRows, errors.New("SELECT command denied"))
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if !errors.Is(err, ErrNoTargetCaptured) {
		t.Fatalf("err = %v, want %v", err, ErrNoTargetCaptured)
	}
	target := sampleOfResult(t, res).Targets["db1"]
	if target.Code != CodeQueryError || !strings.Contains(target.Err, "SELECT command denied") {
		t.Fatalf("db1 = %+v, want the read failure recorded", target)
	}
}

// TestMetadataAndClockFailuresAreRecorded covers the remaining boundary reads.
func TestMetadataAndClockFailuresAreRecorded(t *testing.T) {
	cases := []struct {
		name      string
		breakStmt string
		useSHOW   bool
		want      string
	}{
		{name: "metadata read", breakStmt: metaPFS, want: "server metadata"},
		{name: "uptime read on the SHOW route", breakStmt: uptimeSHOW, useSHOW: true, want: "uptime"},
		{name: "closing clock read", breakStmt: clockAfter, want: "database clock"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer()
			server.uptimeFails = tc.useSHOW
			q := server.querier()
			q.fail(tc.breakStmt, errors.New("boom"))
			c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

			res, _ := c.CaptureBaseline(context.Background(), "run-1", 1)
			target := sampleOfResult(t, res).Targets["db1"]
			if target.Code != CodeQueryError || !strings.Contains(target.Err, tc.want) {
				t.Fatalf("db1 = %+v, want a query-error mentioning %q", target, tc.want)
			}
		})
	}
}

// TestShowRouteReadsUptime checks the fallback path end to end, including the
// row-matching SHOW GLOBAL STATUS requires.
func TestShowRouteReadsUptime(t *testing.T) {
	server := newServer()
	server.uptimeFails = true
	server.uptime = 4242
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	q := server.querier()
	// SHOW returns other variables too when the pattern is loose.
	q.answer(uptimeSHOW, []any{"Uptime_since_flush_status", int64(1)}, []any{"Uptime", []byte("4242")})
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	target := sampleOfResult(t, res).Targets["db1"]
	if target.UptimeSec != 4242 {
		t.Fatalf("uptime = %d, want 4242", target.UptimeSec)
	}
	if !target.Captured {
		t.Fatalf("target was not captured: %+v", target)
	}
}

// TestPanicInMeasurementDegradesOneTarget is the fail-open guarantee:
// measurement may lose a target, never take the instrumented application down
// with it.
func TestPanicInMeasurementDegradesOneTarget(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	c := testCollector(targetInfos("isuconp", "db1", "db2"), map[string]*fakeQuerier{"db2": server.querier()})
	healthy := c.inspect
	c.inspect = func(ctx context.Context, id string, purpose sqlstats.Purpose, fn func(context.Context, sqlstats.Querier) error) error {
		if id == "db1" {
			panic("driver blew up")
		}
		return healthy(ctx, id, purpose, fn)
	}

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	sample := sampleOfResult(t, res)
	if got := sample.Targets["db1"]; got == nil || got.Code != CodeQueryError || !strings.Contains(got.Err, "panic") {
		t.Fatalf("db1 = %+v, want the panic recorded as a query error", got)
	}
	if got := sample.Targets["db2"]; got == nil || !got.Captured {
		t.Fatalf("db2 = %+v, want the healthy target still measured", got)
	}
	if !res.Committed {
		t.Fatal("one healthy target is enough to commit the boundary")
	}
}

// TestDigestTextFailureKeepsTheInterval: the text is descriptive, the counters
// are the measurement.
func TestDigestTextFailureKeepsTheInterval(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	base, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	q.answer(digestRows, digestRow("isuconp", "aaa", DigestRow{CountStar: 9, TimerWait: 90}))
	q.fail(digestTextPrefix, errors.New("digest text read failed"))

	final, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}
	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	stat := findStat(t, findTarget(t, value.(*Section), "db1"), "aaa")
	if stat.Count != 8 {
		t.Fatalf("count = %d, want the interval kept despite the missing text", stat.Count)
	}
	if stat.Query != MissingQueryText {
		t.Fatalf("query = %q, want %q", stat.Query, MissingQueryText)
	}
}
