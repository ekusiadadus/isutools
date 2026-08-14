// Package gojiv2 provides registration-time route adapters for goji.io,
// which was used by ISUCON6 final. Old Goji does not publish a stable matched
// template API, so callers pass the same constant used in mux.Handle.
package gojiv2

import (
	"net/http"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
)

func Route(pattern string, next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpstats.SetRoutePattern(r, pattern)
		next.ServeHTTP(w, r)
	})
}

func Scenario(label string) func(http.Handler) http.Handler { return sessionlabel.Scenario(label) }
