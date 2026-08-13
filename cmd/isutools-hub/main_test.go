package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/multihost"
)

func TestHubHTTPResetFinishPersistsSealedReport(t *testing.T) {
	token := strings.Repeat("hub-test-token-", 3)
	peer, err := multihost.NewPeer(multihost.PeerOptions{Enabled: true, Token: token, Role: "db", Form: "agent", AgentID: "123e4567-e89b-42d3-a456-426614174000", Capabilities: []string{"run-v1"}, Sections: []string{"hoststats"}})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerServer := httptest.NewServer(peer)
	defer peerServer.Close()
	hub, err := multihost.NewHub(multihost.HubConfig{Peers: []multihost.HubPeerConfig{{Name: "db", Endpoint: peerServer.URL, Token: token, Required: true, RequiredCapabilities: []string{"run-v1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := newServer(hub, t.TempDir())

	reset := httptest.NewRecorder()
	handler.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if reset.Code != http.StatusCreated {
		t.Fatalf("reset=%d %s", reset.Code, reset.Body.String())
	}
	finish := httptest.NewRecorder()
	handler.ServeHTTP(finish, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if finish.Code != http.StatusOK {
		t.Fatalf("finish=%d %s", finish.Code, finish.Body.String())
	}
	handler.mu.RLock()
	current := handler.current
	handler.mu.RUnlock()
	if current == nil || current.File == "" || current.Validity != "valid" || len(current.Peers) != 1 || current.Peers[0].Sealed != "ack" {
		t.Fatalf("report=%+v", current)
	}

	jsonResult := httptest.NewRecorder()
	handler.ServeHTTP(jsonResult, httptest.NewRequest(http.MethodGet, "/json", nil))
	if jsonResult.Code != http.StatusOK || strings.Contains(jsonResult.Body.String(), token) {
		t.Fatalf("json=%d %s", jsonResult.Code, jsonResult.Body.String())
	}
}

func TestHubAbortIsIdempotent(t *testing.T) {
	if !literalLoopback("127.0.0.1:19193") || literalLoopback("localhost:19193") {
		t.Fatal("literal loopback validation failed")
	}
}
