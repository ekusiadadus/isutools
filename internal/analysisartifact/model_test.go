package analysisartifact

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCheckedInSchemaFixturePassesMachineValidation(t *testing.T) {
	file, err := os.Open("testdata/valid-accesslog-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	manifest, err := Decode(file)
	if err != nil || manifest.Kind != KindAccessLog || manifest.Status != StatusReady {
		t.Fatalf("manifest=%#v err=%v", manifest, err)
	}
}

func validManifest(t *testing.T) Manifest {
	t.Helper()
	m := Manifest{
		Schema:      SchemaV1,
		Kind:        KindAccessLog,
		GeneratedAt: time.Date(2026, 8, 15, 1, 2, 3, 0, time.FixedZone("JST", 9*60*60)),
		Analyzer: Analyzer{
			Name: "isutools-accesslog", Version: "v1.6.0", Revision: strings.Repeat("a", 40),
		},
		Status: StatusReady,
		Run: &RunBinding{
			RunID: "run-123", SnapshotBase: "20260815-run123", SnapshotSHA256: strings.Repeat("b", 64), SnapshotSchemaVersion: 3,
		},
		Inputs: []FileRef{{
			Role: "access-log", Name: "access.log", SHA256: strings.Repeat("c", 64), Bytes: 1234, MediaType: "text/plain", Visibility: VisibilityRestricted,
		}},
		Outputs: []FileRef{{
			Role: "summary", Name: "summary.json", SHA256: strings.Repeat("d", 64), Bytes: 321, MediaType: "application/json", Visibility: VisibilityPortable,
		}},
		Budget:   ResourceBudget{TimeoutMS: 30000, MaxInputBytes: 64 << 20, MaxOutputBytes: 4 << 20, MaxMemoryBytes: 256 << 20},
		Coverage: Coverage{Complete: true, Clock: "proxy-log", StartOffset: 100, EndOffset: 1334},
	}
	withID, err := SetArtifactID(m)
	if err != nil {
		t.Fatalf("SetArtifactID: %v", err)
	}
	return withID
}

func TestManifestCanonicalRoundTrip(t *testing.T) {
	want := validManifest(t)
	body, err := CanonicalJSON(want)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if bytes.Contains(body, []byte("+09:00")) {
		t.Fatalf("canonical timestamp was not normalized to UTC: %s", body)
	}
	got, err := Decode(bytes.NewReader(append(body, '\n')))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ArtifactID != want.ArtifactID || !got.GeneratedAt.Equal(want.GeneratedAt) {
		t.Fatalf("round trip = %#v, want id=%s time=%s", got, want.ArtifactID, want.GeneratedAt)
	}
}

func TestManifestRejectsUnknownAndTrailingJSON(t *testing.T) {
	body, err := CanonicalJSON(validManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(body, []byte(`"kind":`), []byte(`"unknown":true,"kind":`), 1)
	if _, err := Decode(bytes.NewReader(unknown)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := Decode(bytes.NewReader(append(body, []byte(` {}`)...))); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing error = %v", err)
	}
}

func TestManifestRejectsUnsafeOrInconsistentFields(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{name: "absolute input", edit: func(m *Manifest) { m.Inputs[0].Name = "/etc/passwd" }, want: "basename"},
		{name: "hash", edit: func(m *Manifest) { m.Inputs[0].SHA256 = "ABC" }, want: "sha256"},
		{name: "offset", edit: func(m *Manifest) { m.Coverage.EndOffset = 99 }, want: "offset"},
		{name: "portable raw log", edit: func(m *Manifest) { m.Inputs[0].Visibility = VisibilityPortable }, want: "input visibility"},
		{name: "ready without output", edit: func(m *Manifest) { m.Outputs = nil }, want: "ready"},
		{name: "unbound snapshot", edit: func(m *Manifest) { m.Run.RunID = "" }, want: "run binding"},
		{name: "diagnostic control", edit: func(m *Manifest) {
			m.Diagnostics = []Diagnostic{{Level: DiagnosticWarn, Code: "bad", Message: "secret\nline"}}
		}, want: "diagnostic"},
		{name: "budget", edit: func(m *Manifest) { m.Budget.MaxMemoryBytes = MaxMemoryBytes + 1 }, want: "memory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest(t)
			tc.edit(&m)
			m.ArtifactID = ""
			m, _ = SetArtifactID(m)
			if err := Validate(m); err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("Validate error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestManifestStatusRules(t *testing.T) {
	failed := validManifest(t)
	failed.Status = StatusFailed
	failed.Outputs = nil
	failed.Diagnostics = []Diagnostic{{Level: DiagnosticError, Code: "analyzer-failed", Message: "analyzer exited without a usable result"}}
	failed.ArtifactID = ""
	failed, err := SetArtifactID(failed)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(failed); err != nil {
		t.Fatalf("valid failed manifest: %v", err)
	}

	failed.Outputs = []FileRef{{Role: "summary", Name: "leak.json", SHA256: strings.Repeat("e", 64), Bytes: 1, MediaType: "application/json", Visibility: VisibilityPortable}}
	failed.ArtifactID = ""
	failed, _ = SetArtifactID(failed)
	if err := Validate(failed); err == nil {
		t.Fatal("failed manifest with output was accepted")
	}
}
