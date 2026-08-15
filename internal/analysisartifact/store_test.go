package analysisartifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

func TestStorePublishLoadAndCAS(t *testing.T) {
	root, err := safefs.Open(t.TempDir(), safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	store := NewStore(root)
	first := validManifest(t)

	published, err := store.Publish("run-report", first, NoCurrentArtifact)
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		t.Fatalf("Publish first: %v", err)
	}
	if published.ArtifactID != first.ArtifactID || published.Sequence != 1 || published.ManifestFile == "" || published.ManifestSHA256 == "" {
		t.Fatalf("published = %#v", published)
	}
	loaded, err := store.LoadCurrent("run-report", KindAccessLog)
	if err != nil {
		t.Fatalf("LoadCurrent: %v", err)
	}
	if loaded.ArtifactID != first.ArtifactID || loaded.Sequence != 1 {
		t.Fatalf("loaded = %#v", loaded)
	}

	replayed, err := store.Publish("run-report", first, first.ArtifactID)
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		t.Fatalf("replay: %v", err)
	}
	if replayed.Sequence != 1 {
		t.Fatalf("replay sequence = %d, want 1", replayed.Sequence)
	}

	second := validManifest(t)
	second.Analyzer.Version = "v1.6.1"
	second.ArtifactID = ""
	second, err = SetArtifactID(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish("run-report", second, NoCurrentArtifact); err == nil {
		t.Fatal("stale expected-current publish succeeded")
	} else {
		var conflict *ConflictError
		if !errors.As(err, &conflict) || conflict.CurrentArtifactID != first.ArtifactID {
			t.Fatalf("conflict = %T %v", err, err)
		}
	}
	next, err := store.Publish("run-report", second, first.ArtifactID)
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		t.Fatalf("Publish second: %v", err)
	}
	if next.Sequence != 2 || next.PreviousArtifactID != first.ArtifactID {
		t.Fatalf("next = %#v", next)
	}
}

func TestStorePublishesContentAddressedPrivateOutput(t *testing.T) {
	dir := t.TempDir()
	root, err := safefs.Open(dir, safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	store := NewStore(root)
	body := []byte("bounded normalized summary\n")
	content := Content{Role: "summary", Extension: "json", MediaType: "application/json", Visibility: VisibilityPortable, Body: body, MaxBytes: 1024}
	ref, err := store.PublishContent("run-123", KindMySQLSlowLog, content)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Bytes != uint64(len(body)) || ref.Role != "summary" || ref.Name == "" || ref.SHA256 != hashBytes(body) {
		t.Fatalf("ref=%+v", ref)
	}
	info, err := os.Stat(filepath.Join(dir, ref.Name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	replay, err := store.PublishContent("run-123", KindMySQLSlowLog, content)
	if err != nil || replay != ref {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestStoreRejectsInvalidOrOversizedContent(t *testing.T) {
	root, err := safefs.Open(t.TempDir(), safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	store := NewStore(root)
	for _, content := range []Content{
		{Role: "../raw", Extension: "txt", MediaType: "text/plain", Visibility: VisibilityRestricted, Body: []byte("x"), MaxBytes: 1},
		{Role: "raw", Extension: "../txt", MediaType: "text/plain", Visibility: VisibilityRestricted, Body: []byte("x"), MaxBytes: 1},
		{Role: "raw", Extension: "txt", MediaType: "text/plain", Visibility: VisibilityRestricted, Body: []byte("secret"), MaxBytes: 1},
	} {
		if _, err := store.PublishContent("run-123", KindMySQLSlowLog, content); err == nil {
			t.Fatalf("accepted %+v", content)
		}
	}
}

func TestStoreRejectsUnsafeNamespaceAndTamperedCurrent(t *testing.T) {
	root, err := safefs.Open(t.TempDir(), safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	store := NewStore(root)
	if _, err := store.Publish("../escape", validManifest(t), NoCurrentArtifact); err == nil {
		t.Fatal("unsafe namespace accepted")
	}
	_, err = store.Publish("run-report", validManifest(t), NoCurrentArtifact)
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		t.Fatal(err)
	}
	marker := currentMarkerName("run-report", KindAccessLog)
	if err := root.Remove(marker); err != nil {
		t.Fatal(err)
	}
	f, err := root.CreateExclusive(marker, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"schema":"bad"}`)
	_ = f.Close()
	if _, err := store.LoadCurrent("run-report", KindAccessLog); err == nil || !strings.Contains(err.Error(), "current") {
		t.Fatalf("tampered current error = %v", err)
	}
}
