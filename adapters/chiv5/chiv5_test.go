package chiv5

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/go-chi/chi/v5"
)

func TestChiNativePatternsDoNotLeakParameters(t *testing.T) {
	collector := httpstats.New()
	r := chi.NewRouter()
	Install(r)
	r.Route("/api", func(r chi.Router) {
		r.Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	})
	handler := collector.Middleware(r)
	for _, target := range []string{"/api/users/alice@example.invalid", "/api/users/bob@example.invalid", "/not-found/private"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}
	entries := collector.Snapshot()
	paths := make([]string, 0, len(entries))
	encoded := ""
	for _, entry := range entries {
		paths = append(paths, entry.Path)
		encoded += entry.Path + entry.Key
	}
	sort.Strings(paths)
	want := []string{"/api/users/{id}", "/{not-found}"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths=%v entries=%#v", paths, entries)
	}
	for _, secret := range []string{"alice", "bob", "private"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("route metrics leaked %q", secret)
		}
	}
}

func TestChiScenarioMiddlewareSetsTrustedScenario(t *testing.T) {
	t.Setenv(sessionlabel.EnvFlowLabels, "on")
	t.Setenv(sessionlabel.EnvSourceCookie, "SESSIONID")
	t.Setenv(sessionlabel.EnvHMACKey, strings.Repeat("c", sessionlabel.MinKeyBytes))
	r := chi.NewRouter()
	r.With(Scenario("viewer")).Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/users/private-id", nil)
	req.AddCookie(&http.Cookie{Name: "SESSIONID", Value: "raw-cookie"})
	rec := httptest.NewRecorder()
	isutools.HTTP(r).ServeHTTP(rec, req)
	if got := rec.Header().Get(sessionlabel.ScenarioHeaderName); got != "viewer" {
		t.Fatalf("scenario = %q", got)
	}
}
