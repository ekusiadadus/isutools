package buildinfo

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestCaptureInputFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureInputFile(link); err == nil {
		t.Fatal("CaptureInputFile followed a symlink")
	}
}

func TestCaptureInputFileRejectsHardlink(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("hard-link identity check is provided on supported Unix targets")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	alias := filepath.Join(dir, "alias")
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, alias); err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if _, err := CaptureInputFile(target); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("CaptureInputFile hard link = %v", err)
	}
}

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
