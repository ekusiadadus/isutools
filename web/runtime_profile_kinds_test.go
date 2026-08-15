package web

import (
	"runtime/pprof"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAdditionalProfileKindsUseDeclaredSemantics(t *testing.T) {
	h, dir, _ := profileHandler(t, "allocs", "goroutine", "threadcreate")
	started := time.Now()
	run := RunStart{RunID: "run-moreprof1", Epoch: 1, StartedAt: started, Validity: "valid"}
	opened := h.captureRuntimeProfiles(run, ProfilePointOpen, 1)
	if len(opened) != 2 || slices.ContainsFunc(opened, func(name string) bool { return strings.Contains(name, "goroutine") }) {
		t.Fatalf("open=%v, want allocs/threadcreate cumulative baselines", opened)
	}
	closed := h.captureCloseProfiles(RunFinish{RunID: run.RunID, Epoch: run.Epoch, Validity: "valid", AcceptedAt: started.Add(time.Second)})
	if len(closed) != 3 {
		t.Fatalf("close=%v", closed)
	}
	manifest := h.profileManifestFor(run.RunID, run.Epoch)
	if manifest == nil || len(manifest.Pairs) != 2 || len(manifest.Expected) != 3 {
		t.Fatalf("manifest=%+v", manifest)
	}
	modes := map[string]ProfileExpectation{}
	for _, expectation := range manifest.Expected {
		modes[expectation.Kind] = expectation
	}
	if modes["allocs"].Mode != "cumulative-delta" || len(modes["allocs"].Inputs) != 2 ||
		modes["threadcreate"].Mode != "cumulative-delta" || len(modes["threadcreate"].Inputs) != 2 {
		t.Fatalf("cumulative expectations=%+v", modes)
	}
	if modes["goroutine"].Mode != "interval" || len(modes["goroutine"].Inputs) != 1 || modes["goroutine"].Inputs[0].Point != "interval" {
		t.Fatalf("goroutine expectation=%+v", modes["goroutine"])
	}
	if names := globNames(t, dir, "*_goroutine_open.pprof"); len(names) != 0 {
		t.Fatalf("goroutine open snapshots=%v", names)
	}
}

func TestRuntimeProfileSemanticsReportsAvailability(t *testing.T) {
	semantics := RuntimeProfileSemantics()
	byKind := map[string]RuntimeProfileSemantic{}
	for _, semantic := range semantics {
		byKind[semantic.Kind] = semantic
	}
	for _, kind := range []string{"mutex", "block", "heap", "allocs", "goroutine", "threadcreate", "goroutineleak"} {
		semantic, ok := byKind[kind]
		if !ok || semantic.Available != (pprof.Lookup(kind) != nil) || semantic.Mode == "" || len(semantic.SampleTypes) == 0 {
			t.Fatalf("semantic[%s]=%+v ok=%v", kind, semantic, ok)
		}
	}
}
