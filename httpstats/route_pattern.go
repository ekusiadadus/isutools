package httpstats

import (
	"net/http"
	"strings"
)

const RouteNotFound = "/{not-found}"

// SetRoutePattern is the framework-neutral route adapter contract. Framework
// middleware calls it after routing with the registered template, never the
// expanded URL. The outer Collector.Middleware reads the same request after
// the handler returns.
func SetRoutePattern(request *http.Request, pattern string) bool {
	if request == nil {
		return false
	}
	pattern = strings.TrimSpace(pattern)
	if !safeConstantRoute(pattern) || !strings.HasPrefix(pattern, "/") {
		return false
	}
	request.Pattern = pattern
	return true
}

// SetRouteNotFound installs a constant identity for routers that report no
// registered template after routing. It prevents a secret slug in URL.Path
// from becoming a 404 aggregate key.
func SetRouteNotFound(request *http.Request) bool {
	return SetRoutePattern(request, RouteNotFound)
}
