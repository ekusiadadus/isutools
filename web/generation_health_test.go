package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/sqlstats"
)

func TestResetUsesAtomicSQLGenerationAndRetainsPrevious(t *testing.T) {
	store := sqlstats.NewStore(100)
	store.Observe("SELECT old", time.Millisecond)
	h := NewHandler(Provider{
		SQL:           store,
		SQLGeneration: store.CurrentGeneration,
		RotateSQL: func() (int64, []agg.Entry) {
			frozen := store.Rotate()
			return frozen.Generation, frozen.Entries
		},
	})

	reset := httptest.NewRecorder()
	h.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if reset.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d: %s", reset.Code, reset.Body.String())
	}
	store.Observe("SELECT new", 2*time.Millisecond)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Meta.Generation != 2 || len(payload.SQL) != 1 || payload.SQL[0].Key != "SELECT new" {
		t.Fatalf("current = %#v", payload.Snapshot)
	}
	if payload.Prev == nil || payload.Prev.Meta.Generation != 1 ||
		len(payload.Prev.SQL) != 1 || payload.Prev.SQL[0].Key != "SELECT old" {
		t.Fatalf("previous = %#v", payload.Prev)
	}
}

func TestSnapshotIncludesCollectorHealthAndPartial(t *testing.T) {
	registry := health.NewRegistry()
	registry.Set("sql", health.StatusOK, "")
	registry.Set("accesslog", health.StatusDegraded, "malformed line")
	registry.AddDropped("accesslog", 1)
	h := NewHandler(Provider{Health: registry})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Meta.Partial {
		t.Fatal("partial = false, want true")
	}
	if len(payload.Meta.Health) != 2 || payload.Meta.Health[0].Collector != "accesslog" {
		t.Fatalf("health = %#v", payload.Meta.Health)
	}
}

func TestReadEndpointsRejectOtherMethods(t *testing.T) {
	h := NewHandler(Provider{})
	for _, path := range []string{"/", "/json", "/snapshot.html"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST %s status = %d, want 405", path, rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Fatalf("Allow = %q, want GET", got)
			}
		})
	}
}
