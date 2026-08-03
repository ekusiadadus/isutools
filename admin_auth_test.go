package isutools

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopbackAdminAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:19191", "[::1]:19191", "localhost:19191"} {
		if !isLoopbackAdminAddr(addr) {
			t.Errorf("%q must be loopback", addr)
		}
	}
	for _, addr := range []string{":19191", "0.0.0.0:19191", "192.0.2.1:19191", "example.com:19191", "bad"} {
		if isLoopbackAdminAddr(addr) {
			t.Errorf("%q must not be treated as loopback", addr)
		}
	}
}

func TestProtectAdminKeepsLoopbackTokenFreeAndRequiresExplicitContainerOptIn(t *testing.T) {
	next := &authTestHandler{}
	if got, err := protectAdmin("127.0.0.1:19191", "", false, next); err != nil || got != next {
		t.Fatalf("loopback protect = (%T, %v), want unchanged", got, err)
	}
	// Docker commonly binds 0.0.0.0 inside the container while compose
	// publishes only host 127.0.0.1. Require an explicit security opt-in.
	if _, err := protectAdmin("0.0.0.0:19191", "", false, next); err == nil {
		t.Fatal("tokenless non-loopback bind without explicit opt-in must fail closed")
	}
	if got, err := protectAdmin("0.0.0.0:19191", "", true, next); err != nil || got != next {
		t.Fatalf("explicit tokenless container bind = (%T, %v), want unchanged", got, err)
	}

	protected, err := protectAdmin("0.0.0.0:19191", "correct horse", false, next)
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	for _, tc := range []struct {
		authorization string
		want          int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{"Basic YTpi", http.StatusUnauthorized},
		{"Bearer correct horse", http.StatusNoContent},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/json", nil)
		req.Header.Set("Authorization", tc.authorization)
		protected.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("Authorization %q: status = %d, want %d", tc.authorization, rec.Code, tc.want)
		}
		if tc.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("401 response must advertise Bearer authentication")
		}
	}
}

func TestProtectAdminAcceptsQueryTokenAndSetsCookie(t *testing.T) {
	protected, err := protectAdmin("0.0.0.0:19191", "correct horse", false, &authTestHandler{})
	if err != nil {
		t.Fatalf("protect: %v", err)
	}

	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?token=correct+horse", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("query token status = %d, want 204", rec.Code)
	}
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 || cookie[0].Value == "" || !cookie[0].HttpOnly {
		t.Fatalf("valid query token must set an HttpOnly session cookie, got %v", cookie)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	req.AddCookie(cookie[0])
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("cookie auth status = %d, want 204", rec.Code)
	}

	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?token=wrong", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong query token status = %d, want 401", rec.Code)
	}
}

type authTestHandler struct{}

func (*authTestHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
