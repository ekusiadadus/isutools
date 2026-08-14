package echov3

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/labstack/echo"
)

func TestMiddlewareUsesEchoPath(t *testing.T) {
	e := echo.New()
	e.Pre(NotFoundMiddlewareEnabled(true))
	e.Use(MiddlewareEnabled(true))
	e.GET("/users/:id", func(c echo.Context) error { return c.NoContent(204) })
	httpstats.Default.Reset()
	httpstats.Middleware(e).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/users/secret", nil))
	snap := httpstats.Default.Snapshot()
	if len(snap) != 1 || snap[0].Path != "/users/:id" {
		t.Fatalf("snapshot = %#v", snap)
	}
}
