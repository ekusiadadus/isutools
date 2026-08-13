package pprofanalyze

import (
	"math"
	"sort"
	"strings"

	pprofprofile "github.com/google/pprof/profile"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

type flameTree struct {
	name     string
	value    int64
	weight   int64
	children map[string]*flameTree
}

// BuildFlame turns decoded stack samples into a bounded deterministic flame
// layout. Locations are reversed from pprof's leaf-first order.
func BuildFlame(profile *pprofprofile.Profile) (profilemodel.FlameGraph, error) {
	graph := profilemodel.FlameGraph{Status: "ready", NodeLimit: profilemodel.MaxFlameNodes, DepthLimit: profilemodel.MaxFlameDepth}
	if profile == nil || len(profile.SampleType) == 0 {
		graph.Status, graph.Reason = "unsupported", "sample-type-unavailable"
		return graph, nil
	}
	typeIndex := len(profile.SampleType) - 1
	graph.SampleType, graph.Unit = profile.SampleType[typeIndex].Type, profile.SampleType[typeIndex].Unit
	root := &flameTree{children: map[string]*flameTree{}}
	for _, sample := range profile.Sample {
		if typeIndex >= len(sample.Value) || sample.Value[typeIndex] == 0 {
			continue
		}
		value := sample.Value[typeIndex]
		weight := absSaturated(value)
		if math.MaxInt64-root.weight < weight {
			root.weight = math.MaxInt64
			graph.Truncated = true
		} else {
			root.weight += weight
		}
		current := root
		depth := 0
		for locationIndex := len(sample.Location) - 1; locationIndex >= 0; locationIndex-- {
			if depth >= profilemodel.MaxFlameDepth {
				graph.Truncated = true
				break
			}
			name := flameLocationName(sample.Location[locationIndex])
			child := current.children[name]
			if child == nil {
				child = &flameTree{name: name, children: map[string]*flameTree{}}
				current.children[name] = child
			}
			child.value = saturatedAdd(child.value, value)
			child.weight = saturatedAddPositive(child.weight, weight)
			current = child
			depth++
		}
	}
	graph.TotalWeight = root.weight
	if root.weight == 0 {
		graph.Status, graph.Reason = "unsupported", "no-positive-sample-weight"
		return graph, nil
	}
	emitFlameChildren(&graph, root, 0, 0, 10000)
	if len(graph.Nodes) == 0 {
		graph.Status, graph.Reason = "unsupported", "no-symbolized-stack"
	}
	return graph, nil
}

func emitFlameChildren(graph *profilemodel.FlameGraph, parent *flameTree, depth, x, width int) {
	children := make([]*flameTree, 0, len(parent.children))
	for _, child := range parent.children {
		children = append(children, child)
	}
	sort.Slice(children, func(i, j int) bool {
		if children[i].weight != children[j].weight {
			return children[i].weight > children[j].weight
		}
		return children[i].name < children[j].name
	})
	cursor := x
	for index, child := range children {
		if len(graph.Nodes) >= profilemodel.MaxFlameNodes {
			graph.Truncated = true
			return
		}
		childWidth := 0
		if parent.weight > 0 {
			childWidth = int((int64(width) * child.weight) / parent.weight)
		}
		if index == len(children)-1 && cursor < x+width {
			childWidth = x + width - cursor
		}
		if childWidth <= 0 {
			graph.Truncated = true
			continue
		}
		sign := "positive"
		if child.value < 0 {
			sign = "negative"
		}
		graph.Nodes = append(graph.Nodes, profilemodel.FlameNode{Function: child.name, Depth: depth, X: cursor, Width: childWidth, Value: child.value, Sign: sign})
		if depth+1 < profilemodel.MaxFlameDepth {
			emitFlameChildren(graph, child, depth+1, cursor, childWidth)
		} else if len(child.children) != 0 {
			graph.Truncated = true
		}
		cursor += childWidth
	}
}

func flameLocationName(location *pprofprofile.Location) string {
	name := "(unsymbolized)"
	if location != nil {
		for _, line := range location.Line {
			if line.Function != nil && line.Function.Name != "" {
				name = line.Function.Name
				break
			}
		}
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if len(name) > 256 {
		name = name[:256]
	}
	return name
}

func absSaturated(value int64) int64 {
	if value == math.MinInt64 {
		return math.MaxInt64
	}
	if value < 0 {
		return -value
	}
	return value
}

func saturatedAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

func saturatedAddPositive(a, b int64) int64 {
	if b < 0 || a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
