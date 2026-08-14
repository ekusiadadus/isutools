package web

import (
	"testing"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/flowstats"
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
