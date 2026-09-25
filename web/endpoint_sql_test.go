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
