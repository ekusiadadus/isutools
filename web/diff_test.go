package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/internal/agg"
)

func writeRunJSON(t *testing.T, dir, base, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, base+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+".html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newDiffHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	writeRunJSON(t, dir, "20260804-010000_gen2_aaa_score100",
		`{"meta":{"score":"100"},"sql":[{"key":"SELECT slow","count":100,"total_ns":5000000000},{"key":"SELECT fixed","count":50,"total_ns":9000000000}],"http":[{"key":"GET / HTTP/1.1 200","count":10,"total_ns":1000000000}]}`)
	writeRunJSON(t, dir, "20260804-020000_gen2_bbb_score200",
		`{"meta":{"score":"200"},"sql":[{"key":"SELECT slow","count":100,"total_ns":8000000000}],"http":[{"key":"GET / HTTP/1.1 200","count":10,"total_ns":500000000}]}`)
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir})
	return h, dir
}

func TestDiffComparesTwoRuns(t *testing.T) {
	h, _ := newDiffHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/diff?a=20260804-010000&b=20260804-020000", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"SELECT slow", "SELECT fixed", "score", "100", "200"} {
		if !strings.Contains(body, want) {
			t.Errorf("diff missing %q", want)
		}
	}
	// "SELECT fixed" disappeared in b: its delta (-9000ms) must be shown.
	if !strings.Contains(body, "-9000.0") {
		t.Errorf("expected resolved-query negative delta in body")
	}
}

func TestDiffUnknownRun404(t *testing.T) {
	h, _ := newDiffHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/diff?a=20260804-010000&b=20990101-000000", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDiffInvalidParams400(t *testing.T) {
	h, _ := newDiffHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/diff?a=..&b=x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestIndexLinksDiffToPreviousRun(t *testing.T) {
	h, _ := newDiffHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), "diff?a=20260804-010000&amp;b=20260804-020000") &&
		!strings.Contains(rec.Body.String(), "diff?a=20260804-010000&b=20260804-020000") {
		t.Error("index must link each run to a diff against the previous run")
	}
}
