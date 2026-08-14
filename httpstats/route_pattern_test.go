package httpstats

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetRoutePatternAcceptsOnlyBoundedFrameworkTemplates(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/users/private-value?token=secret", nil)
	if !SetRoutePattern(request, "/users/:id") || request.Pattern != "/users/:id" {
		t.Fatalf("pattern = %q", request.Pattern)
	}
	for _, unsafe := range []string{"users/:id", "/users/private value", "/users/:id?token=:token", "/" + strings.Repeat("x", MaxProfileRouteBytes)} {
		before := request.Pattern
		if SetRoutePattern(request, unsafe) || request.Pattern != before {
			t.Fatalf("accepted unsafe pattern %q as %q", unsafe, request.Pattern)
		}
	}
}
