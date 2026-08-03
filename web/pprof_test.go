package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
)

func TestPprofIndexIsMounted(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("pprof index status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "goroutine") {
		t.Error("pprof index must list available profiles")
	}
}

func TestPprofNamedProfile(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pprof/goroutine", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("goroutine profile status = %d", rec.Code)
	}
}

func TestResetTriggersCPUCapture(t *testing.T) {
	dir := t.TempDir()
	tbl := agg.NewTable(agg.DefaultMaxKeys)
	h := NewHandler(Provider{
		SQL:           tbl,
		DataDir:       dir,
		PprofDuration: 150 * time.Millisecond,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof"))
		if len(matches) > 0 {
			info, err := os.Stat(matches[0])
			if err == nil && info.Size() > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("CPU profile was not captured after reset")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The captured profile must be listed on the index and downloadable.
	matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof"))
	name := filepath.Base(matches[0])

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), name) {
		t.Error("index must list captured profiles")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("profile download status = %d", rec.Code)
	}
}

func TestResetWithoutPprofDurationDoesNotCapture(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}
	time.Sleep(200 * time.Millisecond)
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof")); len(matches) != 0 {
		t.Errorf("no profile expected, got %v", matches)
	}
}
