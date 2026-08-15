package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/flowviz"
)

func TestReportRendersFunnelGraphHeatmapAndQuality(t *testing.T) {
	visualization := &flowviz.Snapshot{
		Status: flowviz.StatusPartial, Partial: true, SessionDropped: 2, TimingMissing: 1,
		Funnels: []flowviz.FunnelSnapshot{{
			ID: "checkout", Scenario: "buyer", Mode: flowviz.ModeOrdered, Entered: 10, Completed: 4, ConversionBP: 4000,
			Steps: []flowviz.FunnelStepSnapshot{
				{ID: "list", Route: "GET /items", Sessions: 10, Requests: 12, DropOff: 3, Retries: 2, RequestP95: 20 * time.Millisecond, FromStartBP: 10000, FromPreviousBP: 10000},
				{ID: "cart", Route: "POST /cart", Sessions: 7, Requests: 8, DropOff: 3, Retries: 1, Status5xx: 1, RequestP95: 40 * time.Millisecond, FromStartBP: 7000, FromPreviousBP: 7000},
				{ID: "done", Route: "POST /checkout", Sessions: 4, Requests: 4, FromStartBP: 4000, FromPreviousBP: 5714},
			},
		}},
		Graph: flowviz.GraphSnapshot{
			Nodes:     []flowviz.GraphNode{{ID: "GET /items", Count: 10}, {ID: "POST /cart", Count: 7}},
			Edges:     []flowviz.GraphEdge{{From: "GET /items", To: "POST /cart", Count: 7}},
			Truncated: true, HiddenCount: 2,
		},
	}
	h := NewHandler(Provider{AccessLog: &fakeAccessLog{current: accesslog.Snapshot{Visualization: visualization}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/snapshot.html", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Journey Funnel", "checkout", "40.00%", "drop-off", "p95", "POST /checkout",
		`aria-label="user flow graph"`, "GET /items", "POST /cart", "Transition Heatmap",
		"partial", "session dropped 2", "timing missing 1", "hidden transition count 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("flow visualization missing %q", want)
		}
	}
	if strings.Contains(body, "<script src=") || strings.Contains(body, "https://") {
		t.Error("flow visualization must be self-contained without external scripts")
	}
}

func TestFlowGraphViewIsDeterministicAndBounded(t *testing.T) {
	graph := flowviz.GraphSnapshot{
		Nodes: []flowviz.GraphNode{{ID: "GET /a", Count: 10}, {ID: "GET /b", Count: 8}, {ID: "POST /c", Count: 5}},
		Edges: []flowviz.GraphEdge{{From: "GET /a", To: "GET /b", Count: 8}, {From: "GET /b", To: "POST /c", Count: 5}},
	}
	first := buildFlowGraphView(graph)
	second := buildFlowGraphView(graph)
	if len(first.Nodes) != 3 || len(first.Edges) != 2 || first.Canonical() != second.Canonical() {
		t.Fatalf("graph view unstable: %#v %#v", first, second)
	}
	for _, node := range first.Nodes {
		if node.X < 20 || node.X > 780 || node.Y < 20 || node.Y > 480 {
			t.Errorf("node outside viewbox: %#v", node)
		}
	}
}

func TestFlowGraphRendersSelfTransitionAsVisibleLoop(t *testing.T) {
	graph := flowviz.GraphSnapshot{
		Nodes: []flowviz.GraphNode{{ID: "GET /poll", Count: 20}},
		Edges: []flowviz.GraphEdge{{From: "GET /poll", To: "GET /poll", Count: 10}},
	}
	view := buildFlowGraphView(graph)
	if len(view.Edges) != 1 || !view.Edges[0].Self || view.Edges[0].Path == "" {
		t.Fatalf("self transition is not a visible loop: %#v", view.Edges)
	}

	visualization := &flowviz.Snapshot{Status: flowviz.StatusReady, Graph: graph}
	h := NewHandler(Provider{AccessLog: &fakeAccessLog{current: accesslog.Snapshot{Visualization: visualization}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/snapshot.html", nil))
	if !strings.Contains(rec.Body.String(), `class="flow-edge self-loop"`) {
		t.Fatalf("rendered graph has no visible self-loop: %s", rec.Body.String())
	}
}

func TestFlowHeatmapUsesEveryVisibleNode(t *testing.T) {
	graph := flowviz.GraphSnapshot{
		Nodes: []flowviz.GraphNode{{ID: "GET /a", Count: 5}, {ID: "POST /b", Count: 5}},
		Edges: []flowviz.GraphEdge{{From: "GET /a", To: "POST /b", Count: 5}},
	}
	heatmap := buildFlowHeatmap(graph)
	if len(heatmap.Nodes) != 2 || len(heatmap.Rows) != 2 || len(heatmap.Rows[0].Cells) != 2 {
		t.Fatalf("heatmap = %#v", heatmap)
	}
	if heatmap.Rows[0].Cells[1].Count != 5 || heatmap.Rows[0].Cells[1].Level == 0 {
		t.Fatalf("hot cell = %#v", heatmap.Rows[0].Cells[1])
	}
}

func TestFlowViewsBoundAndEscapeStoredSnapshotData(t *testing.T) {
	graph := flowviz.GraphSnapshot{}
	for i := 0; i < flowviz.HardMaxNodes+10; i++ {
		id := fmt.Sprintf("GET /node/%d", i)
		graph.Nodes = append(graph.Nodes, flowviz.GraphNode{ID: id, Count: int64(i + 1)})
		if i > 0 {
			graph.Edges = append(graph.Edges, flowviz.GraphEdge{From: fmt.Sprintf("GET /node/%d", i-1), To: id, Count: int64(i)})
		}
	}
	view := buildFlowGraphView(graph)
	heatmap := buildFlowHeatmap(graph)
	if len(view.Nodes) > flowviz.HardMaxNodes || len(view.Edges) > flowviz.HardMaxEdges || len(heatmap.Nodes) > flowviz.HardMaxNodes {
		t.Fatalf("unbounded views: graph=%d/%d heatmap=%d", len(view.Nodes), len(view.Edges), len(heatmap.Nodes))
	}

	oversized := strings.Repeat("z", flowviz.MaxNodeBytes*4)
	visualization := &flowviz.Snapshot{Status: flowviz.StatusReady, Funnels: []flowviz.FunnelSnapshot{{
		ID: "<script>alert(1)</script>", Scenario: oversized, Steps: []flowviz.FunnelStepSnapshot{{ID: "x", Route: "GET /<img src=x onerror=alert(1)>" + oversized}},
	}}}
	h := NewHandler(Provider{AccessLog: &fakeAccessLog{current: accesslog.Snapshot{Visualization: visualization}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/snapshot.html", nil))
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") || strings.Contains(rec.Body.String(), "<img src=x") {
		t.Fatalf("unescaped visualization label: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), strings.Repeat("z", flowviz.MaxNodeBytes+1)) {
		t.Fatal("oversized persisted label was rendered without a bound")
	}
}
