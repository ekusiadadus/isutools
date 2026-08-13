package pprofanalyze

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	pprofprofile "github.com/google/pprof/profile"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

func TestBuildFlameIsDeterministicAndBoundsGeometry(t *testing.T) {
	profile := syntheticProfile(10)
	first, err := BuildFlame(profile)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildFlame(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.Status != "ready" || first.TotalWeight != 10 || len(first.Nodes) == 0 {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	for _, node := range first.Nodes {
		if node.X < 0 || node.Width <= 0 || node.X+node.Width > 10000 || node.Depth >= profilemodel.MaxFlameDepth {
			t.Fatalf("node geometry=%#v", node)
		}
	}
}

func TestBuildFlameBoundsNodesDepthAndSymbolText(t *testing.T) {
	profile := &pprofprofile.Profile{SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}}
	for i := 0; i < profilemodel.MaxFlameNodes+100; i++ {
		name := fmt.Sprintf("fn-%05d-", i) + strings.Repeat("x", 300) + "<script>"
		fn := &pprofprofile.Function{ID: uint64(i + 1), Name: name}
		loc := &pprofprofile.Location{ID: uint64(i + 1), Line: []pprofprofile.Line{{Function: fn}}}
		profile.Function = append(profile.Function, fn)
		profile.Location = append(profile.Location, loc)
		profile.Sample = append(profile.Sample, &pprofprofile.Sample{Location: []*pprofprofile.Location{loc}, Value: []int64{1}})
	}
	graph, err := BuildFlame(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Truncated || len(graph.Nodes) > profilemodel.MaxFlameNodes {
		t.Fatalf("nodes=%d truncated=%v", len(graph.Nodes), graph.Truncated)
	}
	for _, node := range graph.Nodes {
		if len(node.Function) > 256 {
			t.Fatalf("unbounded function length=%d", len(node.Function))
		}
	}
}

func TestBuildFlameReportsUnsupportedEmptyProfile(t *testing.T) {
	graph, err := BuildFlame(&pprofprofile.Profile{})
	if err != nil || graph.Status != "unsupported" || graph.Reason == "" || len(graph.Nodes) != 0 {
		t.Fatalf("graph=%#v err=%v", graph, err)
	}
}
