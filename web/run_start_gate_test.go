package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
)

func TestResetRejectsAbortedRunBeforeStartingProfiles(t *testing.T) {
	dir := t.TempDir()
	registry := health.NewRegistry()
	h := NewHandler(Provider{
		SQL:             agg.NewTable(agg.DefaultMaxKeys),
		DataDir:         dir,
		PprofDuration:   100 * time.Millisecond,
		RuntimeProfiles: []string{"heap"},
		Health:          registry,
		StartRun: func(context.Context) (RunStart, error) {
			return RunStart{
				RunID:     "run-required-failed",
				State:     "aborted",
				Validity:  "invalid",
				StartedAt: time.Now(),
			}, nil
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(runIDHeader); got != "run-required-failed" {
		t.Fatalf("%s = %q, want aborted run id", runIDHeader, got)
	}

	// A timer-based CPU capture starts asynchronously, so wait past its whole
	// duration before proving that neither it nor the synchronous heap opening
	// profile was created.
	time.Sleep(200 * time.Millisecond)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("aborted run published profile artifacts: %v", names)
	}

	_, partial := registry.Snapshot()
	if !partial {
		t.Fatal("aborted run was not recorded as degraded health")
	}
}

func TestResetRejectsUnknownRunStateBeforeStartingProfiles(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Provider{
		SQL:             agg.NewTable(agg.DefaultMaxKeys),
		DataDir:         dir,
		PprofDuration:   50 * time.Millisecond,
		RuntimeProfiles: []string{"heap"},
		StartRun: func(context.Context) (RunStart, error) {
			return RunStart{RunID: "run-unknown-state", Validity: "valid", StartedAt: time.Now()}, nil
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("unknown run state published %d profile artifacts", len(entries))
	}
}
