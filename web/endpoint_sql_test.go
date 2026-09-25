package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
)

func TestEndpointSQLWeightedAggregation(t *testing.T) {
	entries := httpstats.Snapshot{
		{Method: "GET", Path: "/posts/:id", Status: 200, Protocol: "HTTP/1.1", Count: 9, SQLTrackedRequests: 9, SQLCount: 18, SQLMaxPerRequest: 4},
		{Method: "GET", Path: "/posts/:id", Status: 500, Protocol: "HTTP/2.0", Count: 1, SQLTrackedRequests: 1, SQLCount: 12, SQLMaxPerRequest: 12},
		{Method: "POST", Path: "/posts/:id", Count: 2, SQLTrackedRequests: 2, SQLCount: 10, SQLMaxPerRequest: 5},
		{Method: "GET", Path: "/health", Count: 4, SQLTrackedRequests: 4},
		{Method: "GET", Path: "/legacy", Count: 100},
		// An untracked observation must not silently dilute the average.
		{Method: "GET", Path: "/posts/:id", Count: 10},
	}
	rows := endpointSQLRows(entries)
	if len(rows) != 4 || rows[0].Method != "POST" || rows[0].SQLPerRequest != 5 {
		t.Fatalf("unexpected sorted endpoints: %+v", rows)
	}
	got := rows[1]
	if got.Requests != 20 || got.Tracked != 10 || got.SQLCount != 30 || got.SQLPerRequest != 3 || got.SQLMax != 12 {
		t.Fatalf("incorrect weighted summary: %+v", got)
	}
	if rows[2].Path != "/health" || rows[2].SQLPerRequest != 0 || rows[3].Tracked != 0 {
		t.Fatalf("zero and missing observations confused: %+v", rows)
	}
}

func TestEndpointSQLSavedReport(t *testing.T) {
	snap := Snapshot{HTTP: httpstats.Snapshot{
		{Method: "GET", Path: "/posts/:id", Count: 4, SQLTrackedRequests: 4, SQLCount: 10, SQLMaxPerRequest: 7},
		{Method: "GET", Path: "/health", Count: 2, SQLTrackedRequests: 2},
		{Method: "GET", Path: "/old", Count: 3},
	}}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var saved Snapshot
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	body := renderReport(t, saved)
	row := rowContaining(t, body, "GET /posts/:id")
	for _, want := range []string{">4 / 4<", ">10<", ">2.50<", ">7<"} {
		if !strings.Contains(row, want) {
			t.Errorf("saved endpoint row missing %s: %s", want, row)
		}
	}
	if row := rowContaining(t, body, "GET /health"); !strings.Contains(row, ">0.00<") {
		t.Errorf("zero SQL must be displayed: %s", row)
	}
	if row := rowContaining(t, body, "GET /old"); strings.Count(row, "<td>—</td>") != 3 {
		t.Errorf("missing SQL must not look like zero: %s", row)
	}
}

func TestEndpointShapeReportAndCandidateSurviveJSON(t *testing.T) {
	snap := Snapshot{HTTP: httpstats.Snapshot{
		{Method: "GET", Path: "/search/{id}", Count: 8, SQLCount: 120, SQLShapeTrackedRequests: 8,
			SQLShapes: []httpstats.SQLShape{{Key: "SELECT available FROM seats WHERE id = ?", Count: 120, Total: 240000000, MaxPerRequest: 15}}},
		{Method: "GET", Path: "/search/{id}", Count: 2, SQLShapeTrackedRequests: 2},
	}}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var saved Snapshot
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	rows := endpointShapeRows(saved.HTTP)
	if len(rows) != 1 || rows[0].Count != 120 || rows[0].Tracked != 10 || rows[0].PerRequest != 12 {
		t.Fatalf("saved shape rows = %+v", rows)
	}
	if body := renderReport(t, saved); !strings.Contains(body, "endpoint-sql-shapes") || !strings.Contains(body, "SELECT available FROM seats WHERE id = ?") {
		t.Fatal("saved report omitted endpoint SQL shape")
	}
	if candidate, ok := candidateByTitle(diagnosticCandidates(saved), "エンドポイント別の反復SQLを調査"); !ok || !strings.Contains(candidate.Evidence, "GET /search/{id}") || !strings.Contains(candidate.Evidence, "N+1や性能律速の証明ではありません") {
		t.Fatalf("candidate = %+v, ok=%t", candidate, ok)
	}
}

func TestEndpointShapeCoverageDistinguishesOverflowZeroAndUnmeasured(t *testing.T) {
	entries := httpstats.Snapshot{
		{Method: "GET", Path: "/cap", Count: 1, SQLCount: 5, SQLShapeTrackedRequests: 1,
			SQLShapes: []httpstats.SQLShape{{Key: "SELECT ?", Count: 3}, {Key: "(other SQL shapes)", Count: 2}}},
		{Method: "GET", Path: "/zero", Count: 1, SQLShapeTrackedRequests: 1},
		{Method: "GET", Path: "/old", Count: 1, SQLCount: 5},
	}
	rows := endpointSQLRows(entries)
	byPath := make(map[string]endpointSQL)
	for _, row := range rows {
		byPath[row.Path] = row
	}
	if got := byPath["/cap"]; got.ShapeCount != 5 || got.ShapeOverflow != 2 || got.ShapeTracked != 1 {
		t.Fatalf("overflow coverage = %+v", got)
	}
	if got := byPath["/zero"]; got.ShapeTracked != 1 || got.ShapeCount != 0 {
		t.Fatalf("zero SQL coverage = %+v", got)
	}
	if got := byPath["/old"]; got.ShapeTracked != 0 || got.SQLCount != 5 {
		t.Fatalf("old snapshot coverage = %+v", got)
	}
	body := renderReport(t, Snapshot{HTTP: entries})
	if !strings.Contains(body, "endpoint-sql-shape-coverage") || !strings.Contains(body, "5 / 5") || !strings.Contains(body, "(other SQL shapes)") {
		t.Fatal("coverage or overflow missing from report")
	}
}
