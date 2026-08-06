package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndexListsTrajectoryViewersSeparatelyFromRuns(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"20260805-120000.000000000-000001_gen1_rev_score1.html",
		"trajectory_20260805-120000_run-1.html",
		"unrelated.html",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	h := NewHandler(Provider{DataDir: dir})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `files/trajectory_20260805-120000_run-1.html`) {
		t.Fatalf("trajectory link missing: %s", body)
	}
	if strings.Contains(body, `href="trajectory_20260805-120000_run-1`) {
		t.Fatalf("trajectory must not also appear as a run: %s", body)
	}
	if strings.Contains(body, "unrelated.html") {
		t.Fatalf("unrelated HTML leaked into artifact listings: %s", body)
	}
}
