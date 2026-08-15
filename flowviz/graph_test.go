package flowviz

import (
	"math"
	"testing"
)

func TestGraphIsDeterministicBoundedAndMergesOther(t *testing.T) {
	transitions := []Transition{
		{From: "GET /", To: "GET /items", Count: 100},
		{From: "GET /items", To: "GET /items/:id", Count: 80},
		{From: "GET /items/:id", To: "POST /cart", Count: 60},
		{From: "POST /cart", To: "GET /checkout", Count: 40},
		{From: "GET /rare", To: "GET /other", Count: 2},
	}
	graph := BuildGraph(transitions, 4, 4)
	if !graph.Truncated || len(graph.Nodes) > 4 || len(graph.Edges) > 4 {
		t.Fatalf("graph bounds = %#v", graph)
	}
	if graph.Nodes[len(graph.Nodes)-1].ID != OtherNode {
		t.Fatalf("last node = %#v, want other", graph.Nodes)
	}
	if graph.HiddenCount == 0 {
		t.Fatalf("hidden count not reported: %#v", graph)
	}
	again := BuildGraph(append([]Transition(nil), transitions...), 4, 4)
	if graph.Canonical() != again.Canonical() {
		t.Fatalf("graph is not deterministic:\n%s\n%s", graph.Canonical(), again.Canonical())
	}
}

func TestGraphRejectsInvalidCountsAndUnboundedLabels(t *testing.T) {
	graph := BuildGraph([]Transition{
		{From: "GET /ok", To: "POST /done", Count: -1},
		{From: "GET /" + longString(MaxNodeBytes+1), To: "POST /done", Count: 10},
	}, DefaultMaxNodes, DefaultMaxEdges)
	if !graph.Partial || graph.Dropped != 2 || len(graph.Edges) != 0 {
		t.Fatalf("invalid graph = %#v", graph)
	}
}

func TestGraphSaturatesHugeCountsWithoutGoingNegative(t *testing.T) {
	graph := BuildGraph([]Transition{
		{From: "GET /a", To: "GET /b", Count: math.MaxInt64},
		{From: "GET /b", To: "POST /c", Count: math.MaxInt64},
	}, 2, 1)
	if len(graph.Edges) != 1 || graph.Edges[0].Count <= 0 || graph.HiddenCount < 0 {
		t.Fatalf("overflowed graph = %#v", graph)
	}
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
