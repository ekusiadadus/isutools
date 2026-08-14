// Package echov3 adapts the pre-v4 Echo import path used by ISUCON7,
// ISUCON8 qualifier, and ISUCON10 qualifier.
package echov3

import (
	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/labstack/echo"
)

func Middleware() echo.MiddlewareFunc { return MiddlewareEnabled(!isutools.Off()) }

func Install(e *echo.Echo) {
	if e == nil || isutools.Off() {
		return
	}
	e.Pre(NotFoundMiddlewareEnabled(true))
	e.Use(MiddlewareEnabled(true))
}

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

func Scenario(label string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			sessionlabel.SetScenario(c.Request(), label)
			return next(c)
		}
	}
}
