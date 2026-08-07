package httpstats

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSafeProfileLabelNeverFallsBackToRequestPath(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodPost, "/reset-password/alice@example.com", nil)
	label := SafeProfileLabel(request)
	if label.Method != http.MethodPost || label.Route != ProfileRouteUnmatched {
		t.Fatalf("label = %#v, want POST unmatched", label)
	}
	request = httptest.NewRequest(http.MethodGet, "/invite/secret-slug", nil)
	if got := SafeProfileLabel(request).Route; got != ProfileRouteUnmatched {
		t.Fatalf("secret slug leaked as route %q", got)
	}
}

func TestSafeProfileLabelUsesOnlyRouterPatternAndMethodAllowlist(t *testing.T) {
	t.Parallel()

	request := httptest.NewRequest(http.MethodGet, "/users/alice@example.com", nil)
	request.Pattern = "GET /users/{id}"
	if got := SafeProfileLabel(request); got.Method != http.MethodGet || got.Route != "/users/{id}" {
		t.Fatalf("pattern label = %#v", got)
	}
	request.Method = "BREW"
	if got := SafeProfileLabel(request).Method; got != ProfileMethodOther {
		t.Fatalf("unknown method = %q, want %q", got, ProfileMethodOther)
	}
}

func TestSafeProfileLabelRejectsOversizedOrControlPattern(t *testing.T) {
	t.Parallel()

	for _, pattern := range []string{"GET /bad\nsecret", "GET /" + string(make([]byte, 129))} {
		request := httptest.NewRequest(http.MethodGet, "/safe", nil)
		request.Pattern = pattern
		if got := SafeProfileLabel(request).Route; got != ProfileRouteUnmatched {
			t.Errorf("pattern %q produced %q", pattern, got)
		}
	}
}

func TestParseSafeProfileRouteRulesRequiresAnchoredConstantRules(t *testing.T) {
	rules, err := ParseSafeProfileRouteRules(`^/invite/[^/]+$=/invite/{token};^/users/[0-9]+$=/users/{id}`)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/invite/top-secret", nil)
	if got := SafeProfileLabelWithRules(request, rules); got.Route != "/invite/{token}" {
		t.Fatalf("safe rule label = %#v", got)
	}
	request = httptest.NewRequest(http.MethodGet, "/invite/top-secret/extra", nil)
	if got := SafeProfileLabelWithRules(request, rules); got.Route != ProfileRouteUnmatched {
		t.Fatalf("partial safe rule matched: %#v", got)
	}

	invalid := []string{
		`/invite/.*=/invite/{token}`,
		`^/invite/.*$=/invite/$1`,
		`^/invite/.*$=` + strings.Repeat("x", MaxProfileRouteBytes+1),
		strings.Repeat("x", MaxSafeRouteRulesSpecBytes+1),
	}
	for _, spec := range invalid {
		if _, err := ParseSafeProfileRouteRules(spec); err == nil {
			t.Fatalf("ParseSafeProfileRouteRules(%q) succeeded", spec)
		}
	}
}

func TestProfileLabelMiddlewareFailsOpenAndUsesSafeValue(t *testing.T) {
	labeler := &recordingProfileLabeler{}
	rules, err := ParseSafeProfileRouteRules(`^/secret/[^/]+$=/secret/{value}`)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	handler := ProfileLabelMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), labeler, rules)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/secret/alice@example.com?token=no", nil))
	if !called || labeler.calls != 1 || labeler.label.Method != http.MethodPost || labeler.label.Route != "/secret/{value}" {
		t.Fatalf("called=%v labeler=%#v", called, labeler)
	}
	if strings.Contains(labeler.label.Route, "alice") || strings.Contains(labeler.label.Route, "token") {
		t.Fatalf("secret leaked into label: %#v", labeler.label)
	}
}

type recordingProfileLabeler struct {
	calls int
	label ProfileLabel
}

func (l *recordingProfileLabeler) DoProfileLabels(ctx context.Context, label ProfileLabel, fn func(context.Context)) bool {
	l.calls++
	l.label = label
	fn(ctx)
	return true
}
