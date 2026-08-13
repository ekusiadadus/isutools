package echov5

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/labstack/echo/v5"
)

func TestEchoV5NativePatternsAndSafeNotFound(t *testing.T) {
	collector := httpstats.New()
	e := echo.New()
	e.Pre(NotFoundMiddlewareEnabled(true))
	e.Use(MiddlewareEnabled(true))
	e.Group("/api").GET("/users/:id", func(c *echo.Context) error { return c.NoContent(http.StatusCreated) })
	e.GET("/assets/*", func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) })
	handler := collector.Middleware(e)
	for _, target := range []string{
		"/api/users/alice@example.invalid?token=secret-one",
		"/api/users/bob@example.invalid?token=secret-two",
		"/assets/private/file.js",
		"/not-found/private-secret",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	}
	entries := collector.Snapshot()
	paths := make([]string, 0, len(entries))
	encoded := ""
	for _, entry := range entries {
		paths = append(paths, entry.Path)
		encoded += entry.Key + entry.Path
	}
	sort.Strings(paths)
	want := []string{"/api/users/:id", "/assets/*", "/{not-found}"}
	if !reflect.DeepEqual(paths, want) || countFor(entries, "/api/users/:id") != 2 {
		t.Fatalf("paths=%v entries=%#v", paths, entries)
	}
	for _, secret := range []string{"alice", "bob", "token", "private-secret"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("snapshot leaked %q", secret)
		}
	}
}

func countFor(entries httpstats.Snapshot, path string) int64 {
	for _, entry := range entries {
		if entry.Path == path {
			return entry.Count
		}
	}
	return 0
}
