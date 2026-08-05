package queryplan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPrivilegeVerificationSkipsUnsafeCredentials is the security matrix.
//
// The case that matters most is the role-granted DML one: SET ROLE NONE
// succeeds and CURRENT_ROLE() reports NONE, so neutralisation worked — and the
// target is still skipped, because the role's own privileges were expanded and
// judged. Neutralisation protects this session; expansion protects against the
// next connection pool, proxy or server setting that quietly re-activates the
// role.
func TestPrivilegeVerificationSkipsUnsafeCredentials(t *testing.T) {
	tests := []struct {
		name     string
		server   func(*fakeServer)
		wantCode string
	}{
		{
			name: "dml through a role, with the role neutralised",
			server: func(s *fakeServer) {
				s.grants = append(append([]string(nil), leastPrivilegeGrants...),
					"GRANT `r_dml`@`%` TO `isutools_explain`@`%`")
				s.grantsUsing = []string{
					"GRANT USAGE ON *.* TO `isutools_explain`@`%`",
					"GRANT SELECT ON `isuconp`.* TO `isutools_explain`@`%`",
					"GRANT INSERT, UPDATE, DELETE ON `isuconp`.* TO `isutools_explain`@`%`",
				}
			},
			wantCode: CodeGrantsTooBroad,
		},
		{
			name: "execute through a role",
			server: func(s *fakeServer) {
				s.grants = append(append([]string(nil), leastPrivilegeGrants...),
					"GRANT `r_exec`@`%` TO `isutools_explain`@`%`")
				s.grantsUsing = []string{
					"GRANT USAGE ON *.* TO `isutools_explain`@`%`",
					"GRANT EXECUTE ON `isuconp`.* TO `isutools_explain`@`%`",
				}
			},
			wantCode: CodeGrantsTooBroad,
		},
		{
			name: "select on every schema",
			server: func(s *fakeServer) {
				s.grants = []string{"GRANT SELECT ON *.* TO `isutools_explain`@`%`"}
			},
			wantCode: CodeGrantsTooBroad,
		},
		{
			name: "a grant line the parser does not understand",
			server: func(s *fakeServer) {
				s.grants = append(append([]string(nil), leastPrivilegeGrants...),
					"REVOKE SELECT ON `mysql`.* FROM `isutools_explain`@`%`")
			},
			wantCode: CodeGrantsUnverifiable,
		},
		{
			name:     "SHOW GRANTS cannot be read",
			server:   func(s *fakeServer) { s.grantsErr = errCanned },
			wantCode: CodeGrantsUnverifiable,
		},
		{
			// Every MySQL account has at least GRANT USAGE ON *.*, so an empty
			// SHOW GRANTS is a read that did not happen. Judged as an account
			// with no privileges it would pass the allowlist vacuously, which
			// is the one way this check could fail open.
			name:     "SHOW GRANTS returns nothing at all",
			server:   func(s *fakeServer) { s.grants = nil },
			wantCode: CodeGrantsUnverifiable,
		},
		{
			// The same absence, one statement later: the account holds a role,
			// and the expansion that would say what the role carries comes back
			// empty. Nothing about that role has been established.
			name: "the role expansion returns nothing at all",
			server: func(s *fakeServer) {
				s.grants = append(append([]string(nil), leastPrivilegeGrants...),
					"GRANT `r_ro`@`%` TO `isutools_explain`@`%`")
				s.grantsUsing = nil
			},
			wantCode: CodeGrantsUnverifiable,
		},
		{
			// The account holds a role and the statement that would say what
			// the role carries fails. Unverified is not unprivileged.
			name: "the role expansion cannot be read",
			server: func(s *fakeServer) {
				s.grants = append(append([]string(nil), leastPrivilegeGrants...),
					"GRANT `r_ro`@`%` TO `isutools_explain`@`%`")
				s.grantsUsingErr = errCanned
			},
			wantCode: CodeGrantsUnverifiable,
		},
		{
			name: "roles stay active and cannot be enumerated",
			server: func(s *fakeServer) {
				s.setRoleErr = errCanned
				s.currentRole = "something the parser cannot read"
			},
			wantCode: CodeRolesActive,
		},
		{
			// MySQL 8 answers CURRENT_ROLE() with the literal string NONE when
			// nothing is active. A NULL is not that answer: it is a server, a
			// proxy or a driver that did not answer the question, and "no roles
			// are active" may only be concluded from the positive reply.
			name:     "CURRENT_ROLE() answers NULL",
			server:   func(s *fakeServer) { s.currentRole = nil },
			wantCode: CodeRolesActive,
		},
		{
			name:     "CURRENT_ROLE() answers an empty string",
			server:   func(s *fakeServer) { s.currentRole = "" },
			wantCode: CodeRolesActive,
		},
		{
			name: "the session cannot be un-instrumented",
			server: func(s *fakeServer) {
				s.instrumented = "YES"
			},
			wantCode: CodeSessionInstrumented,
		},
		{
			name: "the server has no QUERY_SAMPLE_TEXT column",
			server: func(s *fakeServer) {
				s.hasSampleColumn = false
			},
			wantCode: CodeUnsupported,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
			tc.server(server)
			queriers := map[string]*fakeQuerier{"db1": server.querier()}
			rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

			section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			target, _ := findTarget(section, "db1")
			if target.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", target.Code, tc.wantCode)
			}
			if len(target.Plans) != 0 || target.Explained {
				t.Fatalf("target = %+v, want no plans at all", target)
			}
			if _, ok := noteFor(section, tc.wantCode); !ok {
				t.Fatalf("health = %+v, want a note for %q", section.Health, tc.wantCode)
			}
			if got := queriers["db1"].countWithPrefix(explainPrefix); got != 0 {
				t.Fatalf("%d EXPLAIN statements ran on a session that was not proven safe", got)
			}
			// Nothing may reach the measured schema either: USE is the
			// statement that would give this session a default database, and
			// with it a way into the measured schema's digests.
			if got := queriers["db1"].countWithPrefix("USE "); got != 0 {
				t.Fatal("USE ran on a session that was not proven safe")
			}
		})
	}
}

