package gojiv2

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
)

func TestRouteUsesExplicitPattern(t *testing.T) {
	h := Route("/rooms/:id", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	httpstats.Default.Reset()
	httpstats.Middleware(h).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/rooms/secret", nil))
	snap := httpstats.Default.Snapshot()
	if len(snap) != 1 || snap[0].Path != "/rooms/:id" {
		t.Fatalf("snapshot = %#v", snap)
	}
}
