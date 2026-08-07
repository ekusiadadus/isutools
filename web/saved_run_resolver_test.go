package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/httpstats"
)

func TestSavedRunResolverIgnoresDerivedHTMLCandidates(t *testing.T) {
	dir := t.TempDir()
	base := "20260806-120000_gen1_deadbee"
	if err := os.WriteFile(filepath.Join(dir, base+".html"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	derived := base + ".profile.render." + strings.Repeat("a", 64) + ".html"
	if err := os.WriteFile(filepath.Join(dir, derived), []byte("derived"), 0o600); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(Provider{DataDir: dir})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/20260806-120000", nil))
	if response.Code != http.StatusOK || response.Body.String() != "original" {
		t.Fatalf("saved detail = %d %q", response.Code, response.Body.String())
	}
}

func TestSavedRunCanBePreviewedWithCurrentRendererWithoutMutatingOriginal(t *testing.T) {
	dir := t.TempDir()
	base := "20260806-120000.000000000-000001_gen1_deadbee"
	original := []byte("immutable original report")
	if err := os.WriteFile(filepath.Join(dir, base+".html"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{HTTP: httpstats.Snapshot{{
		Key: "GET /api/app/notification HTTP/1.1 200", Count: 99,
		Total: 99 * time.Second, Avg: time.Second, P95: 1100 * time.Millisecond,
	}}}
	jsonBytes, err := json.Marshal(jsonPayload{Snapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+".json"), jsonBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(Provider{DataDir: dir})
	preview := httptest.NewRecorder()
	handler.ServeHTTP(preview, httptest.NewRequest(http.MethodGet, "/20260806-120000.000000000-000001?view=current", nil))
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "結論: 次に修正する場所") ||
		!strings.Contains(preview.Body.String(), "/api/app/notification") {
		t.Fatalf("current renderer preview = %d %q", preview.Code, preview.Body.String())
	}
	if preview.Header().Get("X-Isutools-View") != "current-renderer" {
		t.Fatalf("preview header = %q", preview.Header().Get("X-Isutools-View"))
	}

	stored, err := os.ReadFile(filepath.Join(dir, base+".html"))
	if err != nil || string(stored) != string(original) {
		t.Fatalf("preview mutated original: body=%q err=%v", stored, err)
	}
	originalResponse := httptest.NewRecorder()
	handler.ServeHTTP(originalResponse, httptest.NewRequest(http.MethodGet, "/20260806-120000.000000000-000001", nil))
	if originalResponse.Body.String() != string(original) {
		t.Fatalf("default saved detail changed = %q", originalResponse.Body.String())
	}
}
