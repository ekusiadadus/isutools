package profilecapture

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

func TestFileArtifactPublishesPrivateHashedImmutableFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := safefs.Open(dir, safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	factory := NewFileArtifactFactory(root, 32<<20)
	artifact, err := factory.New(validStartRequest(), strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("profile bytes")
	if _, err := artifact.Writer().Write(body); err != nil {
		t.Fatal(err)
	}
	published, err := artifact.Publish()
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		t.Fatalf("Publish: %v", err)
	}
	wantHash := sha256.Sum256(body)
	if published.SHA256 != hex.EncodeToString(wantHash[:]) || published.Bytes != int64(len(body)) {
		t.Fatalf("published = %#v", published)
	}
	info, err := os.Stat(dir + string(os.PathSeparator) + published.File)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	got, err := root.ReadFile(published.File, 1024)
	if err != nil || string(got) != string(body) {
		t.Fatalf("read = %q err=%v", got, err)
	}
	if _, err := artifact.Publish(); !errors.Is(err, ErrArtifactSettled) {
		t.Fatalf("second Publish = %v, want settled", err)
	}
}

func TestFileArtifactEnforcesWriteCeilingAndAbortsTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := safefs.Open(dir, safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	artifact, err := NewFileArtifactFactory(root, 4).New(validStartRequest(), strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(artifact.Writer(), "12345"); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("Write = %v, want ErrArtifactTooLarge", err)
	}
	if err := artifact.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Abort left files: %v", entries)
	}
}
