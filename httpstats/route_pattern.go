package httpstats

import (
	"context"
	"net/http"
	"strings"
	"sync"
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
	if state, ok := request.Context().Value(routePatternContextKey{}).(*routePatternState); ok {
		state.set(pattern)
	}
	return true
}

// SetRouteNotFound installs a constant identity for routers that report no
// registered template after routing. It prevents a secret slug in URL.Path
// from becoming a 404 aggregate key.
func SetRouteNotFound(request *http.Request) bool {
	return SetRoutePattern(request, RouteNotFound)
}

type routePatternContextKey struct{}

type routePatternState struct {
	mu      sync.RWMutex
	pattern string
}

func (s *routePatternState) set(pattern string) {
	s.mu.Lock()
	s.pattern = pattern
	s.mu.Unlock()
}

func (s *routePatternState) get() string {
	s.mu.RLock()
	pattern := s.pattern
	s.mu.RUnlock()
	return pattern
}

func withRoutePatternState(request *http.Request) *http.Request {
	if request == nil {
		return nil
	}
	if _, ok := request.Context().Value(routePatternContextKey{}).(*routePatternState); ok {
		return request
	}
	state := &routePatternState{pattern: request.Pattern}
	return request.WithContext(context.WithValue(request.Context(), routePatternContextKey{}, state))
}

func routePattern(request *http.Request) string {
	if request == nil {
		return ""
	}
	if state, ok := request.Context().Value(routePatternContextKey{}).(*routePatternState); ok {
		if pattern := state.get(); pattern != "" {
			return pattern
		}
	}
	return request.Pattern
}
