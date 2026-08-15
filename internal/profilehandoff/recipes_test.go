package profilehandoff

import (
	"strings"
	"testing"
)

func TestGenerateUsesBaseForCumulativeAndNeverTraceTool(t *testing.T) {
	recipes, err := Generate(ProfileInput{
		Kind: "allocs", Mode: ModeCumulativeDelta, Binary: "MATCHING_BINARY", BinaryMatch: true, SourceAvailable: true, SourceRoot: "/matching source",
		Inputs: []InputFile{{Point: "open", File: "allocs_open.pprof"}, {Point: "close", File: "allocs_close.pprof"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	web := findRecipe(t, recipes, "pprof-web")
	joined := strings.Join(web.Argv, " ")
	if !strings.Contains(joined, "go tool pprof -base allocs_open.pprof -http=:0 MATCHING_BINARY allocs_close.pprof") || strings.Contains(joined, "-diff_base") || strings.Contains(joined, "go tool trace") {
		t.Fatalf("recipe=%+v", web)
	}
	if !findRecipe(t, recipes, "pprof-source").Ready || !findRecipe(t, recipes, "pprof-disasm").Ready {
		t.Fatalf("source recipes=%+v", recipes)
	}
	for _, purpose := range []string{"pprof-weblist", "pprof-callers-callees", "pprof-ignore"} {
		if !findRecipe(t, recipes, purpose).Ready {
			t.Fatalf("%s recipe not ready: %+v", purpose, recipes)
		}
	}
	if source := strings.Join(findRecipe(t, recipes, "pprof-source").Argv, " "); !strings.Contains(source, "-source_path=/matching source") {
		t.Fatalf("source recipe=%q", source)
	}
}

func TestGenerateGatesBinarySourceAndLabels(t *testing.T) {
	recipes, err := Generate(ProfileInput{
		Kind: "cpu", Mode: ModeInterval, Binary: "MATCHING_BINARY", Inputs: []InputFile{{Point: "interval", File: "cpu.pprof"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if recipe := findRecipe(t, recipes, "pprof-web"); recipe.Ready || recipe.Code != "binary-match-required" {
		t.Fatalf("web=%+v", recipe)
	}
	if hasRecipe(recipes, "pprof-tagfocus") {
		t.Fatalf("tag recipe emitted without trusted labels")
	}

	recipes, err = Generate(ProfileInput{
		Kind: "cpu", Mode: ModeInterval, Binary: "MATCHING_BINARY", BinaryMatch: true, SourceAvailable: false, HasLabels: true,
		SampleTypes: []SampleType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		Inputs:      []InputFile{{Point: "interval", File: "cpu.pprof"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if recipe := findRecipe(t, recipes, "pprof-source"); recipe.Ready || recipe.Code != "source-unavailable" {
		t.Fatalf("source=%+v", recipe)
	}
	if !findRecipe(t, recipes, "pprof-tagfocus").Ready {
		t.Fatalf("trusted label recipe not ready")
	}
	if !findRecipe(t, recipes, "pprof-tagignore").Ready {
		t.Fatalf("trusted label ignore recipe not ready")
	}
	if recipe := findRecipe(t, recipes, "pprof-sample-cpu"); !recipe.Ready || recipe.SampleType != "cpu" || recipe.Unit != "nanoseconds" || !strings.Contains(strings.Join(recipe.Argv, " "), "-sample_index=cpu") {
		t.Fatalf("sample recipe=%+v", recipe)
	}
}

func TestGenerateRejectsUnsafeOrMissingSourceRoot(t *testing.T) {
	base := ProfileInput{Kind: "cpu", Mode: ModeInterval, Binary: "MATCHING_BINARY", BinaryMatch: true, SourceAvailable: true, Inputs: []InputFile{{Point: "interval", File: "cpu.pprof"}}}
	if _, err := Generate(base); err == nil {
		t.Fatal("source availability without a root accepted")
	}
	base.SourceRoot = "bad\nroot"
	if _, err := Generate(base); err == nil {
		t.Fatal("control character in source root accepted")
	}
}

func TestGenerateTraceUsesOnlyGoToolTrace(t *testing.T) {
	recipes, err := GenerateTrace("trace_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.out", true)
	if err != nil || len(recipes) != 1 {
		t.Fatalf("recipes=%+v err=%v", recipes, err)
	}
	if got := strings.Join(recipes[0].Argv, " "); got != "go tool trace trace_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.out" || !recipes[0].Ready {
		t.Fatalf("recipe=%+v", recipes[0])
	}
}

func TestComparisonUsesDiffBaseAndNormalizeOnlyWhenExplicit(t *testing.T) {
	current := ProfileInput{Kind: "cpu", Mode: ModeInterval, Binary: "MATCHING_BINARY", BinaryMatch: true, Inputs: []InputFile{{Point: "interval", File: "current.pprof"}}}
	base := ProfileInput{Kind: "cpu", Mode: ModeInterval, Binary: "MATCHING_BINARY", BinaryMatch: true, Inputs: []InputFile{{Point: "interval", File: "base.pprof"}}}
	recipe, err := Comparison(current, base, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(recipe.Argv, " ")
	if !strings.Contains(joined, "-diff_base base.pprof") || strings.Contains(joined, "-normalize") {
		t.Fatalf("recipe=%+v", recipe)
	}
	normalized, err := Comparison(current, base, true)
	if err != nil || !strings.Contains(strings.Join(normalized.Argv, " "), "-normalize") {
		t.Fatalf("normalized=%+v err=%v", normalized, err)
	}
	base.Kind = "heap"
	if _, err := Comparison(current, base, false); err == nil {
		t.Fatal("incompatible kinds accepted")
	}
}

func TestRenderShellQuotesAndRejectsControls(t *testing.T) {
	command, err := RenderShell([]string{"go", "tool", "pprof", "/tmp/a b/it's", "cpu.pprof"})
	if err != nil || command != `'go' 'tool' 'pprof' '/tmp/a b/it'"'"'s' 'cpu.pprof'` {
		t.Fatalf("command=%q err=%v", command, err)
	}
	if _, err := RenderShell([]string{"go", "bad\narg"}); err == nil {
		t.Fatal("newline argument accepted")
	}
}

func findRecipe(t *testing.T, recipes []Recipe, purpose string) Recipe {
	t.Helper()
	for _, recipe := range recipes {
		if recipe.Purpose == purpose {
			return recipe
		}
	}
	t.Fatalf("recipe %s not found: %+v", purpose, recipes)
	return Recipe{}
}

func hasRecipe(recipes []Recipe, purpose string) bool {
	for _, recipe := range recipes {
		if recipe.Purpose == purpose {
			return true
		}
	}
	return false
}
