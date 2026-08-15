package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ekusiadadus/isutools/flowviz"
)

type flowGraphView struct {
	Nodes []flowGraphNodeView
	Edges []flowGraphEdgeView
}

type flowGraphNodeView struct {
	ID     string
	Label  string
	Count  int64
	X      int
	Y      int
	Radius int
}

type flowGraphEdgeView struct {
	From, To       string
	Count          int64
	X1, Y1, X2, Y2 int
	Width          int
	Self           bool
	Path           string
}

func buildFlowGraphView(graph flowviz.GraphSnapshot) flowGraphView {
	view := flowGraphView{}
	nodes := graph.Nodes
	if len(nodes) > flowviz.HardMaxNodes {
		nodes = nodes[:flowviz.HardMaxNodes]
	}
	edges := graph.Edges
	if len(edges) > flowviz.HardMaxEdges {
		edges = edges[:flowviz.HardMaxEdges]
	}
	if len(nodes) == 0 {
		return view
	}
	maxNode, maxEdge := int64(1), int64(1)
	for _, node := range nodes {
		if node.Count > maxNode {
			maxNode = node.Count
		}
	}
	for _, edge := range edges {
		if edge.Count > maxEdge {
			maxEdge = edge.Count
		}
	}
	positions := make(map[string]flowGraphNodeView, len(nodes))
	const centerX, centerY, radius = 400, 250, 190
	for i, node := range nodes {
		count := maxInt64(node.Count, 0)
		angle := -math.Pi/2 + 2*math.Pi*float64(i)/float64(len(nodes))
		item := flowGraphNodeView{
			ID: node.ID, Label: truncateFlowLabel(node.ID, 28), Count: count,
			X:      centerX + int(math.Round(radius*math.Cos(angle))),
			Y:      centerY + int(math.Round(radius*math.Sin(angle))),
			Radius: 16 + int(float64(count)/float64(maxNode)*12),
		}
		positions[node.ID] = item
		view.Nodes = append(view.Nodes, item)
	}
	for _, edge := range edges {
		if edge.Count <= 0 {
			continue
		}
		from, fromOK := positions[edge.From]
		to, toOK := positions[edge.To]
		if !fromOK || !toOK {
			continue
		}
		item := flowGraphEdgeView{
			From: edge.From, To: edge.To, Count: edge.Count,
			X1: from.X, Y1: from.Y, X2: to.X, Y2: to.Y,
			Width: 1 + int(float64(edge.Count)/float64(maxEdge)*7),
		}
		if edge.From == edge.To {
			item.Self = true
			item.Path = fmt.Sprintf("M %d %d C %d %d, %d %d, %d %d",
				from.X+12, from.Y-12, from.X+55, from.Y-55, from.X+55, from.Y+55, from.X+12, from.Y+12)
		}
		view.Edges = append(view.Edges, item)
	}
	return view
}

func (v flowGraphView) Canonical() string {
	body, _ := json.Marshal(v)
	return string(body)
}

type flowHeatmapView struct {
	Nodes []string
	Rows  []flowHeatmapRow
}

type flowHeatmapRow struct {
	From  string
	Cells []flowHeatmapCell
}

type flowHeatmapCell struct {
	To    string
	Count int64
	Level int
}

