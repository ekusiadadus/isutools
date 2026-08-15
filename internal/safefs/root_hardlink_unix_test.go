//go:build darwin || linux

package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRootReadRejectsHardlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	alias := filepath.Join(dir, "alias")
	if err := os.WriteFile(target, []byte("ambiguous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, alias); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.ReadFile("target", 1024); !errors.Is(err, ErrAmbiguousLink) {
		t.Fatalf("ReadFile hard link = %v, want ErrAmbiguousLink", err)
	}
	if _, err := root.ReadFile("alias", 1024); !errors.Is(err, ErrAmbiguousLink) {
		t.Fatalf("ReadFile alias = %v, want ErrAmbiguousLink", err)
	}
}
