package isutools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFlowVisualizationOptions(t *testing.T) {
	getenv := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	options, reason := resolveFlowVisualizationOptions(getenv(map[string]string{"ISUTOOLS_FLOW_VIZ": "off"}))
	if options.Enabled || reason != "flow-viz-off" {
		t.Fatalf("off = %#v %q", options, reason)
	}
	options, reason = resolveFlowVisualizationOptions(getenv(map[string]string{"ISUTOOLS_FLOW_VIZ": "on", "ISUTOOLS_FLOW_MAX_NODES": "24", "ISUTOOLS_FLOW_MAX_EDGES": "64"}))
	if !options.Enabled || options.MaxNodes != 24 || options.MaxEdges != 64 || reason != "graph-only" {
		t.Fatalf("on = %#v %q", options, reason)
	}
	options, reason = resolveFlowVisualizationOptions(getenv(map[string]string{"ISUTOOLS_FLOW_VIZ": "on", "ISUTOOLS_FLOW_MAX_NODES": "999"}))
	if options.Enabled || reason != "invalid-limit" {
		t.Fatalf("invalid limit = %#v %q", options, reason)
	}
}

func TestResolveFlowVisualizationLoadsFunnelConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "funnels.yaml")
	if err := os.WriteFile(path, []byte(`version: 1
funnels:
  - id: checkout
    scenario: buyer
    mode: ordered
    steps:
      - {id: list, route: "GET /items"}
      - {id: done, route: "POST /checkout"}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	options, reason := resolveFlowVisualizationOptions(func(key string) string {
		values := map[string]string{"ISUTOOLS_FLOW_VIZ": "on", "ISUTOOLS_FUNNEL_CONFIG": path}
		return values[key]
	})
	if !options.Enabled || len(options.Config.Funnels) != 1 || reason != "funnel-configured" {
		t.Fatalf("configured = %#v %q", options, reason)
	}
}
