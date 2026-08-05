package sqlrows

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunProbe(t *testing.T) {
	permissionDenied := errors.New("Access denied for user 'isu'@'%'")

	cases := []struct {
		name string
		// server tweaks the scripted server; overrides tweaks the resulting
		// connection, which is how a statement is made to fail.
		server     func(s *fakeServer)
		overrides  func(q *fakeQuerier)
		want       probeResult
		wantReason string
	}{
		{
			name: "healthy server",
			want: probeResult{probed: true, supported: true, hasQuerySampleText: true},
		},
		{
			name:       "performance_schema is off",
			server:     func(s *fakeServer) { s.performance = int64(0) },
			wantReason: "performance_schema is OFF",
		},
		{
			name:       "performance_schema is off as text",
			server:     func(s *fakeServer) { s.performance = []byte("OFF") },
			wantReason: "performance_schema is OFF",
		},
		{
			name:       "the inspection connection has a default database",
			server:     func(s *fakeServer) { s.defaultSchema = []byte("isuconp") },
			want:       probeResult{probed: true, unsafeConn: true},
			wantReason: `default database "isuconp"`,
		},
		{
			name:       "the connection's default database is unknown",
			server:     func(s *fakeServer) { s.defaultSchemaAbsent = true },
			want:       probeResult{probed: true, unsafeConn: true},
			wantReason: "could not be verified",
		},
		{
			name:       "the hygiene check cannot be run",
			overrides:  func(q *fakeQuerier) { q.fail(probeDefaultSchema, permissionDenied) },
			want:       probeResult{probed: true, unsafeConn: true},
			wantReason: "could not be read",
		},
		{
			name:       "the hygiene check answers with a row it cannot scan",
			overrides:  func(q *fakeQuerier) { q.answer(probeDefaultSchema, []any{nil, nil}) },
			want:       probeResult{probed: true, unsafeConn: true},
			wantReason: "could not be read",
		},
		{
			name:       "digest consumer disabled",
			server:     func(s *fakeServer) { s.consumer = "NO" },
			wantReason: "the statements_digest consumer is disabled",
		},
		{
			name:       "digest consumer row missing",
			server:     func(s *fakeServer) { s.consumerAbsent = true },
			wantReason: "setup_consumers has no statements_digest row",
		},
		{
			name:       "digest table is missing a column",
			server:     func(s *fakeServer) { s.columns = []string{"SCHEMA_NAME", "DIGEST", "COUNT_STAR"} },
			wantReason: "SUM_ROWS_EXAMINED",
		},
		{
			name:   "query sample text is absent",
			server: func(s *fakeServer) { s.columns = append([]string(nil), requiredColumns...) },
			want:   probeResult{probed: true, supported: true},
		},
		{
			name:       "permission denied on the first probe",
			server:     func(s *fakeServer) { s.probeErr = permissionDenied },
			want:       probeResult{probed: true, failed: true},
			wantReason: "Access denied",
		},
		{
			name:       "permission denied on setup_consumers",
			overrides:  func(q *fakeQuerier) { q.fail(probeDigestConsumer, permissionDenied) },
			want:       probeResult{probed: true, failed: true},
			wantReason: "Access denied",
		},
		{
			name:       "column listing fails",
			overrides:  func(q *fakeQuerier) { q.fail(probeColumns, permissionDenied) },
			want:       probeResult{probed: true, failed: true},
			wantReason: "digest table columns",
		},
		{
			name:   "uptime falls back to SHOW",
			server: func(s *fakeServer) { s.uptimeFails = true },
			want:   probeResult{probed: true, supported: true, useSHOW: true, hasQuerySampleText: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer()
			if tc.server != nil {
				tc.server(server)
			}
			q := server.querier()
			if tc.overrides != nil {
				tc.overrides(q)
			}

			probe := runProbe(context.Background(), q)
			if probe.supported != tc.want.supported {
				t.Fatalf("supported = %v, want %v (reason %q)", probe.supported, tc.want.supported, probe.reason)
			}
			if probe.failed != tc.want.failed {
				t.Fatalf("failed = %v, want %v (reason %q)", probe.failed, tc.want.failed, probe.reason)
			}
			if probe.unsafeConn != tc.want.unsafeConn {
				t.Fatalf("unsafeConn = %v, want %v (reason %q)", probe.unsafeConn, tc.want.unsafeConn, probe.reason)
			}
			if probe.useSHOW != tc.want.useSHOW {
				t.Fatalf("useSHOW = %v, want %v", probe.useSHOW, tc.want.useSHOW)
			}
			if probe.hasQuerySampleText != tc.want.hasQuerySampleText {
				t.Fatalf("hasQuerySampleText = %v, want %v", probe.hasQuerySampleText, tc.want.hasQuerySampleText)
			}
			if !probe.probed {
				t.Fatal("the verdict is indistinguishable from an unprobed target")
			}
			if tc.wantReason != "" && !strings.Contains(probe.reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to mention %q", probe.reason, tc.wantReason)
			}
		})
	}
}