// TestRolesThatCannotBeNeutralisedAreStillJudged covers the fallback path: an
// account whose roles stay active is not skipped out of hand, because active
// roles inside the allowlist are safe. The expansion is what decides.
func TestRolesThatCannotBeNeutralisedAreStillJudged(t *testing.T) {
	tests := []struct {
		name        string
		grantsUsing []string
		wantCode    string
	}{
		{
			name: "the active role only carries SELECT",
			grantsUsing: []string{
				"GRANT USAGE ON *.* TO `isutools_explain`@`%`",
				"GRANT SELECT ON `isuconp`.* TO `isutools_explain`@`%`",
			},
			wantCode: "",
		},
		{
			name: "the active role carries DML",
			grantsUsing: []string{
				"GRANT INSERT ON `isuconp`.* TO `isutools_explain`@`%`",
			},
			wantCode: CodeGrantsTooBroad,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
			server.setRoleErr = errCanned
			server.currentRole = "`r_ro`@`%`"
			server.grantsUsing = tc.grantsUsing
			queriers := map[string]*fakeQuerier{"db1": server.querier()}
			rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

			section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			target, _ := findTarget(section, "db1")
			if target.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", target.Code, tc.wantCode)
			}
			statement, ok := queriers["db1"].firstWithPrefix(stmtShowGrantsUsing)
			if !ok {
				t.Fatal("an account with active roles must have them expanded")
			}
			if !strings.Contains(statement, "`r_ro`@`%`") {
				t.Fatalf("expansion = %q, want the active role re-quoted into it", statement)
			}
		})
	}
}

// TestGrantedRolesAreExpandedEvenWhenNeutralised pins the detection half of
// the design: SET ROLE NONE worked, and the roles are expanded anyway.
func TestGrantedRolesAreExpandedEvenWhenNeutralised(t *testing.T) {
	server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
	server.grants = append(append([]string(nil), leastPrivilegeGrants...),
		"GRANT `r_ro`@`%` TO `isutools_explain`@`%`")
	server.grantsUsing = []string{"GRANT SELECT ON `isuconp`.* TO `isutools_explain`@`%`"}
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	if _, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, ok := queriers["db1"].firstWithPrefix(stmtShowGrantsUsing); !ok {
		t.Fatalf("statements = %v, want the granted role expanded", queriers["db1"].statements())
	}
}

