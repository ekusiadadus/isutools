package accesslog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/flowviz"
)

func TestAggregatorPublishesConfiguredFlowVisualization(t *testing.T) {
	agg, err := NewAggregatorWithFlowVisualization(100, flowviz.Options{
		Enabled: true, MaxNodes: 8, MaxEdges: 12,
		Config: flowviz.Config{Version: 1, Funnels: []flowviz.FunnelDefinition{{
			ID: "checkout", Scenario: "buyer", Mode: flowviz.ModeOrdered,
			Steps: []flowviz.StepDefinition{{ID: "list", Route: "GET /items"}, {ID: "done", Route: "POST /checkout"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	for _, rec := range []Record{
		{Session: "s1", Scenario: "buyer", Method: "GET", URI: "/items", Status: 200, RequestTime: 10 * time.Millisecond, ObservedAt: base},
		{Session: "s1", Scenario: "buyer", Method: "POST", URI: "/checkout", Status: 201, RequestTime: 20 * time.Millisecond, ObservedAt: base.Add(time.Second)},
		{Session: "s2", Scenario: "buyer", Method: "GET", URI: "/items", Status: 200, RequestTime: 30 * time.Millisecond, ObservedAt: base},
	} {
		agg.Observe(rec)
	}
	snapshot := agg.Snapshot()
	if snapshot.Visualization == nil || len(snapshot.Visualization.Funnels) != 1 || snapshot.Visualization.Funnels[0].Completed != 1 {
		t.Fatalf("visualization = %#v", snapshot.Visualization)
	}
	if len(snapshot.Visualization.Graph.Edges) != 1 || snapshot.Visualization.Graph.Edges[0].Count != 1 {
		t.Fatalf("graph = %#v", snapshot.Visualization.Graph)
	}

	agg.Reset()
	reset := agg.Snapshot()
	if reset.Visualization == nil || reset.Visualization.Funnels[0].Entered != 0 || len(reset.Visualization.Graph.Edges) != 0 {
		t.Fatalf("reset visualization = %#v", reset.Visualization)
	}
}

func TestLegacyAggregatorOmitsVisualization(t *testing.T) {
	agg := NewAggregator(10)
	agg.Observe(Record{Session: "s", Method: "GET", URI: "/"})
	if got := agg.Snapshot().Visualization; got != nil {
		t.Fatalf("legacy visualization = %#v", got)
	}
}

func TestCollectorFlowVisualizationOptionsAreOrderIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	options := flowviz.Options{Enabled: true, Config: flowviz.Config{Version: 1}}
	for _, opts := range [][]Option{
		{WithMaxKeys(2), WithFlowVisualization(options)},
		{WithFlowVisualization(options), WithMaxKeys(2)},
	} {
		collector := New(path, opts...)
		if got := collector.Peek().Visualization; got == nil || got.Status == flowviz.StatusDisabled {
			t.Fatalf("options lost visualization: %#v", got)
		}
		collector.Close()
	}
}