// TestProbeVerdictSeparatesSkipFromFailure fixes the distinction the Committed
// rule depends on: a server that answers "no" is skipped, a question that
// cannot be asked is a failure.
func TestProbeVerdictSeparatesSkipFromFailure(t *testing.T) {
	off := newServer()
	off.performance = int64(0)
	denied := newServer()
	denied.probeErr = errors.New("Access denied")

	cases := []struct {
		name     string
		server   *fakeServer
		wantCode string
	}{
		{name: "configuration says no", server: off, wantCode: CodeProbeSkip},
		{name: "the question cannot be asked", server: denied, wantCode: CodeQueryError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			infos := targetInfos("isuconp", "db1")
			c := testCollector(infos, map[string]*fakeQuerier{"db1": tc.server.querier()})

			res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
			sample := res.Handle.Sample().(*Sample)
			target := sample.Targets["db1"]
			if target.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q (err %q)", target.Code, tc.wantCode, target.Err)
			}
			if target.Captured {
				t.Fatal("an unmeasurable target was reported as captured")
			}
			wantCommitted := tc.wantCode == CodeProbeSkip
			if res.Committed != wantCommitted {
				t.Fatalf("committed = %v, want %v", res.Committed, wantCommitted)
			}
			if wantCommitted != (err == nil) {
				t.Fatalf("err = %v with committed = %v", err, res.Committed)
			}
		})
	}
}

// TestUnsafeConnectionSkipsOneTargetOnly fixes the fail-open side of the
// hygiene verdict: the target whose connection would contaminate the schema it
// measures loses its numbers, every other target keeps its own, the boundary
// still commits — and that connection is never asked anything again.
func TestUnsafeConnectionSkipsOneTargetOnly(t *testing.T) {
	dirty := newServer()
	dirty.defaultSchema = []byte("isuconp")
	clean := newServer()
	clean.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 10})}

	dirtyQ, cleanQ := dirty.querier(), clean.querier()
	c := testCollector(targetInfos("isuconp", "db1", "db2"),
		map[string]*fakeQuerier{"db1": dirtyQ, "db2": cleanQ})

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil || !res.Committed {
		t.Fatalf("CaptureBaseline = %+v, %v; want a committed boundary", res, err)
	}
	sample := sampleOfResult(t, res)
	if target := sample.Targets["db1"]; target.Captured || target.Code != CodeInspectorDefaultDB {
		t.Fatalf("contaminating target = %+v, want it skipped with %q", target, CodeInspectorDefaultDB)
	}
	if target := sample.Targets["db2"]; !target.Captured {
		t.Fatalf("clean target = %+v, want it measured", target)
	}

	stmts := dirtyQ.statements()
	if len(stmts) == 0 || stmts[len(stmts)-1] != probeDefaultSchema {
		t.Fatalf("statements on the contaminating connection = %v, want them to stop at the hygiene check", stmts)
	}

	dirtyQ.reset()
	if _, err := c.CaptureFinal(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}
	if repeat := dirtyQ.statements(); len(repeat) != 0 {
		t.Fatalf("the closing boundary issued %v on a connection already known to contaminate", repeat)
	}
}

// TestProbeIsCachedPerTarget pins the statement cost of probing: it is paid
// once per process, not once per boundary.
func TestProbeIsCachedPerTarget(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	if got := countStatement(q.statements(), probePerformanceSchema); got != 1 {
		t.Fatalf("first boundary probed %d times, want 1", got)
	}

	q.reset()
	if _, err := c.CaptureBaseline(context.Background(), "run-2", 2); err != nil {
		t.Fatalf("second CaptureBaseline: %v", err)
	}
	if got := countStatement(q.statements(), probePerformanceSchema); got != 0 {
		t.Fatalf("second boundary probed %d times, want the cached verdict to be reused", got)
	}
}

// TestProbeIsRepeatedWhenTheServerChanges covers the other side of the cache:
// a different server behind the same target is a different set of
// capabilities.
func TestProbeIsRepeatedWhenTheServerChanges(t *testing.T) {
	server := newServer()
	server.digests = [][]any{digestRow("isuconp", "aaa", DigestRow{CountStar: 1, TimerWait: 1})}
	q := server.querier()
	c := testCollector(targetInfos("isuconp", "db1"), map[string]*fakeQuerier{"db1": q})

	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}

	// The target now answers with a different server identity, and that server
	// has performance_schema off.
	q.reset()
	q.answer(metaPFS, []any{"uuid-2", server.uptime, server.before})
	q.answer(probePerformanceSchema, []any{int64(0)})

	res, err := c.CaptureBaseline(context.Background(), "run-2", 2)
	if err != nil {
		t.Fatalf("second CaptureBaseline: %v", err)
	}
	if got := countStatement(q.statements(), probePerformanceSchema); got != 1 {
		t.Fatalf("the target was probed %d times after changing identity, want 1", got)
	}
	target := res.Handle.Sample().(*Sample).Targets["db1"]
	if target.Code != CodeProbeSkip {
		t.Fatalf("code = %q, want the new server's verdict %q", target.Code, CodeProbeSkip)
	}
}

// countStatement counts exact occurrences of a statement.
func countStatement(stmts []string, want string) int {
	n := 0
	for _, stmt := range stmts {
		if stmt == want {
			n++
		}
	}
	return n
}