// TestNestedRolesAreExpandedToAFixpoint pins that role expansion follows the
// chain rather than stopping after one round.
//
// r_outer carries no privilege of its own; it holds r_dml. The first
// SHOW GRANTS ... USING therefore reports nothing but a membership row, and a
// check that read the expansion once and threw away the roles it named would
// judge this account harmless — the DML would never appear in anything it
// looked at. Whether the server walks the chain unasked is not a question this
// package may answer with an assumption, so it feeds every newly named role
// back into the next USING list.
func TestNestedRolesAreExpandedToAFixpoint(t *testing.T) {
	outer := []account{{name: "r_outer", host: "%"}}
	nested := []account{{name: "r_outer", host: "%"}, {name: "r_dml", host: "%"}}

	server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
	server.grants = append(append([]string(nil), leastPrivilegeGrants...),
		"GRANT `r_outer`@`%` TO `isutools_explain`@`%`")
	server.grantsUsingChain = map[string][]string{
		grantsUsing(outer): {
			"GRANT USAGE ON *.* TO `isutools_explain`@`%`",
			"GRANT `r_dml`@`%` TO `r_outer`@`%`",
		},
		grantsUsing(nested): {
			"GRANT USAGE ON *.* TO `isutools_explain`@`%`",
			"GRANT INSERT, UPDATE, DELETE ON `isuconp`.* TO `isutools_explain`@`%`",
		},
	}
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	if target.Code != CodeGrantsTooBroad {
		t.Fatalf("code = %q, want %q: the DML held through r_outer must be found",
			target.Code, CodeGrantsTooBroad)
	}
	if got := queriers["db1"].countWithPrefix(stmtShowGrantsUsing); got != 2 {
		t.Fatalf("%d expansions issued, want the nested role expanded in a second round", got)
	}
	if got := queriers["db1"].countWithPrefix(explainPrefix); got != 0 {
		t.Fatalf("%d EXPLAIN statements ran on an account that reaches DML through a role chain", got)
	}
	if got := queriers["db1"].countWithPrefix("USE "); got != 0 {
		t.Fatal("USE ran on an account that reaches DML through a role chain")
	}
}

// TestARoleGraphThatNeverClosesIsUnverifiable covers the other end of the
// fixpoint: each round names one more role, so the expansion never settles. The
// bound has to fail closed — a role that was never expanded is a privilege
// nobody has looked at, which is exactly the state the allowlist may not judge.
func TestARoleGraphThatNeverClosesIsUnverifiable(t *testing.T) {
	server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
	server.grants = append(append([]string(nil), leastPrivilegeGrants...),
		"GRANT `r_1`@`%` TO `isutools_explain`@`%`")

	chain := map[string][]string{}
	using := []account{{name: "r_1", host: "%"}}
	for i := 1; i <= maxRoleExpansions; i++ {
		next := account{name: fmt.Sprintf("r_%d", i+1), host: "%"}
		chain[grantsUsing(using)] = []string{
			"GRANT " + next.quoted() + " TO `isutools_explain`@`%`",
		}
		using = append(append([]account(nil), using...), next)
	}
	server.grantsUsingChain = chain
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	target, _ := findTarget(section, "db1")
	if target.Code != CodeGrantsUnverifiable {
		t.Fatalf("code = %q, want %q", target.Code, CodeGrantsUnverifiable)
	}
	// Every round the bound allows was spent on the chain, and it still had
	// not closed: the statement count is what proves the loop is bounded
	// rather than the expansion having failed for some other reason.
	if got := queriers["db1"].countWithPrefix(stmtShowGrantsUsing); got != maxRoleExpansions {
		t.Fatalf("%d expansions issued, want the loop bounded at %d", got, maxRoleExpansions)
	}
	if got := queriers["db1"].countWithPrefix(explainPrefix); got != 0 {
		t.Fatalf("%d EXPLAIN statements ran with roles left unexpanded", got)
	}
}

