package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pprofprofile "github.com/google/pprof/profile"

	localbuildinfo "github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/internal/pgoworkflow"
	"github.com/ekusiadadus/isutools/web"
)

func TestReadRegularRejectsHardlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "snapshot.json")
	alias := filepath.Join(dir, "alias.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegular(alias, 1024); err == nil {
		t.Fatal("readRegular accepted a hard-linked input")
	}
}

func TestPrepareCreatesPrivateNoOverwriteCandidateWithoutChangingSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module example.test/app\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "cmd", "server"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "cmd", "server", "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"add", "go.mod", "cmd/server/main.go"}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "initial"}} {
		command := exec.Command("git", append([]string{"-C", source}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %q: %v: %s", args, err, output)
		}
	}
	revisionBefore, dirtyBefore, err := sourceState(source)
	if err != nil || dirtyBefore {
		t.Fatalf("source before revision=%q dirty=%v err=%v", revisionBefore, dirtyBefore, err)
	}
	profileBody := testCPUProfile(t)
	profileName := "cpu_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.pprof"
	profilePath := filepath.Join(root, profileName)
	if err := os.WriteFile(profilePath, profileBody, 0o600); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(root, "server")
	if err := os.WriteFile(binaryPath, []byte("captured target binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	binaryIdentity, err := localbuildinfo.CaptureInputFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	toolchain, err := goVersion()
	if err != nil {
		t.Fatal(err)
	}
	binaryIdentity.Source = localbuildinfo.SourceProcSelfExe
	binaryIdentity.GoVersion = toolchain
	binaryIdentity.VCSRevision = revisionBefore
	snapshot := struct {
		web.Snapshot
	}{Snapshot: web.Snapshot{Meta: web.Meta{SchemaVersion: 3, Run: &web.RunInfo{RunID: "run-pgo", Epoch: 1}, Profiles: &web.ProfileManifest{
		RunID: "run-pgo", Epoch: 1, Executable: &binaryIdentity,
		CPU: &web.CPUIntervalCapture{ExpectedFile: profileName, File: profileName, SHA256: hashBytes(profileBody), Bytes: int64(len(profileBody)), Status: "published", Complete: true},
	}}}}
	snapshotBody, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(root, "snapshot-pgo.json")
	if err := os.WriteFile(snapshotPath, snapshotBody, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "candidate")
	args := []string{
		"--snapshot", snapshotPath, "--snapshot-sha256", hashBytes(snapshotBody), "--profile", profilePath,
		"--binary", binaryPath, "--source-dir", source, "--main-package", "./cmd/server", "--output-dir", output,
		"--rationale", "representative passing diagnostic run",
	}
	var stdout bytes.Buffer
	if err := runPrepare(args, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "rollback: go build -pgo=off") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	manifestFile, err := os.Open(filepath.Join(output, "candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := pgoworkflow.Decode(manifestFile)
	_ = manifestFile.Close()
	if err != nil || candidate.SourceRevision != revisionBefore || candidate.CapturedBinarySHA256 != binaryIdentity.SHA256 {
		t.Fatalf("candidate=%#v err=%v", candidate, err)
	}
	for _, name := range []string{"default.pgo", "candidate.json", "candidate.ready.json"} {
		info, err := os.Stat(filepath.Join(output, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s info=%v err=%v", name, info, err)
		}
	}
	revisionAfter, dirtyAfter, err := sourceState(source)
	if err != nil || dirtyAfter || revisionAfter != revisionBefore {
		t.Fatalf("source changed revision=%q dirty=%v err=%v", revisionAfter, dirtyAfter, err)
	}
	if err := runPrepare(args, &stdout); err == nil || !strings.Contains(err.Error(), "must-not-exist") {
		t.Fatalf("overwrite err=%v", err)
	}
	t.Setenv("GOFLAGS", "-definitely-invalid-inherited-flag")
	stdout.Reset()
	if err := runBuild([]string{"--candidate-dir", output, "--source-dir", source, "--variant", "pgo"}, &stdout); err != nil {
		t.Fatal(err)
	}
	var built buildVerification
	if err := json.Unmarshal(stdout.Bytes(), &built); err != nil || !built.PGOEnabled || built.BinaryBytes == 0 || built.DurationNS <= 0 || built.SourceRevision != revisionBefore {
		t.Fatalf("build=%+v err=%v output=%q", built, err, stdout.String())
	}
	for _, item := range []struct {
		name string
		mode os.FileMode
	}{{"candidate-pgo.bin", 0o700}, {"build-pgo.json", 0o600}} {
		info, err := os.Stat(filepath.Join(output, item.name))
		if err != nil || info.Mode().Perm() != item.mode {
			t.Fatalf("%s info=%v err=%v", item.name, info, err)
		}
	}
	if err := runBuild([]string{"--candidate-dir", output, "--source-dir", source, "--variant", "pgo"}, &stdout); err == nil || !strings.Contains(err.Error(), "must-not-exist") {
		t.Fatalf("build overwrite err=%v", err)
	}
	stdout.Reset()
	if err := runBuild([]string{"--candidate-dir", output, "--source-dir", source, "--variant", "off"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &built); err != nil || built.PGOEnabled || built.BinaryBytes == 0 {
		t.Fatalf("off build=%+v err=%v", built, err)
	}
	stdout.Reset()
	if err := runVerifyBuild([]string{"--candidate-dir", output, "--binary", filepath.Join(output, "candidate-pgo.bin"), "--variant", "pgo"}, &stdout); err != nil {
		t.Fatal(err)
	}
	bad := append([]string(nil), args...)
	bad[3] = strings.Repeat("a", 64)
	bad[len(bad)-3] = filepath.Join(root, "bad-candidate")
	if err := runPrepare(bad, &stdout); err == nil || !strings.Contains(err.Error(), "snapshot-sha-mismatch") {
		t.Fatalf("mismatch err=%v", err)
	}
}

func TestCLIUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{nil, {"unknown"}, {"prepare"}, {"build"}, {"verify-build"}} {
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("args=%q code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

func TestCLIHelpIsSuccessful(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"prepare", "--help"}, {"build", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "usage:") || stderr.Len() != 0 {
			t.Fatalf("args=%v code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func testCPUProfile(t *testing.T) []byte {
	t.Helper()
	profile := &pprofprofile.Profile{SampleType: []*pprofprofile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}}, PeriodType: &pprofprofile.ValueType{Type: "cpu", Unit: "nanoseconds"}}
	var body bytes.Buffer
	if err := profile.Write(&body); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}
