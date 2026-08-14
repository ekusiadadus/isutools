package isutools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeerEnabledIsExplicitOptIn(t *testing.T) {
	for _, raw := range []string{"", "0", "false", "off", "unexpected"} {
		if peerEnabled(raw) {
			t.Fatalf("peerEnabled(%q) = true, want false", raw)
		}
	}
	for _, raw := range []string{"1", " true ", "YES", "on", "enabled"} {
		if !peerEnabled(raw) {
			t.Fatalf("peerEnabled(%q) = false, want true", raw)
		}
	}
}

func TestLiteralLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:19192", "[::1]:19192"} {
		if !literalLoopbackAddr(addr) {
			t.Fatalf("literalLoopbackAddr(%q) = false", addr)
		}
	}
	for _, addr := range []string{"localhost:19192", "0.0.0.0:19192", ":19192", "127.0.0.1"} {
		if literalLoopbackAddr(addr) {
			t.Fatalf("literalLoopbackAddr(%q) = true", addr)
		}
	}
}

func TestPeerHandlerAuthAndNoSecretDisclosure(t *testing.T) {
	t.Setenv(EnvPeer, "on")
	token := strings.Repeat("peer-secret-", 3)
	handler, err := PeerHandler(PeerOptions{Token: token})
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/peer/info", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	if strings.Contains(unauthorized.Body.String(), token) {
		t.Fatal("unauthorized response disclosed peer token")
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/peer/info", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d body=%s", authorized.Code, authorized.Body.String())
	}
	if strings.Contains(authorized.Body.String(), token) {
		t.Fatal("info response disclosed peer token")
	}
}