// TestNoSampleTextReachesTheSection is the leak test the design turns on.
//
// The sample is a marker with no overlap with anything the section legitimately
// contains, and it is fed through the four failure shapes that carry a driver
// message: permission denied, a syntax error (MySQL's 1064 quotes part of the
// statement back), a missing object, and a timeout. Every substring of the
// sample four characters and longer has to be absent from the marshalled
// section — an equality check would miss a driver that truncates or escapes
// what it echoes.
func TestNoSampleTextReachesTheSection(t *testing.T) {
	const marker = "ZZ_ISUTOOLS_MARKER_ZZ"
	tests := []struct {
		name      string
		err       error
		wantClass PlanErrorClass
	}{
		{
			name:      "permission denied",
			err:       driverErrorf(1142, "42000", "SELECT command denied for '"+marker+"'"),
			wantClass: PlanErrPermission,
		},
		{
			name:      "syntax error quoting the statement",
			err:       driverErrorf(1064, "42000", "check the manual near '"+marker+"' at line 1"),
			wantClass: PlanErrSyntax,
		},
		{
			name:      "object missing",
			err:       driverErrorf(1146, "42S02", "Table 'isuconp."+marker+"' doesn't exist"),
			wantClass: PlanErrObjectMissing,
		},
		{
			name:      "timeout",
			err:       fmt.Errorf("inspect query near '%s': %w", marker, context.DeadlineExceeded),
			wantClass: PlanErrTimeout,
		},
		{
			name:      "no failure at all",
			err:       nil,
			wantClass: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer().withSample("d1", marker, baseTime.Add(10*time.Second))
			server.explainErr = tc.err
			queriers := map[string]*fakeQuerier{"db1": server.querier()}
			rows := interval(usableTarget("db1", selectStat("d1", "SELECT ? FROM posts WHERE id = ?", 100)))

			section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			plan := mustPlan(t, section, "db1", 0)
			switch {
			case tc.wantClass == "" && plan.Err != nil:
				t.Fatalf("plan error = %+v, want none", *plan.Err)
			case tc.wantClass != "" && (plan.Err == nil || plan.Err.Class != tc.wantClass):
				t.Fatalf("plan error = %+v, want class %q", plan.Err, tc.wantClass)
			}
			// The statement really did carry the marker, so the assertions
			// below are not vacuous.
			if statement, ok := queriers["db1"].firstWithPrefix(explainPrefix); !ok || !strings.Contains(statement, marker) {
				t.Fatalf("the fixture never explained the marked sample: %q", statement)
			}
			encoded, err := json.Marshal(section)
			if err != nil {
				t.Fatalf("marshal section: %v", err)
			}
			assertNoSubstring(t, string(encoded), marker)

			planJSON, err := json.Marshal(plan)
			if err != nil {
				t.Fatalf("marshal plan: %v", err)
			}
			assertNoSubstring(t, string(planJSON), marker)
		})
	}
}

// assertNoSubstring fails if any substring of sample four characters or longer
// occurs in out.
func assertNoSubstring(t *testing.T, out, sample string) {
	t.Helper()
	const minRun = 4
	for i := 0; i+minRun <= len(sample); i++ {
		for j := i + minRun; j <= len(sample); j++ {
			if strings.Contains(out, sample[i:j]) {
				t.Fatalf("output carries %q, a fragment of the statement sample:\n%s", sample[i:j], out)
			}
		}
	}
}

// TestSourceNeverNamesAnotherCredential is the acceptance condition of the
// no-fallback rule, checked on the source itself: a future edit that reaches
// for the application or stats credential when the explain one is missing has
// to fail here. EXPLAIN under a credential holding DML can have side effects
// through a stored function, which is the whole reason for the dedicated user.
func TestSourceNeverNamesAnotherCredential(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	forbidden := []string{"Purpose" + "App", "Purpose" + "Stats"}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, symbol := range forbidden {
			if strings.Contains(string(source), symbol) {
				t.Fatalf("%s names %s: EXPLAIN must never fall back to another credential", name, symbol)
			}
		}
	}
}

// TestReasonsAreClosedText pins that every reason a target can carry comes
// from the fixed table. A reason built from a driver error would be the same
// leak the PlanError design closes, arriving through a different field.
func TestReasonsAreClosedText(t *testing.T) {
	for code, reason := range reasons {
		if reason == "" {
			t.Fatalf("reason ID %q has no sentence", code)
		}
	}
	codes := []string{
		CodePurposeUnregistered, CodeUnknownTarget, CodeNoSchema, CodeUnsupported,
		CodeRolesActive, CodeGrantsTooBroad, CodeGrantsUnverifiable, CodeSessionInstrumented,
		CodeBudgetExhausted, CodeTargetTimeout, CodeQueryError, CodeNoInterval, CodeNoDigests,
		CodeNoDefaultDatabase,
	}
	for _, code := range codes {
		if _, ok := reasons[code]; !ok {
			t.Fatalf("reason ID %q has no entry in the reason table", code)
		}
		if !strings.HasPrefix(code, "explain-") {
			t.Fatalf("reason ID %q does not carry the explain- prefix the dashboard keys on", code)
		}
	}
	if len(reasons) != len(codes) {
		t.Fatalf("the reason table has %d entries for %d reason IDs", len(reasons), len(codes))
	}
}

func TestErrCannedIsUsed(t *testing.T) {
	// Guards the fixture itself: errCanned must stay a plain error so the
	// classification tests exercise the default branch.
	if errors.Is(errCanned, context.DeadlineExceeded) {
		t.Fatal("errCanned must not carry a sentinel")
	}
}
