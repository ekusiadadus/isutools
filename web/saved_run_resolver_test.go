package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
