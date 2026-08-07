package pprofanalyze

import (
	"math"
	"reflect"
	"strings"
	"testing"

	pprofprofile "github.com/google/pprof/profile"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

func TestAnalyzeIntervalSeparatesGranularitiesAndDeduplicatesRecursion(t *testing.T) {
	p := syntheticProfile(10)
	decoded := &DecodedProfile{Profile: p, AbsoluteBudgets: []int64{10}}

	summaries, err := AnalyzeInterval(decoded, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %d, want 1", len(summaries))
	}
	s := summaries[0]
	if s.SampleType != "cpu" || s.Unit != "nanoseconds" || s.NetTotal != 10 ||
		s.PositiveTotal != 10 || s.NegativeMagnitude != 0 ||
		s.PercentDenominator != 10 || s.DenominatorMode != profilemodel.DenominatorNet {
		t.Fatalf("summary totals = %+v", s)
	}
	if len(s.Reports) != 4 {
		t.Fatalf("reports = %d, want 4", len(s.Reports))
	}

	functions := reportByGranularity(t, s.Reports, profilemodel.GranularityFunctions)
	wantFlat := []profilemodel.ProfileNode{{Function: "leaf.inline", Value: 10}}
	if !reflect.DeepEqual(functions.TopFlat, wantFlat) {
		t.Fatalf("function flat = %#v, want %#v", functions.TopFlat, wantFlat)
	}
	wantCum := []profilemodel.ProfileNode{
		{Function: "caller", Value: 10},
		{Function: "leaf.inline", Value: 10},
		{Function: "leaf.outer", Value: 10},
	}
	if !reflect.DeepEqual(functions.TopCumulative, wantCum) {
		t.Fatalf("function cumulative = %#v, want %#v", functions.TopCumulative, wantCum)
	}

	lines := reportByGranularity(t, s.Reports, profilemodel.GranularityLines)
	if !reflect.DeepEqual(lines.TopFlat, []profilemodel.ProfileNode{{Function: "leaf.inline", File: "leaf.go", Line: 11, Value: 10}}) {
		t.Fatalf("line flat = %#v", lines.TopFlat)
	}
	// caller appears twice in the stack but contributes once per sample.
	callerCount := 0
	for _, node := range lines.TopCumulative {
		if node.Function == "caller" && node.File == "caller.go" && node.Line == 22 {
			callerCount++
		}
	}
	if callerCount != 1 {
		t.Fatalf("recursive caller entries = %d, want 1: %#v", callerCount, lines.TopCumulative)
	}
}

func TestAnalyzeDeltaPreservesNegativeValuesAndInputs(t *testing.T) {
	open := syntheticProfile(12)
	closeProfile := syntheticProfile(5)
	openBefore := profileValues(open)
	closeBefore := profileValues(closeProfile)

	summaries, err := AnalyzeDelta(
		&DecodedProfile{Profile: open, AbsoluteBudgets: []int64{12}},
		&DecodedProfile{Profile: closeProfile, AbsoluteBudgets: []int64{5}},
		50,
	)
	if err != nil {
		t.Fatal(err)
	}
	s := summaries[0]
	if s.NetTotal != -7 || s.PositiveTotal != 0 || s.NegativeMagnitude != 7 ||
		s.PercentDenominator != 7 || s.DenominatorMode != profilemodel.DenominatorAbsoluteAddress {
		t.Fatalf("delta totals = %+v", s)
	}
	functions := reportByGranularity(t, s.Reports, profilemodel.GranularityFunctions)
	if len(functions.TopFlat) != 0 || len(functions.TopNegativeFlat) != 1 || functions.TopNegativeFlat[0].Value != -7 {
		t.Fatalf("delta function flat = positive %#v negative %#v", functions.TopFlat, functions.TopNegativeFlat)
	}
	if !reflect.DeepEqual(profileValues(open), openBefore) || !reflect.DeepEqual(profileValues(closeProfile), closeBefore) {
		t.Fatal("AnalyzeDelta mutated an input profile")
	}
}

func TestAnalyzeIntervalRejectsNegativeAndOverflow(t *testing.T) {
	negative := syntheticProfile(-1)
	if _, err := AnalyzeInterval(&DecodedProfile{Profile: negative, AbsoluteBudgets: []int64{1}}, 50); err == nil || !strings.Contains(err.Error(), profilemodel.DiagnosticNegativeIntervalSample) {
		t.Fatalf("negative interval error = %v", err)
	}

	large := syntheticProfile(math.MaxInt64)
	large.Sample = append(large.Sample, &pprofprofile.Sample{Location: large.Sample[0].Location, Value: []int64{1}})
	if _, err := AnalyzeInterval(&DecodedProfile{Profile: large, AbsoluteBudgets: []int64{math.MaxInt64}}, 50); err == nil || !strings.Contains(err.Error(), profilemodel.DiagnosticSampleValueOverflow) {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestAnalyzeDeltaRejectsCombinedAbsoluteBudgetOverflow(t *testing.T) {
	open := syntheticProfile(math.MaxInt64)
	closeProfile := syntheticProfile(1)
	_, err := AnalyzeDelta(
		&DecodedProfile{Profile: open, AbsoluteBudgets: []int64{math.MaxInt64}},
		&DecodedProfile{Profile: closeProfile, AbsoluteBudgets: []int64{1}},
		50,
	)
	if err == nil || !strings.Contains(err.Error(), profilemodel.DiagnosticSampleValueOverflow) {
		t.Fatalf("combined budget error = %v", err)
	}
}

func TestAnalyzeUsesDeterministicTieOrderAndTopLimit(t *testing.T) {
	p := syntheticProfile(1)
	for i := 0; i < 55; i++ {
		fn := &pprofprofile.Function{ID: uint64(100 + i), Name: "fn." + string(rune('a'+i%26)), Filename: "many.go"}
		loc := &pprofprofile.Location{ID: uint64(100 + i), Address: uint64(100 + i), Line: []pprofprofile.Line{{Function: fn, Line: int64(i + 1)}}}
		p.Function = append(p.Function, fn)
		p.Location = append(p.Location, loc)
		p.Sample = append(p.Sample, &pprofprofile.Sample{Location: []*pprofprofile.Location{loc}, Value: []int64{1}})
	}
	p.Sample = p.Sample[1:]
	decoded := &DecodedProfile{Profile: p, AbsoluteBudgets: []int64{55}}
	summaries, err := AnalyzeInterval(decoded, 50)
	if err != nil {
		t.Fatal(err)
	}
	report := reportByGranularity(t, summaries[0].Reports, profilemodel.GranularityLines)
	if len(report.TopFlat) != 50 {
		t.Fatalf("top flat = %d, want 50", len(report.TopFlat))
	}
	for i := 1; i < len(report.TopFlat); i++ {
		previous, current := report.TopFlat[i-1], report.TopFlat[i]
		if previous.Function > current.Function && previous.Value == current.Value {
			t.Fatalf("tie order is not deterministic: %#v before %#v", previous, current)
		}
	}
}

func reportByGranularity(t *testing.T, reports []profilemodel.ProfileReport, granularity string) profilemodel.ProfileReport {
	t.Helper()
	for _, report := range reports {
		if report.Granularity == granularity {
			return report
		}
	}
	t.Fatalf("missing report %q", granularity)
	return profilemodel.ProfileReport{}
}

func syntheticProfile(value int64) *pprofprofile.Profile {
	leafInline := &pprofprofile.Function{ID: 1, Name: "leaf.inline", Filename: "leaf.go"}
	leafOuter := &pprofprofile.Function{ID: 2, Name: "leaf.outer", Filename: "leaf.go"}
	caller := &pprofprofile.Function{ID: 3, Name: "caller", Filename: "caller.go"}
	leaf := &pprofprofile.Location{ID: 1, Address: 1, Line: []pprofprofile.Line{
		{Function: leafInline, Line: 11},
		{Function: leafOuter, Line: 12},
	}}
	call := &pprofprofile.Location{ID: 2, Address: 2, Line: []pprofprofile.Line{{Function: caller, Line: 22}}}
	return &pprofprofile.Profile{
		SampleType: []*pprofprofile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Sample:     []*pprofprofile.Sample{{Location: []*pprofprofile.Location{leaf, call, call}, Value: []int64{value}}},
		Location:   []*pprofprofile.Location{leaf, call},
		Function:   []*pprofprofile.Function{leafInline, leafOuter, caller},
		PeriodType: &pprofprofile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     1,
	}
}

func profileValues(profile *pprofprofile.Profile) [][]int64 {
	values := make([][]int64, len(profile.Sample))
	for i, sample := range profile.Sample {
		values[i] = append([]int64(nil), sample.Value...)
	}
	return values
}
