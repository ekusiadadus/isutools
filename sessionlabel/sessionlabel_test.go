package sessionlabel

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestTrustedSessionLabelsAreStableDistinctAndSafe(t *testing.T) {
	a := New("isucon_session", []byte(strings.Repeat("k", MinKeyBytes)))
	one, ok := a.Label("raw-session-one@example.invalid")
	if !ok {
		t.Fatal("label disabled")
	}
	again, _ := a.Label("raw-session-one@example.invalid")
	two, _ := a.Label("raw-session-two@example.invalid")
	if one != again || one == two {
		t.Fatalf("one=%q again=%q two=%q", one, again, two)
	}
	if len(one) != 24 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(one) {
		t.Fatalf("unsafe label %q", one)
	}
}

func TestMiddlewareOverwritesSpoofedClientHeader(t *testing.T) {
	a := New("session", []byte(strings.Repeat("s", MinKeyBytes)))
	var requestHeader string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHeader = r.Header.Get(HeaderName)
		w.Header().Set(HeaderName, "malicious-app-overwrite")
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, "attacker-label")
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw-source-secret"})
	rec := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rec, req)
	if requestHeader != "" {
		t.Fatalf("downstream saw spoofed header %q", requestHeader)
	}
	got := rec.Header().Get(HeaderName)
	if got == "" || got == "attacker-label" || got == "malicious-app-overwrite" || strings.Contains(got, "raw-source") {
		t.Fatalf("trusted response label = %q", got)
	}
}

func TestMissingKeyFailsClosedWithBoundedHealth(t *testing.T) {
	a := New("session", []byte("raw-secret-too-short"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, "attacker-label")
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw-cookie-secret"})
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderName) != "" {
			t.Error("spoofed header survived")
		}
	})).ServeHTTP(rec, req)
	if rec.Header().Get(HeaderName) != "" || a.Health() != (Health{Reason: "key-invalid"}) {
		t.Fatalf("header=%q health=%#v", rec.Header().Get(HeaderName), a.Health())
	}
}

func TestOversizedSourceFailsClosed(t *testing.T) {
	a := New("session", []byte(strings.Repeat("k", MinKeyBytes)))
	if label, ok := a.Label(strings.Repeat("x", MaxSourceBytes+1)); ok || label != "" {
		t.Fatalf("oversized label=%q ok=%v", label, ok)
	}
}
