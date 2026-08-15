package flowstats

import (
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/flowviz"
	"github.com/ekusiadadus/isutools/sessionlabel"
)

func TestRegistryCarriesDetailedVisualizationAcrossSnapshotAndReset(t *testing.T) {
	registry, err := NewRegistryWithOptions(flowviz.Options{
		Enabled: true,
		Config: flowviz.Config{Version: 1, Funnels: []flowviz.FunnelDefinition{{
			ID: "checkout", Scenario: "buyer", Mode: flowviz.ModeOrdered,
			Steps: []flowviz.StepDefinition{{ID: "list", Route: "GET /items"}, {ID: "done", Route: "POST /checkout"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now()
	registry.ObserveRequest(sessionlabel.Observation{Session: "s1", Scenario: "buyer", Method: "GET", Route: "/items", Status: 200, Duration: 10 * time.Millisecond, At: base})
	registry.ObserveRequest(sessionlabel.Observation{Session: "s1", Scenario: "buyer", Method: "POST", Route: "/checkout", Status: 503, Duration: 20 * time.Millisecond, At: base.Add(time.Second)})
	snapshot := registry.Snapshot()
	if snapshot.Visualization == nil || snapshot.Visualization.Funnels[0].Completed != 1 || snapshot.Visualization.Funnels[0].Steps[1].Status5xx != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	registry.Reset()
	if got := registry.Snapshot(); got.Visualization == nil || got.Visualization.Funnels[0].Entered != 0 {
		t.Fatalf("reset = %#v", got)
	}
}

func TestConfigureIsAtomicAndKeepsPriorCollectorOnInvalidOptions(t *testing.T) {
	registry := NewRegistry()
	registry.Observe("s1", "story", "GET", "/before")
	if err := registry.Configure(flowviz.Options{Enabled: true, MaxNodes: 1, Config: flowviz.Config{Version: 1}}); err == nil {
		t.Fatal("invalid visualization options were accepted")
	}
	if got := registry.Snapshot(); len(got.Stories) != 1 {
		t.Fatalf("invalid configure replaced prior collector: %#v", got)
	}

	if err := registry.Configure(flowviz.Options{Enabled: true, Config: flowviz.Config{Version: 1}}); err != nil {
		t.Fatal(err)
	}
	got := registry.Snapshot()
	if len(got.Stories) != 0 || got.Visualization == nil || got.Visualization.Status == flowviz.StatusDisabled {
		t.Fatalf("successful configure = %#v", got)
	}
}

func TestNilRegistryMethodsAreSafe(t *testing.T) {
	var registry *Registry
	registry.Observe("s", "x", "GET", "/")
	registry.ObserveRequest(sessionlabel.Observation{Session: "s", Scenario: "x", Method: "GET", Route: "/"})
	registry.Reset()
	if err := registry.Configure(flowviz.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := registry.Snapshot(); len(got.Flows) != 0 || got.Visualization != nil {
		t.Fatalf("nil snapshot = %#v", got)
	}
}

func TestGenerationCollectorNameAndNilRegistryDefault(t *testing.T) {
	collector := NewGenerationCollector(nil)
	if collector.Name() != SectionName || collector.registry == nil {
		t.Fatalf("collector = %#v", collector)
	}
}
