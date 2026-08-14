package echov4

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/labstack/echo/v4"
)

func TestEchoRoutesUseOneNativePatternWithoutSecrets(t *testing.T) {
	collector := httpstats.New()
	e := echo.New()
	e.HideBanner = true
	e.Pre(NotFoundMiddlewareEnabled(true))
	e.Use(MiddlewareEnabled(true))
	group := e.Group("/api")
	route := group.GET("/users/:id", func(c echo.Context) error {
		return c.String(http.StatusCreated, "ok")
	})
	route.Name = "user.show"
	e.GET("/assets/*", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
	handler := collector.Middleware(e)
	for _, target := range []string{
		"/api/users/alice@example.invalid?token=secret-one",
		"/api/users/bob@example.invalid?token=secret-two",
		"/assets/private/file.js",
		"/not-found/private-secret",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer top-secret")
		req.AddCookie(&http.Cookie{Name: "session", Value: "cookie-secret"})
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
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
	if !reflect.DeepEqual(paths, want) || entriesByPath(entries, "/api/users/:id") != 2 {
		t.Fatalf("paths=%v entries=%#v", paths, entries)
	}
	for _, secret := range []string{"alice", "bob", "token", "top-secret", "cookie-secret", "private-secret"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("snapshot leaked %q: %s", secret, encoded)
		}
	}
}

func TestEchoMiddlewareErrorAndPanicMatchHTTPContract(t *testing.T) {
	collector := httpstats.New()
	e := echo.New()
	e.Use(MiddlewareEnabled(true))
	e.GET("/error", func(echo.Context) error { return echo.NewHTTPError(http.StatusTeapot) })
	e.GET("/panic", func(echo.Context) error { panic("private panic value") })
	handler := collector.Middleware(e)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/error", nil))
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("error status=%d", recorder.Code)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was not propagated")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	entries := collector.Snapshot()
	if !hasStatus(entries, "/error", http.StatusTeapot) || !hasStatus(entries, "/panic", http.StatusInternalServerError) {
		t.Fatalf("entries=%#v", entries)
	}
}

func TestEchoMiddlewareDisabledReturnsOriginalHandler(t *testing.T) {
	next := func(echo.Context) error { return errors.New("sentinel") }
	got := MiddlewareEnabled(false)(next)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(next).Pointer() {
		t.Fatal("disabled adapter added a request-path wrapper")
	}
}

func TestEchoScenarioMiddlewareSetsTrustedScenario(t *testing.T) {
	t.Setenv(sessionlabel.EnvFlowLabels, "on")
	t.Setenv(sessionlabel.EnvSourceCookie, "SESSIONID")
	t.Setenv(sessionlabel.EnvHMACKey, strings.Repeat("e", sessionlabel.MinKeyBytes))
	e := echo.New()
	e.Use(Scenario("viewer"))
	e.GET("/users/:id", func(c echo.Context) error { return c.NoContent(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/users/private-id", nil)
	req.AddCookie(&http.Cookie{Name: "SESSIONID", Value: "raw-cookie"})
	rec := httptest.NewRecorder()
	isutools.HTTP(e).ServeHTTP(rec, req)
	if got := rec.Header().Get(sessionlabel.ScenarioHeaderName); got != "viewer" {
		t.Fatalf("scenario = %q", got)
	}
}

func entriesByPath(entries httpstats.Snapshot, path string) int64 {
	for _, entry := range entries {
		if entry.Path == path {
			return entry.Count
		}
	}
	return 0
}

func hasStatus(entries httpstats.Snapshot, path string, status int) bool {
	for _, entry := range entries {
		if entry.Path == path && entry.Status == status {
			return true
		}
	}
	return false
}
