package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDataDirReadsRejectSymlinkArtifacts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.html"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.json"), []byte(`{"schema_version":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runID := "20260806-130000.000000000-000001"
	htmlName := runID + "_gen1_deadbee.html"
	jsonName := runID + "_gen1_deadbee.json"
	if err := os.Symlink(filepath.Join(outside, "secret.html"), filepath.Join(dir, htmlName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.json"), filepath.Join(dir, jsonName)); err != nil {
		t.Fatal(err)
	}
	h := newHandler(Provider{DataDir: dir})

	files := httptest.NewRecorder()
	h.routes().ServeHTTP(files, httptest.NewRequest(http.MethodGet, "/files/"+htmlName, nil))
	if files.Code == http.StatusOK || files.Body.String() == "secret" {
		t.Fatalf("/files followed symlink: status=%d body=%q", files.Code, files.Body.String())
	}

	detail := httptest.NewRecorder()
	h.routes().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/"+runID, nil))
	if detail.Code == http.StatusOK || detail.Body.String() == "secret" {
		t.Fatalf("saved-run detail followed symlink: status=%d body=%q", detail.Code, detail.Body.String())
	}

	if _, err := h.loadRun(runID); err == nil {
		t.Fatal("loadRun followed a JSON symlink")
	}
}
