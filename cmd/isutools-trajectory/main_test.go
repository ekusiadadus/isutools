package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesPortableHTML(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "trace.ndjson")
	output := filepath.Join(dir, "trace.html")
	raw := strings.Join([]string{
		`{"type":"meta","schema":1,"title":"test trace"}`,
		`{"type":"agent","id":"a"}`,
		`{"type":"point","agent_id":"a","at":"2026-08-05T12:00:00Z","x":0,"y":0}`,
		`{"type":"job","id":"j","requested_at":"2026-08-05T12:00:00Z","pickup":{"x":1,"y":1},"destination":{"x":2,"y":2}}`,
		`{"type":"assignment","job_id":"j","agent_id":"a","at":"2026-08-05T12:00:00Z"}`,
	}, "\n")
	if err := os.WriteFile(input, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(input, output); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "test trace") || !strings.Contains(string(got), "dispatch p95") {
		t.Fatalf("output does not look like the trajectory viewer: %s", got[:min(len(got), 300)])
	}
	if matches, err := filepath.Glob(filepath.Join(dir, ".trace.html-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary outputs = %v, err = %v", matches, err)
	}
}
