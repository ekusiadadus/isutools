package martini

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
	framework "github.com/go-martini/martini"
)

func TestRouteUsesExplicitPattern(t *testing.T) {
	m := framework.Classic()
	m.Get("/users/:id", Route("/users/:id"), func() (int, string) { return 204, "" })
	httpstats.Default.Reset()
	httpstats.Middleware(m).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/users/secret", nil))
	snap := httpstats.Default.Snapshot()
	if len(snap) != 1 || snap[0].Path != "/users/:id" {
		t.Fatalf("snapshot = %#v", snap)
	}
}
