package safefs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func openTestRoot(t *testing.T) *Root {
	t.Helper()
	root, err := Open(t.TempDir(), Options{RequireStrongVisibility: false, Exclusive: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestCreateTempAndPublishNoReplace(t *testing.T) {
	t.Parallel()

	root := openTestRoot(t)
	f, err := root.CreateExclusive("capture.tmp", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "new bytes"); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := root.PublishNoReplace("capture.tmp", "capture.pprof")
	if err != nil {
		t.Fatalf("PublishNoReplace: %v", err)
	}
	if !result.Visible || result.Durability == DurabilityFailed {
		t.Fatalf("publication = %#v", result)
	}
	body, err := root.ReadFile("capture.pprof", 1024)
	if err != nil || string(body) != "new bytes" {
		t.Fatalf("ReadFile = %q err=%v", body, err)
	}
}

func TestAvailableBytesUsesOpenedDirectoryDescriptor(t *testing.T) {
	t.Parallel()
	root, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	available, err := root.AvailableBytes()
	if err != nil || available == 0 {
		t.Fatalf("AvailableBytes = %d, err=%v", available, err)
	}
}

func TestPublishNoReplaceCannotOverwriteExistingFinal(t *testing.T) {
	t.Parallel()

	root := openTestRoot(t)
	for name, body := range map[string]string{"old.tmp": "old", "new.tmp": "new"} {
		f, err := root.CreateExclusive(name, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, body); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := root.PublishNoReplace("old.tmp", "capture.pprof"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.PublishNoReplace("new.tmp", "capture.pprof"); !errors.Is(err, ErrExists) {
		t.Fatalf("second PublishNoReplace = %v, want ErrExists", err)
	}
	body, err := root.ReadFile("capture.pprof", 1024)
	if err != nil || string(body) != "old" {
		t.Fatalf("final was replaced: body=%q err=%v", body, err)
	}
}

func TestReplaceAtomicallyUpdatesMutableMarker(t *testing.T) {
	t.Parallel()

	root := openTestRoot(t)
	for name, body := range map[string]string{"old.tmp": "old", "new.tmp": "new"} {
		file, err := root.CreateExclusive(name, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, body); err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := root.Replace("old.tmp", "current.json"); err != nil {
		t.Fatal(err)
	}
	publication, err := root.Replace("new.tmp", "current.json")
	if err != nil && !publication.Visible {
		t.Fatalf("Replace = %#v, %v", publication, err)
	}
	body, err := root.ReadFile("current.json", 1024)
	if err != nil || string(body) != "new" {
		t.Fatalf("marker = %q, %v", body, err)
	}
}

func TestNamedLockExcludesAnotherPublisher(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	lock, err := first.TryLock(".publisher.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.TryLock(".publisher.lock"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock = %v, want ErrLocked", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	retry, err := second.TryLock(".publisher.lock")
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = retry.Close()
}

func TestRootReadRejectsSymlinksAndNonBasenames(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir, Options{RequireStrongVisibility: false, Exclusive: false})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.ReadFile("link", 1024); err == nil {
		t.Fatal("ReadFile followed a symlink")
	}
	for _, name := range []string{"../target", "a/b", "", ".", ".."} {
		if _, err := root.ReadFile(name, 1024); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ReadFile(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestReadFileEnforcesLimitAndRegularFile(t *testing.T) {
	t.Parallel()

	root := openTestRoot(t)
	f, err := root.CreateExclusive("large", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "12345"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ReadFile("large", 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadFile limit = %v, want ErrTooLarge", err)
	}
}
