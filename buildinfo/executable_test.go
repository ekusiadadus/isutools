package buildinfo

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestCaptureFileHashesExactBytesAndSafeBuildProjection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	if err := os.WriteFile(path, []byte("running image"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := captureFile(path, SourceProcSelfExe, func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			GoVersion: "go1.24.0",
			Main:      debug.Module{Path: "example/app", Version: "v1.2.3", Sum: "h1:safe"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: strings.Repeat("a", 40)},
				{Key: "vcs.modified", Value: "true"},
				{Key: "-ldflags", Value: "-X main.token=top-secret"},
				{Key: "-pgo", Value: "/private/source/profile.pprof"},
				{Key: "GOAMD64", Value: "v3"},
			},
		}, true
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Status != ExecutableStatusCaptured || identity.Source != SourceProcSelfExe || len(identity.SHA256) != 64 || len(identity.BuildInfoSHA256) != 64 {
		t.Fatalf("identity = %#v", identity)
	}
	if identity.VCSRevision != strings.Repeat("a", 40) || !identity.VCSModified {
		t.Fatalf("VCS identity = %#v", identity)
	}
	encoded := identity.String()
	if strings.Contains(encoded, "top-secret") || strings.Contains(encoded, "/private/source") {
		t.Fatalf("secret build setting leaked: %s", encoded)
	}
	for _, want := range []string{"ldflags=present", "pgo=present", "GOAMD64=v3"} {
		if !strings.Contains(encoded, want) {
			t.Errorf("identity missing %q: %s", want, encoded)
		}
	}
}

func TestCaptureFileDetectsPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	if err := os.WriteFile(path, []byte("first"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := captureFile(path, SourcePathUnbound, noBI, func() {
		replacement := filepath.Join(dir, "replacement")
		_ = os.WriteFile(replacement, []byte("second"), 0o700)
		_ = os.Rename(replacement, path)
	})
	if err == nil || identity.Status != ExecutableStatusChangedDuringRead || identity.SHA256 != "" {
		t.Fatalf("replacement identity = %#v, err=%v", identity, err)
	}
}

func TestCaptureInputFileDoesNotRetainPath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "analysis-target")
	if err := os.WriteFile(path, []byte("target binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := CaptureInputFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Source != SourceInputFile || identity.Status != ExecutableStatusCaptured || len(identity.SHA256) != 64 || strings.Contains(identity.String(), path) {
		t.Fatalf("input identity = %#v", identity)
	}
}
