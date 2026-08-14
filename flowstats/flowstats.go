// Package flowstats records bounded, pseudonymous user-flow transitions and
// explicit scenario journeys directly at the application middleware boundary.
// It is independent of the selected reverse proxy.
package flowstats

import (
	"sync"

	"github.com/ekusiadadus/isutools/accesslog"
)

type Snapshot struct {
	Stories      []accesslog.StoryEntry `json:"stories,omitempty"`
	StoryDropped int64                  `json:"story_dropped,omitempty"`
	Flows        []accesslog.FlowEntry  `json:"flows,omitempty"`
	FlowDropped  int64                  `json:"flow_dropped,omitempty"`
}

type Registry struct {
	mu  sync.RWMutex
	agg *accesslog.Aggregator
}

var Default = NewRegistry()

func NewRegistry() *Registry { return &Registry{agg: accesslog.NewAggregator(1)} }

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

func (r *Registry) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.RLock()
	snapshot := snapshotFromAccessLog(r.agg.Snapshot())
	r.mu.RUnlock()
	return snapshot
}

func (r *Registry) Reset() { r.rotate() }

func (r *Registry) rotate() *accesslog.Aggregator {
	r.mu.Lock()
	old := r.agg
	r.agg = accesslog.NewAggregator(1)
	r.mu.Unlock()
	return old
}

func snapshotFromAccessLog(value accesslog.Snapshot) Snapshot {
	return Snapshot{
		Stories: append([]accesslog.StoryEntry(nil), value.Stories...), StoryDropped: value.StoryDropped,
		Flows: append([]accesslog.FlowEntry(nil), value.Flows...), FlowDropped: value.FlowDropped,
	}
}

func cloneSnapshot(value Snapshot) Snapshot {
	result := Snapshot{
		Stories: make([]accesslog.StoryEntry, len(value.Stories)), StoryDropped: value.StoryDropped,
		Flows: append([]accesslog.FlowEntry(nil), value.Flows...), FlowDropped: value.FlowDropped,
	}
	for i, story := range value.Stories {
		result.Stories[i] = story
		result.Stories[i].Journey = append([]string(nil), story.Journey...)
	}
	return result
}
