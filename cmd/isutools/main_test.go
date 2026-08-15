package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const accessFixture = `{"schema":"isutools.http-access.v1","method":"GET","uri":"/items?token=secret","status":503,"duration_ms":125,"bytes":42}` + "\n"

func TestRunInspectAccesslogFromStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"inspect", "accesslog", "--format", "isutools-json-v1", "--where", "status >= 500",
		"--percentiles", "50,99", "--output", "markdown", "--columns", "method,path,count,p50_ns,p99_ns",
	}, strings.NewReader(accessFixture), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "/items") || strings.Contains(stdout.String(), "secret") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "parsed=1") || strings.Contains(stderr.String(), "secret") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunOfflineInspectIsExplicitlyAvailableWhenLibraryOff(t *testing.T) {
	t.Setenv("ISUTOOLS", "off")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"inspect", "accesslog", "--format", "isutools-json-v1", "--output", "json"}, strings.NewReader(accessFixture), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"schema": "isutools.accesslog-inspect/v1"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsSymlinkInput(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "access.log")
	if err := os.WriteFile(target, []byte(accessFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"inspect", "accesslog", "--file", link}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "input-not-regular") || strings.Contains(stderr.String(), target) {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunRejectsHardlinkInput(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "access.log")
	alias := filepath.Join(dir, "alias.log")
	if err := os.WriteFile(target, []byte(accessFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, alias); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"inspect", "accesslog", "--file", alias}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "input-not-regular") || strings.Contains(stderr.String(), target) {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunReportsStableErrorsWithoutEchoingFilter(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"inspect", "accesslog", "--where", `cookie = super-secret`}, strings.NewReader(accessFixture), &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "unsupported-field") || strings.Contains(stderr.String(), "super-secret") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestRunUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stderr.String(), "isutools inspect accesslog") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunHelpIsSuccessful(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"inspect", "accesslog", "--help"}, {"analyze", "mysql-slowlog", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "usage:") || stderr.Len() != 0 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunAnalyzeSlowlogPortableSummary(t *testing.T) {
	input := `# Query_time: 0.125 Lock_time: 0.010 Rows_sent: 1 Rows_examined: 42
SET timestamp=1786755600;
SELECT * FROM users WHERE email='private@example.com' AND id=123;
`
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"analyze", "mysql-slowlog"}, strings.NewReader(input), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"schema": "isutools.mysql-slowlog/v1"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, secret := range []string{"private@example.com", "SELECT *", "id=123"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatalf("output leaked %q", secret)
		}
	}
}

func TestRunAnalyzeSlowlogPublishesBoundArtifact(t *testing.T) {
	input := `# Query_time: 0.125 Lock_time: 0.010 Rows_sent: 1 Rows_examined: 42
SET timestamp=1786755600;
SELECT 123;
`
	dir := t.TempDir()
	snapshot := []byte(`{"meta":{"schema_version":3,"run":{"run_id":"run-123"}}}`)
	if err := os.WriteFile(filepath.Join(dir, "snapshot-123.json"), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"analyze", "mysql-slowlog", "--data-dir", dir, "--namespace", "run-123",
		"--run-id", "run-123", "--snapshot-base", "snapshot-123", "--snapshot-sha256", testHash(snapshot), "--snapshot-schema", "3",
	}, strings.NewReader(input), &stdout, &stderr)
	if code != 0 || !strings.Contains(stderr.String(), "artifact=") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		body, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr == nil && strings.Contains(string(body), "SELECT 123") {
			t.Fatalf("raw SQL persisted in %s", entry.Name())
		}
	}
	joined := strings.Join(names, "\n")
	if !strings.Contains(joined, "mysql-slowlog.analysis.current.json") || !strings.Contains(joined, "analysis-") {
		t.Fatalf("files=%v", names)
	}
}

func TestRunInspectAccesslogPublishesOnlyAgainstExactSnapshot(t *testing.T) {
	directory := t.TempDir()
	snapshot := []byte(`{"meta":{"schema_version":3,"run":{"run_id":"run-access"}}}`)
	if err := os.WriteFile(filepath.Join(directory, "snapshot-access.json"), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"inspect", "accesslog", "--format", "isutools-json-v1", "--output", "json", "--data-dir", directory,
		"--run-id", "run-access", "--snapshot-base", "snapshot-access", "--snapshot-sha256", testHash(snapshot), "--snapshot-schema", "3",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, strings.NewReader(accessFixture), &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "artifact=") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	args[13] = strings.Repeat("a", 64)
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), args, strings.NewReader(accessFixture), &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "snapshot sha256 mismatch") {
		t.Fatalf("mismatch code=%d stderr=%q", code, stderr.String())
	}
}

func testHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
