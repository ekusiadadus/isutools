package queryplan

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionFailuresSkipTheTarget(t *testing.T) {
	tests := []struct {
		name     string
		server   func(*fakeServer)
		wantCode string
	}{
		{
			name:     "the default database cannot be selected",
			server:   func(s *fakeServer) { s.useErr = errCanned },
			wantCode: CodeNoSchema,
		},
		{
			name:     "the sample read fails",
			server:   func(s *fakeServer) { s.sampleErr = errCanned },
			wantCode: CodeQueryError,
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
			if got := queriers["db1"].countWithPrefix(explainPrefix); got != 0 {
				t.Fatalf("%d EXPLAIN statements ran after a failed session step", got)
			}
		})
	}
}

// TestUnreadableTextLengthAssumesTheDefault: not knowing where the server
// truncates is a reason to assume the documented default, which makes long
// samples look truncated and be skipped rather than sent half-formed.
func TestUnreadableTextLengthAssumesTheDefault(t *testing.T) {
	server := newServer().
		withSample("d1", strings.Repeat("a", defaultMaxSQLTextLength), baseTime.Add(10*time.Second))
	server.maxLengthErr = errCanned
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plan := mustPlan(t, section, "db1", 0)
	if plan.Err == nil || plan.Err.Class != PlanErrSampleTruncated {
		t.Fatalf("plan error = %+v, want the sample treated as truncated", plan.Err)
	}
}

// TestZeroTextLengthAssumesTheDefault covers a server that answers the
// variable with a nonsense value.
func TestZeroTextLengthAssumesTheDefault(t *testing.T) {
	server := newServer().
		withSample("d1", strings.Repeat("a", defaultMaxSQLTextLength-1), baseTime.Add(10*time.Second))
	server.maxTextLength = 0
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plan := mustPlan(t, section, "db1", 0)
	if plan.Err != nil {
		t.Fatalf("a sample below the assumed default must still be explained, got %+v", *plan.Err)
	}
}

// TestNoDefaultDatabaseIsReportedInHealth: 1046 means the USE step did not
// take effect, which is this package's own bug rather than a property of the
// statement, so it is reported where an operator will see it.
func TestNoDefaultDatabaseIsReportedInHealth(t *testing.T) {
	server := newServer().withSample("d1", "SELECT id FROM posts", baseTime.Add(10*time.Second))
	server.explainErr = driverErrorf(errnoNoDatabase, "3D000", "No database selected")
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plan := mustPlan(t, section, "db1", 0)
	if plan.Err == nil || plan.Err.Class != PlanErrObjectMissing || plan.Err.Errno != errnoNoDatabase {
		t.Fatalf("plan error = %+v, want object_missing with errno 1046", plan.Err)
	}
	if _, ok := noteFor(section, CodeNoDefaultDatabase); !ok {
		t.Fatalf("health = %+v, want the lost-default-database note", section.Health)
	}
}

func TestExplainResultEdges(t *testing.T) {
	tests := []struct {
		name      string
		columns   []string
		rows      [][]any
		wantRows  int
		wantClass PlanErrorClass
	}{
		{
			name:      "a result set with no columns is not a plan",
			columns:   nil,
			rows:      nil,
			wantClass: PlanErrOther,
		},
		{
			name:     "unknown columns are ignored rather than rejected",
			columns:  []string{"id", "select_type", "table", "some_future_column"},
			rows:     [][]any{{int64(1), "SIMPLE", "posts", "whatever"}},
			wantRows: 1,
		},
		{
			name:    "more rows than a snapshot should carry",
			columns: explainColumns,
			rows:    repeatRow(fullScanRow("posts", 10, "Using where"), maxPlanRows+5),
			// The cap is what keeps a UNION of hundreds of branches from
			// inflating the snapshot.
			wantRows: maxPlanRows,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
			queriers := map[string]*fakeQuerier{"db1": server.querier()}
			queriers["db1"].answerCols(explainPrefix, tc.columns, tc.rows...)
			rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

			section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			plan := mustPlan(t, section, "db1", 0)
			if tc.wantClass != "" {
				if plan.Err == nil || plan.Err.Class != tc.wantClass {
					t.Fatalf("plan error = %+v, want class %q", plan.Err, tc.wantClass)
				}
				return
			}
			if plan.Err != nil {
				t.Fatalf("plan error = %+v, want none", *plan.Err)
			}
			if len(plan.Rows) != tc.wantRows {
				t.Fatalf("rows = %d, want %d", len(plan.Rows), tc.wantRows)
			}
		})
	}
}

// TestPlanCellsAreBounded: EXPLAIN's Extra is fixed vocabulary in practice,
// but a snapshot is held in memory and published, so one cell cannot be
// allowed to grow without bound.
func TestPlanCellsAreBounded(t *testing.T) {
	server := newServer().withSample("d1", "SELECT 1", baseTime.Add(10*time.Second))
	server.explainRows = [][]any{fullScanRow("posts", 1, strings.Repeat("x", maxPlanCell*2))}
	queriers := map[string]*fakeQuerier{"db1": server.querier()}
	rows := interval(usableTarget("db1", selectStat("d1", "SELECT ?", 100)))

	section, err := Capture(context.Background(), Input{Rows: rows, Inspect: fakeInspect(queriers)})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	plan := mustPlan(t, section, "db1", 0)
	if plan.Rows[0].Extra == nil || len(*plan.Rows[0].Extra) != maxPlanCell {
		t.Fatalf("extra length = %v, want it capped at %d", plan.Rows[0].Extra, maxPlanCell)
	}
}

func repeatRow(row []any, n int) [][]any {
	out := make([][]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, row)
	}
	return out
}
