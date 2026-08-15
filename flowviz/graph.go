package flowviz

import (
	"encoding/json"
	"math"
	"sort"
	"unicode/utf8"
)

const OtherNode = "(other)"

type Transition struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int64  `json:"count"`
}

type GraphSnapshot struct {
	Nodes       []GraphNode `json:"nodes,omitempty"`
	Edges       []GraphEdge `json:"edges,omitempty"`
	InputEdges  int         `json:"input_edges"`
	HiddenCount int64       `json:"hidden_count,omitempty"`
	Dropped     int64       `json:"dropped,omitempty"`
	Truncated   bool        `json:"truncated,omitempty"`
	Partial     bool        `json:"partial,omitempty"`
}

type GraphNode struct {
	ID    string `json:"id"`
	Count int64  `json:"count"`
}

type GraphEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int64  `json:"count"`
}

func BuildGraph(transitions []Transition, maxNodes, maxEdges int) GraphSnapshot {
	if maxNodes < 2 {
		maxNodes = DefaultMaxNodes
	}
	if maxNodes > HardMaxNodes {
		maxNodes = HardMaxNodes
	}
	if maxEdges < 1 {
		maxEdges = DefaultMaxEdges
	}
	if maxEdges > HardMaxEdges {
		maxEdges = HardMaxEdges
	}
	graph := GraphSnapshot{InputEdges: len(transitions)}
	valid := make([]Transition, 0, len(transitions))
	nodeTotals := map[string]int64{}
	for _, transition := range transitions {
		if transition.Count <= 0 || !validNode(transition.From) || !validNode(transition.To) {
			graph.Dropped++
			graph.Partial = true
			continue
		}
		valid = append(valid, transition)
		nodeTotals[transition.From] = saturatingAdd(nodeTotals[transition.From], transition.Count)
		nodeTotals[transition.To] = saturatingAdd(nodeTotals[transition.To], transition.Count)
	}
	type rankedNode struct {
		id    string
		count int64
	}
	ranked := make([]rankedNode, 0, len(nodeTotals))
	for id, count := range nodeTotals {
		ranked = append(ranked, rankedNode{id: id, count: count})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].id < ranked[j].id
	})
	keepCount := len(ranked)
	needsOther := keepCount > maxNodes
	if needsOther {
		keepCount = maxNodes - 1
		graph.Truncated = true
	}
	kept := make(map[string]struct{}, keepCount)
	for _, node := range ranked[:keepCount] {
		kept[node.id] = struct{}{}
	}
	edgeCounts := map[string]int64{}
	edgeHidden := map[string]bool{}
	for _, transition := range valid {
		from, to := transition.From, transition.To
		_, fromKept := kept[from]
		_, toKept := kept[to]
		if !fromKept {
			from = OtherNode
		}
		if !toKept {
			to = OtherNode
		}
		if !fromKept || !toKept {
			graph.HiddenCount = saturatingAdd(graph.HiddenCount, transition.Count)
		}
		key := from + "\x00" + to
		edgeCounts[key] = saturatingAdd(edgeCounts[key], transition.Count)
		edgeHidden[key] = edgeHidden[key] || !fromKept || !toKept
	}
	for key, count := range edgeCounts {
		from, to, _ := splitTransitionKey(key)
		graph.Edges = append(graph.Edges, GraphEdge{From: from, To: to, Count: count})
	}
	sort.Slice(graph.Edges, func(i, j int) bool {
		if graph.Edges[i].Count != graph.Edges[j].Count {
			return graph.Edges[i].Count > graph.Edges[j].Count
		}
		if graph.Edges[i].From != graph.Edges[j].From {
			return graph.Edges[i].From < graph.Edges[j].From
		}
		return graph.Edges[i].To < graph.Edges[j].To
	})
	if len(graph.Edges) > maxEdges {
		for _, edge := range graph.Edges[maxEdges:] {
			if !edgeHidden[edge.From+"\x00"+edge.To] {
				graph.HiddenCount = saturatingAdd(graph.HiddenCount, edge.Count)
			}
		}
		graph.Edges = graph.Edges[:maxEdges]
		graph.Truncated = true
	}
	visibleTotals := map[string]int64{}
	for _, edge := range graph.Edges {
		visibleTotals[edge.From] = saturatingAdd(visibleTotals[edge.From], edge.Count)
		visibleTotals[edge.To] = saturatingAdd(visibleTotals[edge.To], edge.Count)
	}
	for _, node := range ranked[:keepCount] {
		if count := visibleTotals[node.id]; count > 0 {
			graph.Nodes = append(graph.Nodes, GraphNode{ID: node.id, Count: count})
		}
	}
	if needsOther && visibleTotals[OtherNode] > 0 {
		graph.Nodes = append(graph.Nodes, GraphNode{ID: OtherNode, Count: visibleTotals[OtherNode]})
	}
	return graph
}

func saturatingAdd(current, value int64) int64 {
	if value <= 0 {
		return current
	}
	if current > math.MaxInt64-value {
		return math.MaxInt64
	}
	return current + value
}

func validNode(value string) bool {
	return value != "" && len(value) <= MaxNodeBytes && utf8.ValidString(value) && routePattern.MatchString(value)
}

func splitTransitionKey(key string) (string, string, bool) {
	for i := range key {
		if key[i] == 0 {
			return key[:i], key[i+1:], true
		}
	}
	return key, "", false
}

func (g GraphSnapshot) Canonical() string {
	body, _ := json.Marshal(g)
	return string(body)
}
