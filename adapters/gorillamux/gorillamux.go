// Package gorillamux adapts Gorilla mux route templates used by ISUCON3,
// ISUCON5, ISUCON6 qualifier, and ISUCON7 final.
package gorillamux

import (
	"net/http"

	"github.com/ekusiadadus/isutools"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	"github.com/gorilla/mux"
)

// Install adds route-template instrumentation when isutools is enabled.
func Install(router *mux.Router) {
	if router == nil || isutools.Off() {
		return
	}
	router.Use(MiddlewareEnabled(true))
}

// Middleware resolves the immutable global mode when the router is wired.
func Middleware(next http.Handler) http.Handler {
	return MiddlewareEnabled(!isutools.Off())(next)
}

// MiddlewareEnabled observes Gorilla's registered template after routing.
func MiddlewareEnabled(enabled bool) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		if !enabled {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			if route := mux.CurrentRoute(r); route != nil {
				if pattern, err := route.GetPathTemplate(); err == nil && pattern != "" {
					httpstats.SetRoutePattern(r, pattern)
					return
				}
			}
			httpstats.SetRouteNotFound(r)
		})
	}
}

// Scenario assigns one bounded, non-secret story label.
func Scenario(label string) mux.MiddlewareFunc { return sessionlabel.Scenario(label) }
