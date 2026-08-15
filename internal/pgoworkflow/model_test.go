package pgoworkflow

import (
	"bytes"
	"testing"
	"time"

	pprofprofile "github.com/google/pprof/profile"
)

func TestPrepareCandidateIsProvenanceBoundAndRollbackFirst(t *testing.T) {
	profileBody := cpuProfileFixture(t)
	candidate, err := Prepare(Input{
		RunID: "run-1", SnapshotBase: "20260815-120000_gen1_deadbeef", SnapshotSHA256: repeat("a", 64),
		ProfileSHA256: hashBytes(profileBody), ProfileBytes: uint64(len(profileBody)), CapturedBinarySHA256: repeat("b", 64),
		SourceRevision: repeat("c", 40), SourceDirty: false, Toolchain: "go1.24.6", MainPackage: "./cmd/server",
		Rationale: "representative passing diagnostic run", CreatedAt: time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Schema != SchemaV1 || len(candidate.CandidateID) != 64 || candidate.Profile.SHA256 != hashBytes(profileBody) {
		t.Fatalf("candidate=%#v", candidate)
	}
	if got := candidate.BuildPGO.Argv; len(got) != 6 || got[2] != "-pgo=CANDIDATE_DIR/default.pgo" || got[4] != "OUTPUT_BINARY" {
		t.Fatalf("pgo argv=%q", got)
	}
	if got := candidate.Rollback.Argv; len(got) != 6 || got[2] != "-pgo=off" {
		t.Fatalf("rollback argv=%q", got)
	}
	if candidate.Evaluation.Sequence != "ABBA" || !candidate.Evaluation.RequirePass || len(candidate.Evaluation.Results) != 0 {
		t.Fatalf("evaluation=%#v", candidate.Evaluation)
	}
}

func TestValidateCPUProfileRejectsHeapAndCorrupt(t *testing.T) {
	if err := ValidateCPUProfile(cpuProfileFixture(t)); err != nil {
		t.Fatal(err)
	}
	heap := &pprofprofile.Profile{SampleType: []*pprofprofile.ValueType{{Type: "alloc_space", Unit: "bytes"}}}
	var body bytes.Buffer
	if err := heap.Write(&body); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCPUProfile(body.Bytes()); err == nil {
		t.Fatal("heap profile accepted")
	}
	if err := ValidateCPUProfile([]byte("not a profile")); err == nil {
		t.Fatal("corrupt profile accepted")
	}
}

func TestPrepareRejectsWrongIdentityAndUnsafeFields(t *testing.T) {
	base := Input{
		RunID: "run-1", SnapshotBase: "20260815-120000_gen1_deadbeef", SnapshotSHA256: repeat("a", 64),
		ProfileSHA256: repeat("b", 64), ProfileBytes: 1, CapturedBinarySHA256: repeat("c", 64),
		SourceRevision: repeat("d", 40), Toolchain: "go1.24.6", MainPackage: "./cmd/server", Rationale: "representative",
		CreatedAt: time.Now(),
	}
	mutations := []func(*Input){
		func(value *Input) { value.SnapshotSHA256 = "bad" },
		func(value *Input) { value.ProfileBytes = 0 },
		func(value *Input) { value.MainPackage = "-toolexec=evil" },
		func(value *Input) { value.Rationale = "token\nsecret" },
		func(value *Input) { value.SourceDirty = true },
	}
	for index, mutate := range mutations {
		input := base
		mutate(&input)
		if _, err := Prepare(input); err == nil {
			t.Fatalf("mutation %d accepted", index)
		}
	}
}

func cpuProfileFixture(t *testing.T) []byte {
	t.Helper()
	profile := &pprofprofile.Profile{
		SampleType: []*pprofprofile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &pprofprofile.ValueType{Type: "cpu", Unit: "nanoseconds"}, Period: 10_000_000,
	}
	var body bytes.Buffer
	if err := profile.Write(&body); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func repeat(value string, count int) string {
	var out bytes.Buffer
	for index := 0; index < count; index++ {
		out.WriteString(value)
	}
	return out.String()
}
