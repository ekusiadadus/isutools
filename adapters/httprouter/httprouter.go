// Package httprouter provides registration-time route adapters for the router
// used by ISUCON8 final. httprouter does not expose the matched template on
// http.Request, so the immutable registration pattern is captured explicitly.
package httprouter

import (
	"net/http"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	router "github.com/julienschmidt/httprouter"
)

// Handle wraps a native handler with its registered, unexpanded pattern.
func Handle(pattern string, next router.Handle) router.Handle {
	return func(w http.ResponseWriter, r *http.Request, params router.Params) {
		httpstats.SetRoutePattern(r, pattern)
		next(w, r, params)
	}
}

// Scenario assigns a story label to one native handler.
func Scenario(label string, next router.Handle) router.Handle {
	return func(w http.ResponseWriter, r *http.Request, params router.Params) {
		sessionlabel.SetScenario(r, label)
		next(w, r, params)
	}
}
