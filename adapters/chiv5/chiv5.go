// Package chiv5 passes chi v5's registered route template to httpstats and
// exposes the framework-neutral scenario middleware with chi's native type.
package chiv5

import (
	"net/http"

	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/go-chi/chi/v5"
)

// Install adds route-template instrumentation when isutools is enabled.
func Install(router chi.Router) {
	if router == nil || isutools.Off() {
		return
	}
	router.Use(MiddlewareEnabled(true))
}

// Middleware resolves the immutable global mode when chi is wired.
func Middleware(next http.Handler) http.Handler {
	return MiddlewareEnabled(!isutools.Off())(next)
}

// MiddlewareEnabled reads RoutePattern after the downstream handler, as chi's
// routing context is complete only then.
func MiddlewareEnabled(enabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if !enabled {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
				httpstats.SetRoutePattern(r, pattern)
			} else {
				httpstats.SetRouteNotFound(r)
			}
		})
	}
}

// Scenario assigns one explicit non-secret story label to a chi route/group.
func Scenario(label string) func(http.Handler) http.Handler {
	return sessionlabel.Scenario(label)
}
