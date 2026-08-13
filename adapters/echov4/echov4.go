// Package echov4 passes Echo's registered route template to httpstats.
package echov4

import (
	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/labstack/echo/v4"
)

// Middleware resolves the immutable global mode once, when Echo is wired.
func Middleware() echo.MiddlewareFunc { return MiddlewareEnabled(!isutools.Off()) }

// Install wires both the pre-routing safe 404 fallback and the post-routing
// native template adapter. Applications should call this once before routes
// are served.
func Install(e *echo.Echo) {
	if e == nil || isutools.Off() {
		return
	}
	e.Pre(NotFoundMiddlewareEnabled(true))
	e.Use(MiddlewareEnabled(true))
}

// NotFoundMiddlewareEnabled sets a safe default before Echo routing. Matched
// requests overwrite it with their native route template in Middleware.
func NotFoundMiddlewareEnabled(enabled bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if !enabled {
			return next
		}
		return func(c echo.Context) error {
			httpstats.SetRouteNotFound(c.Request())
			return next(c)
		}
	}
}

// MiddlewareEnabled is useful when an application owns its enablement config.
// Disabled returns the original handler and adds no request-path wrapper.
func MiddlewareEnabled(enabled bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if !enabled {
			return next
		}
		return func(c echo.Context) error {
			if path := c.Path(); path != "" {
				httpstats.SetRoutePattern(c.Request(), path)
			}
			return next(c)
		}
	}
}
