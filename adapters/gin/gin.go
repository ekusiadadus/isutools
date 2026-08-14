// Package ginadapter passes Gin's registered route template to httpstats and
// provides a thin scenario-label helper without duplicating flow-label logic.
package ginadapter

import (
	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/gin-gonic/gin"
)

// Install adds route-template instrumentation when isutools is enabled.
func Install(engine *gin.Engine) {
	if engine == nil || isutools.Off() {
		return
	}
	engine.Use(MiddlewareEnabled(true))
}

// Middleware resolves the immutable global mode when Gin is wired.
func Middleware() gin.HandlerFunc { return MiddlewareEnabled(!isutools.Off()) }

// MiddlewareEnabled records only Gin's native route pattern. Raw request paths
// and parameter values are never used as metric identities.
func MiddlewareEnabled(enabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if !enabled {
			return
		}
		if path := c.FullPath(); path != "" {
			httpstats.SetRoutePattern(c.Request, path)
		} else {
			httpstats.SetRouteNotFound(c.Request)
		}
	}
}

// Scenario assigns one explicit non-secret story label to a Gin route/group.
func Scenario(label string) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionlabel.SetScenario(c.Request, label)
		c.Next()
	}
}
