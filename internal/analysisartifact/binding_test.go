package analysisartifact

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

func TestVerifyRunBindingRejectsHashRunAndSchemaMismatch(t *testing.T) {
	directory := t.TempDir()
	body := []byte(`{"meta":{"schema_version":3,"run":{"run_id":"run-1"}}}`)
	if err := os.WriteFile(directory+"/snapshot.json", body, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(directory, safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	binding := RunBinding{RunID: "run-1", SnapshotBase: "snapshot", SnapshotSHA256: hashBytes(body), SnapshotSchemaVersion: 3}
	if err := VerifyRunBinding(root, binding); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*RunBinding){
		func(value *RunBinding) { value.SnapshotSHA256 = strings.Repeat("a", 64) },
		func(value *RunBinding) { value.RunID = "run-2" },
		func(value *RunBinding) { value.SnapshotSchemaVersion = 4 },
	} {
		copy := binding
		mutate(&copy)
		if err := VerifyRunBinding(root, copy); err == nil {
			t.Fatalf("accepted mismatch: %#v", copy)
		}
	}
}

func TestInspectTreatsUnknownSchemaAsUnsupported(t *testing.T) {
	unknown := `{"schema":"vendor.future/v9","kind":"future-kind","artifact_id":"` + strings.Repeat("a", 64) + `","status":"ready","new":{"field":true}}`
	inspection, err := Inspect(bytes.NewBufferString(unknown))
	if err != nil || inspection.Status != StatusUnsupported || inspection.Code != "unknown-schema" || inspection.Kind != "future-kind" {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	if _, err := Decode(bytes.NewBufferString(unknown)); err == nil {
		t.Fatal("strict decoder accepted unknown schema")
	}
}
