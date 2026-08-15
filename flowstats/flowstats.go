// Package flowstats records bounded, pseudonymous user-flow transitions and
// explicit scenario journeys directly at the application middleware boundary.
// It is independent of the selected reverse proxy.
package flowstats

import (
	"sync"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/flowviz"
	"github.com/ekusiadadus/isutools/sessionlabel"
)

type Snapshot struct {
	Stories       []accesslog.StoryEntry `json:"stories,omitempty"`
	StoryDropped  int64                  `json:"story_dropped,omitempty"`
	Flows         []accesslog.FlowEntry  `json:"flows,omitempty"`
	FlowDropped   int64                  `json:"flow_dropped,omitempty"`
	Visualization *flowviz.Snapshot      `json:"visualization,omitempty"`
}

type Registry struct {
	mu      sync.RWMutex
	agg     *accesslog.Aggregator
	options flowviz.Options
}

var Default = NewRegistry()

func NewRegistry() *Registry {
	registry, _ := NewRegistryWithOptions(flowviz.Options{})
	return registry
}

func NewRegistryWithOptions(options flowviz.Options) (*Registry, error) {
	agg, err := accesslog.NewAggregatorWithFlowVisualization(1, options)
	if err != nil {
		return nil, err
	}
	return &Registry{agg: agg, options: options}, nil
}

// Configure replaces the empty/current generation with one using immutable
// visualization options. Startup wiring calls it before serving traffic.
func (r *Registry) Configure(options flowviz.Options) error {
	if r == nil {
		return nil
	}
	agg, err := accesslog.NewAggregatorWithFlowVisualization(1, options)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.agg = agg
	r.options = options
	r.mu.Unlock()
	return nil
}

// Observe receives only already-pseudonymized session labels, bounded scenario
// names, and registered route templates. Raw cookies and raw URL paths never
// enter this package.
func (r *Registry) Observe(session, scenario, method, route string) {
	if r == nil || session == "" || method == "" || route == "" {
		return
	}
	r.mu.RLock()
	r.agg.Observe(accesslog.Record{
		Session: session, Scenario: scenario, Method: method, URI: route,
		Status: 200, UpstreamValid: true, NoUpstreamTiming: true,
	})
	r.mu.RUnlock()
}

func (r *Registry) ObserveRequest(value sessionlabel.Observation) {
	if r == nil {
		return
	}
	r.mu.RLock()
	r.agg.Observe(accesslog.Record{
		Session: value.Session, Scenario: value.Scenario, Method: value.Method, URI: value.Route,
		Status: value.Status, RequestTime: value.Duration, ObservedAt: value.At,
		UpstreamValid: true, NoUpstreamTiming: true,
	})
	r.mu.RUnlock()
}

func (r *Registry) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.RLock()
	snapshot := snapshotFromAccessLog(r.agg.Snapshot())
	r.mu.RUnlock()
	return snapshot
}

func (r *Registry) Reset() {
	if r == nil {
		return
	}
	r.rotate()
}

func (r *Registry) rotate() *accesslog.Aggregator {
	r.mu.Lock()
	old := r.agg
	configured, err := accesslog.NewAggregatorWithFlowVisualization(1, r.options)
	if err != nil {
		configured = accesslog.NewAggregator(1)
	}
	r.agg = configured
	r.mu.Unlock()
	return old
}

func snapshotFromAccessLog(value accesslog.Snapshot) Snapshot {
	return Snapshot{
		Stories: append([]accesslog.StoryEntry(nil), value.Stories...), StoryDropped: value.StoryDropped,
		Flows: append([]accesslog.FlowEntry(nil), value.Flows...), FlowDropped: value.FlowDropped,
		Visualization: flowviz.CloneSnapshot(value.Visualization),
	}
}

func cloneSnapshot(value Snapshot) Snapshot {
	result := Snapshot{
		Stories: make([]accesslog.StoryEntry, len(value.Stories)), StoryDropped: value.StoryDropped,
		Flows: append([]accesslog.FlowEntry(nil), value.Flows...), FlowDropped: value.FlowDropped,
		Visualization: flowviz.CloneSnapshot(value.Visualization),
	}
	for i, story := range value.Stories {
		result.Stories[i] = story
		result.Stories[i].Journey = append([]string(nil), story.Journey...)
	}
	return result
}
