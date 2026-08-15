package web

import (
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/flowstats"
	"github.com/ekusiadadus/isutools/flowviz"
)

func TestFlowViewPrefersMiddlewareWithoutCombiningSources(t *testing.T) {
	snapshot := Snapshot{
		FlowSource: "middleware",
		Flow: &flowstats.Snapshot{
			Flows:   []accesslog.FlowEntry{{From: "GET /", To: "GET /new", Count: 2}},
			Stories: []accesslog.StoryEntry{{Scenario: "new", Sessions: 2}},
		},
		AccessLog: &accesslog.Snapshot{
			Flows:   []accesslog.FlowEntry{{From: "GET /", To: "GET /legacy", Count: 99}},
			Stories: []accesslog.StoryEntry{{Scenario: "legacy", Sessions: 99}},
		},
	}
	if got := snapshot.UserFlows(); len(got) != 1 || got[0].To != "GET /new" {
		t.Fatalf("flows = %#v", got)
	}
	if got := snapshot.ScenarioStories(); len(got) != 1 || got[0].Scenario != "new" {
		t.Fatalf("stories = %#v", got)
	}
}

func TestFlowViewFallsBackToProxy(t *testing.T) {
	snapshot := Snapshot{FlowSource: "auto", Flow: &flowstats.Snapshot{}, AccessLog: &accesslog.Snapshot{
		Flows: []accesslog.FlowEntry{{From: "GET /", To: "GET /legacy", Count: 1}},
	}}
	if got := snapshot.UserFlows(); len(got) != 1 || got[0].To != "GET /legacy" {
		t.Fatalf("flows = %#v", got)
	}
}

func TestExplicitFlowSourcesDoNotFallbackOrLeakWhenOff(t *testing.T) {
	proxy := &accesslog.Snapshot{
		Flows:   []accesslog.FlowEntry{{From: "GET /", To: "GET /proxy", Count: 3}},
		Stories: []accesslog.StoryEntry{{Scenario: "proxy", Sessions: 3}},
	}
	middleware := &flowstats.Snapshot{}

	for _, source := range []string{"middleware", "off", "invalid"} {
		snapshot := Snapshot{FlowSource: source, Flow: middleware, AccessLog: proxy}
		if got := snapshot.UserFlows(); len(got) != 0 {
			t.Errorf("%s flows = %#v, want none", source, got)
		}
		if got := snapshot.ScenarioStories(); len(got) != 0 {
			t.Errorf("%s stories = %#v, want none", source, got)
		}
	}

	snapshot := Snapshot{FlowSource: "proxy", Flow: middleware, AccessLog: proxy}
	if got := snapshot.UserFlows(); len(got) != 1 || got[0].To != "GET /proxy" {
		t.Fatalf("proxy flows = %#v", got)
	}
}

func TestApplyFlowSourceKeepsOnlyOneSerializedSource(t *testing.T) {
	proxy := func() *accesslog.Snapshot {
		return &accesslog.Snapshot{Flows: []accesslog.FlowEntry{{From: "GET /", To: "GET /proxy", Count: 3}}}
	}
	middleware := func() *flowstats.Snapshot {
		return &flowstats.Snapshot{Flows: []accesslog.FlowEntry{{From: "GET /", To: "GET /middleware", Count: 2}}}
	}

	auto := Snapshot{FlowSource: "auto", Flow: middleware(), AccessLog: proxy()}
	applyFlowSource(&auto)
	if len(auto.AccessLog.Flows) != 0 || auto.Flow == nil {
		t.Fatalf("auto snapshot retained both sources: %#v", auto)
	}

	proxyOnly := Snapshot{FlowSource: "proxy", Flow: middleware(), AccessLog: proxy()}
	applyFlowSource(&proxyOnly)
	if proxyOnly.Flow != nil || len(proxyOnly.AccessLog.Flows) != 1 {
		t.Fatalf("proxy snapshot = %#v", proxyOnly)
	}

	off := Snapshot{FlowSource: "off", Flow: middleware(), AccessLog: proxy()}
	applyFlowSource(&off)
	if off.Flow != nil || len(off.AccessLog.Flows) != 0 {
		t.Fatalf("off snapshot leaked flow data: %#v", off)
	}
}

