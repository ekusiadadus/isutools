// Package echov5 passes Echo v5's registered route template to httpstats.
package echov5

import (
	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/labstack/echo/v5"
)

// Middleware resolves the immutable global mode once, when Echo is wired.
func Middleware() echo.MiddlewareFunc { return MiddlewareEnabled(!isutools.Off()) }

// Install wires a safe pre-routing 404 fallback and the post-routing native
// route template adapter. Applications should call it before serving routes.
func Install(e *echo.Echo) {
	if e == nil || isutools.Off() {
		return
	}
	e.Pre(NotFoundMiddlewareEnabled(true))
	e.Use(MiddlewareEnabled(true))
}

// Scenario assigns one explicit non-secret story label to an Echo route or
// group. isutools.HTTP performs the trusted response-header emission.
func Scenario(label string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			sessionlabel.SetScenario(c.Request(), label)
			return next(c)
		}
	}
}

func NotFoundMiddlewareEnabled(enabled bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if !enabled {
			return next
		}
		return func(c *echo.Context) error {
			httpstats.SetRouteNotFound(c.Request())
			return next(c)
		}
	}
}

// MiddlewareEnabled uses only Echo's trusted route template. It never falls
// back to the raw request path, which could contain identifiers or secrets.
func MiddlewareEnabled(enabled bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if !enabled {
			return next
		}
		return func(c *echo.Context) error {
			if path := c.Path(); path != "" {
				httpstats.SetRoutePattern(c.Request(), path)
			}
			return next(c)
		}
	}
}
