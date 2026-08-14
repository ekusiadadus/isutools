package ginadapter

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/gin-gonic/gin"
)

func TestGinNativePatternsDoNotLeakParameters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	collector := httpstats.New()
	r := gin.New()
	Install(r)
	r.GET("/users/:id", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/assets/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	handler := collector.Middleware(r)
	for _, target := range []string{"/users/alice@example.invalid", "/users/bob@example.invalid", "/assets/private/file.js", "/not-found/private"} {
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
	want := []string{"/assets/*path", "/users/:id", "/{not-found}"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths=%v entries=%#v", paths, entries)
	}
	for _, secret := range []string{"alice", "bob", "private"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("route metrics leaked %q", secret)
		}
	}
}

func TestGinScenarioMiddlewareSetsTrustedScenario(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(sessionlabel.EnvFlowLabels, "on")
	t.Setenv(sessionlabel.EnvSourceCookie, "SESSIONID")
	t.Setenv(sessionlabel.EnvHMACKey, strings.Repeat("g", sessionlabel.MinKeyBytes))
	r := gin.New()
	r.Use(Scenario("viewer"))
	r.GET("/users/:id", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodGet, "/users/private-id", nil)
	req.AddCookie(&http.Cookie{Name: "SESSIONID", Value: "raw-cookie"})
	rec := httptest.NewRecorder()
	isutools.HTTP(r).ServeHTTP(rec, req)
	if got := rec.Header().Get(sessionlabel.ScenarioHeaderName); got != "viewer" {
		t.Fatalf("scenario = %q", got)
	}
}

func TestGinCloseNotifierSurvivesIsutoolsMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/stream", func(c *gin.Context) {
		if got := c.Writer.CloseNotify(); got == nil {
			t.Error("Gin received a nil close notification channel")
		}
		c.Status(http.StatusNoContent)
	})
	rec := &closeNotifyRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		closed:           make(chan bool),
	}
	isutools.HTTP(r).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream", nil))
}

type closeNotifyRecorder struct {
	*httptest.ResponseRecorder
	closed chan bool
}

func (r *closeNotifyRecorder) CloseNotify() <-chan bool { return r.closed }