func TestFlowVisualizationUsesTheSameSingleSourceContract(t *testing.T) {
	middlewareViz := &flowviz.Snapshot{Status: flowviz.StatusReady, Funnels: []flowviz.FunnelSnapshot{{ID: "middleware"}}}
	proxyViz := &flowviz.Snapshot{Status: flowviz.StatusReady, Funnels: []flowviz.FunnelSnapshot{{ID: "proxy"}}}
	middleware := &flowstats.Snapshot{Visualization: middlewareViz}
	proxy := &accesslog.Snapshot{Visualization: proxyViz}

	for _, tc := range []struct {
		source string
		want   string
	}{
		{source: "middleware", want: "middleware"},
		{source: "proxy", want: "proxy"},
		{source: "auto", want: "middleware"},
		{source: "off", want: ""},
	} {
		snapshot := Snapshot{FlowSource: tc.source, Flow: middleware, AccessLog: proxy}
		got := snapshot.FlowVisualization()
		if tc.want == "" {
			if got != nil {
				t.Errorf("%s visualization = %#v, want nil", tc.source, got)
			}
			continue
		}
		if got == nil || len(got.Funnels) != 1 || got.Funnels[0].ID != tc.want {
			t.Errorf("%s visualization = %#v", tc.source, got)
		}
	}

	off := Snapshot{FlowSource: "off", Flow: &flowstats.Snapshot{Visualization: middlewareViz}, AccessLog: &accesslog.Snapshot{Visualization: proxyViz}}
	applyFlowSource(&off)
	if off.Flow != nil || off.AccessLog.Visualization != nil {
		t.Fatalf("off serialization leaked visualization: %#v", off)
	}
}

func TestAutoFlowVisualizationFallsBackWhenMiddlewareHasOnlyConfiguration(t *testing.T) {
	middlewareViz := &flowviz.Snapshot{Status: flowviz.StatusReady, Funnels: []flowviz.FunnelSnapshot{{ID: "configured-empty"}}}
	proxyViz := &flowviz.Snapshot{Status: flowviz.StatusReady, Graph: flowviz.GraphSnapshot{
		InputEdges: 1,
		Nodes:      []flowviz.GraphNode{{ID: "GET /proxy", Count: 1}},
		Edges:      []flowviz.GraphEdge{{From: "GET /proxy", To: "GET /proxy", Count: 1}},
	}}
	snapshot := Snapshot{FlowSource: "auto", Flow: &flowstats.Snapshot{Visualization: middlewareViz}, AccessLog: &accesslog.Snapshot{Visualization: proxyViz}}
	if got := snapshot.FlowVisualization(); got != proxyViz {
		t.Fatalf("auto visualization = %#v, want observed proxy data", got)
	}
	applyFlowSource(&snapshot)
	if snapshot.AccessLog == nil || snapshot.AccessLog.Visualization != proxyViz {
		t.Fatalf("auto serialization discarded proxy fallback: %#v", snapshot)
	}
}

func TestFlowVisualizationHealthReportsOnlySelectedPartialData(t *testing.T) {
	snapshot := Snapshot{
		FlowSource: "middleware",
		Flow: &flowstats.Snapshot{Visualization: &flowviz.Snapshot{
			Status: flowviz.StatusPartial, Partial: true, SessionDropped: 3, TimingMissing: 2,
		}},
		AccessLog: &accesslog.Snapshot{Visualization: &flowviz.Snapshot{
			Status: flowviz.StatusPartial, Partial: true, SessionDropped: 99,
		}},
	}
	applyFlowSource(&snapshot)
	applyFlowVisualizationHealth(&snapshot)
	if !snapshot.Meta.Partial || len(snapshot.Meta.Health) != 1 || snapshot.Meta.Health[0].Collector != "flow-viz" || snapshot.Meta.Health[0].Dropped != 3 {
		t.Fatalf("health = %#v", snapshot.Meta)
	}
	if !strings.Contains(snapshot.Meta.Health[0].Message, "timing metadata missing for 2 events") {
		t.Fatalf("message = %q", snapshot.Meta.Health[0].Message)
	}
}