func buildFlowHeatmap(graph flowviz.GraphSnapshot) flowHeatmapView {
	view := flowHeatmapView{}
	nodes := graph.Nodes
	if len(nodes) > flowviz.HardMaxNodes {
		nodes = nodes[:flowviz.HardMaxNodes]
	}
	edges := graph.Edges
	if len(edges) > flowviz.HardMaxEdges {
		edges = edges[:flowviz.HardMaxEdges]
	}
	for _, node := range nodes {
		view.Nodes = append(view.Nodes, node.ID)
	}
	counts := make(map[string]int64, len(edges))
	maxCount := int64(0)
	for _, edge := range edges {
		if edge.Count <= 0 {
			continue
		}
		counts[edge.From+"\x00"+edge.To] = edge.Count
		if edge.Count > maxCount {
			maxCount = edge.Count
		}
	}
	for _, from := range view.Nodes {
		row := flowHeatmapRow{From: from, Cells: make([]flowHeatmapCell, 0, len(view.Nodes))}
		for _, to := range view.Nodes {
			count := counts[from+"\x00"+to]
			level := 0
			if count > 0 && maxCount > 0 {
				level = 1 + int(float64(count-1)/float64(maxCount)*5)
				if level > 5 {
					level = 5
				}
			}
			row.Cells = append(row.Cells, flowHeatmapCell{To: to, Count: count, Level: level})
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

func truncateFlowLabel(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func percentBasisPoints(value int64) string {
	return strconv.FormatFloat(float64(value)/100, 'f', 2, 64) + "%"
}

func signedPercentBasisPoints(value int64) template.HTML {
	prefix := ""
	if value > 0 {
		prefix = "+"
	}
	return template.HTML(prefix + strconv.FormatFloat(float64(value)/100, 'f', 2, 64))
}

// boundedFlowVisualization treats persisted run data as untrusted input before
// passing it to html/template. It keeps rendering work within the same hard
// limits as the online aggregator even if a saved snapshot was edited.
func boundedFlowVisualization(value *flowviz.Snapshot) *flowviz.Snapshot {
	if value == nil {
		return nil
	}
	result := *value
	result.Status = boundFlowString(result.Status, 32)
	result.Reason = boundFlowString(result.Reason, 256)
	result.SessionDropped = maxInt64(result.SessionDropped, 0)
	result.TimingMissing = maxInt64(result.TimingMissing, 0)

	funnelCount := len(value.Funnels)
	if funnelCount > flowviz.MaxFunnels {
		funnelCount = flowviz.MaxFunnels
		result.Partial = true
	}
	result.Funnels = make([]flowviz.FunnelSnapshot, funnelCount)
	for i := 0; i < funnelCount; i++ {
		funnel := value.Funnels[i]
		funnel.ID = boundFlowString(funnel.ID, 64)
		funnel.Scenario = boundFlowString(funnel.Scenario, 64)
		funnel.Mode = boundFlowString(funnel.Mode, 32)
		funnel.Within = boundFlowString(funnel.Within, 32)
		funnel.Entered = maxInt64(funnel.Entered, 0)
		funnel.Completed = maxInt64(funnel.Completed, 0)
		funnel.Expired = maxInt64(funnel.Expired, 0)
		funnel.ConversionBP = clampBasisPoints(funnel.ConversionBP)
		stepCount := len(funnel.Steps)
		if stepCount > flowviz.MaxStepsPerFunnel {
			stepCount = flowviz.MaxStepsPerFunnel
			result.Partial = true
		}
		steps := make([]flowviz.FunnelStepSnapshot, stepCount)
		for j := 0; j < stepCount; j++ {
			step := funnel.Steps[j]
			step.ID = boundFlowString(step.ID, 64)
			step.Route = boundFlowString(step.Route, flowviz.MaxNodeBytes)
			step.Sessions = maxInt64(step.Sessions, 0)
			step.Requests = maxInt64(step.Requests, 0)
			step.DropOff = maxInt64(step.DropOff, 0)
			step.Retries = maxInt64(step.Retries, 0)
			step.Status4xx = maxInt64(step.Status4xx, 0)
			step.Status5xx = maxInt64(step.Status5xx, 0)
			step.RequestP95 = maxDuration(step.RequestP95, 0)
			step.FromStartBP = clampBasisPoints(step.FromStartBP)
			step.FromPreviousBP = clampBasisPoints(step.FromPreviousBP)
			steps[j] = step
		}
		funnel.Steps = steps
		result.Funnels[i] = funnel
	}

	result.Graph = value.Graph
	result.Graph.Dropped = maxInt64(result.Graph.Dropped, 0)
	result.Graph.HiddenCount = maxInt64(result.Graph.HiddenCount, 0)
	nodeCount := len(value.Graph.Nodes)
	if nodeCount > flowviz.HardMaxNodes {
		nodeCount = flowviz.HardMaxNodes
		result.Graph.Truncated = true
		result.Partial = true
	}
	result.Graph.Nodes = append([]flowviz.GraphNode(nil), value.Graph.Nodes[:nodeCount]...)
	for i := range result.Graph.Nodes {
		result.Graph.Nodes[i].ID = boundFlowString(result.Graph.Nodes[i].ID, flowviz.MaxNodeBytes)
		result.Graph.Nodes[i].Count = maxInt64(result.Graph.Nodes[i].Count, 0)
	}
	edgeCount := len(value.Graph.Edges)
	if edgeCount > flowviz.HardMaxEdges {
		edgeCount = flowviz.HardMaxEdges
		result.Graph.Truncated = true
		result.Partial = true
	}
	result.Graph.Edges = append([]flowviz.GraphEdge(nil), value.Graph.Edges[:edgeCount]...)
	for i := range result.Graph.Edges {
		result.Graph.Edges[i].From = boundFlowString(result.Graph.Edges[i].From, flowviz.MaxNodeBytes)
		result.Graph.Edges[i].To = boundFlowString(result.Graph.Edges[i].To, flowviz.MaxNodeBytes)
		result.Graph.Edges[i].Count = maxInt64(result.Graph.Edges[i].Count, 0)
	}
	return &result
}

func clampBasisPoints(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > 10000 {
		return 10000
	}
	return value
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func boundFlowString(value string, limit int) string {
	if limit < 1 {
		return ""
	}
	if len(value) > limit {
		value = value[:limit]
		for len(value) > 0 && !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.ToValidUTF8(value, "")
}
