// Package martini provides registration-time route adapters for the framework
// used by ISUCON4. Martini exposes params but not the immutable matched route,
// so the registered constant is passed explicitly and raw paths are ignored.
package martini

import (
	"net/http"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/sessionlabel"
	framework "github.com/go-martini/martini"
)

// Route returns Martini middleware suitable for m.Get(pattern, Route(pattern), handler).
func Route(pattern string) framework.Handler {
	return func(r *http.Request) { httpstats.SetRoutePattern(r, pattern) }
}

// Scenario returns Martini middleware assigning one bounded story label.
func Scenario(label string) framework.Handler {
	return func(r *http.Request) { sessionlabel.SetScenario(r, label) }
}
