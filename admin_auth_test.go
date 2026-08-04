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

func TestProtectAdminIsSSHOnlyAndHasNoTokenMode(t *testing.T) {
	next := &authTestHandler{}
	loopback, err := protectAdmin("127.0.0.1:19191", false, next)
	if err != nil || loopback == nil {
		t.Fatalf("loopback protect = (%T, %v), want handler", loopback, err)
	}
	if _, err := protectAdmin("0.0.0.0:19191", false, next); err == nil {
		t.Fatal("non-loopback bind without explicit SSH/container opt-in must fail closed")
	}
	if got, err := protectAdmin("0.0.0.0:19191", true, next); err != nil || got == nil {
		t.Fatalf("explicit SSH/container bind = (%T, %v), want handler", got, err)
	}

	// The old Bearer/query-token layer is intentionally gone. When reachability
	// is allowed, requests pass through without application-level auth.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/json?token=ignored", nil)
	req.Host = "localhost:19191"
	loopback.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("SSH-only handler unexpectedly used token auth: status=%d cookies=%v", rec.Code, rec.Result().Cookies())
	}
}

func TestSSHOnlyAdminRejectsCrossSiteBrowserRequests(t *testing.T) {
	protected, err := protectAdmin("127.0.0.1:19191", false, &authTestHandler{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		host       string
		origin     string
		fetchSite  string
		wantStatus int
	}{
		{name: "curl", host: "127.0.0.1:19191", wantStatus: http.StatusNoContent},
		{name: "same origin", host: "localhost:19191", origin: "http://localhost:19191", fetchSite: "same-origin", wantStatus: http.StatusNoContent},
		{name: "foreign host", host: "evil.example", wantStatus: http.StatusForbidden},
		{name: "foreign origin", host: "localhost:19191", origin: "https://evil.example", wantStatus: http.StatusForbidden},
		{name: "cross site fetch", host: "localhost:19191", fetchSite: "cross-site", wantStatus: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/reset", nil)
			req.Host = tc.host
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			protected.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

type authTestHandler struct{}

func (*authTestHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}
