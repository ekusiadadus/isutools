package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/httpstats"
)

func TestEndpointWorkCoverageAndSavedReport(t *testing.T) {
	snap := Snapshot{HTTP: httpstats.Snapshot{
		{Method: "GET", Path: "/orders", Count: 2, Work: &httpstats.RequestWork{TrackedRequests: 2, CompleteRequests: 1, TruncatedRequests: 1, Total: 20 * time.Millisecond, CompleteTotal: 10 * time.Millisecond, ExternalOnly: 6 * time.Millisecond, Other: 4 * time.Millisecond, ExternalCount: 4, ExternalTotal: 12 * time.Millisecond, Calls: []httpstats.RequestCall{{Label: "<shipping>", Count: 4, Total: 12 * time.Millisecond}}}},
		{Method: "GET", Path: "/orders", Count: 1, Status: 500, Work: &httpstats.RequestWork{TrackedRequests: 1, CompleteRequests: 1, Total: 10 * time.Millisecond, CompleteTotal: 10 * time.Millisecond, SQLOnly: time.Millisecond, Other: 9 * time.Millisecond}},
		{Method: "GET", Path: "/orders", Count: 3},
		{Method: "GET", Path: "/legacy", Count: 5},
	}}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var saved Snapshot
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	rows := endpointWorkRows(saved.HTTP)
	r := rows[0]
	if r.Requests != 6 || r.Work.TrackedRequests != 3 || r.Work.CompleteRequests != 2 || r.Work.TruncatedRequests != 1 || workAverage(r.Work.ExternalTotal, r.Work.TrackedRequests) != 4*time.Millisecond {
		t.Fatalf("wrong denominators: %+v", r)
	}
	body := renderReport(t, saved)
	section := strings.Split(strings.Split(body, `id="endpoint-work-table"`)[1], "</table>")[0]
	legacy := rowContaining(t, section, "GET /legacy")
	if strings.Count(legacy, "<td>—</td>") != 10 {
		t.Fatalf("legacy looks measured: %s", legacy)
	}
	if !strings.Contains(body, "&lt;shipping&gt;") || strings.Contains(body, "<shipping>") {
		t.Fatal("label not escaped")
	}
	if saved.HTTP[3].Work != nil {
		t.Fatal("old JSON lost missing state")
	}
}
