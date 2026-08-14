package multihost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

func peerServer(t *testing.T, agentID string, sections []string, wrap func(http.Handler) http.Handler) (*Peer, *httptest.Server) {
	t.Helper()
	peer, err := NewPeer(PeerOptions{Enabled: true, Token: testToken, Role: "agent", Form: "agent", AgentID: agentID, Sections: sections, Capabilities: []string{"run-v1"}, Snapshot: func(snapshot *runctl.Snapshot) map[string]any {
		return map[string]any{"hoststats": map[string]any{"run": snapshot.RunID}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler := http.Handler(peer)
	if wrap != nil {
		handler = wrap(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(func() { server.Close(); peer.Close() })
	return peer, server
}

func TestHubTwoPeerRunUsesCommonWireIdentityAndAssemblesBoth(t *testing.T) {
	_, one := peerServer(t, "123e4567-e89b-42d3-a456-426614174001", []string{"hoststats"}, nil)
	_, two := peerServer(t, "123e4567-e89b-42d3-a456-426614174002", []string{"hoststats"}, nil)
	hub, err := NewHub(HubConfig{Peers: []HubPeerConfig{{Name: "app", Endpoint: one.URL, Token: testToken, Required: true, RequiredSections: []string{"hoststats"}}, {Name: "db", Endpoint: two.URL, Token: testToken, Required: true, RequiredSections: []string{"hoststats"}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := hub.Start(ctx, "run-shared", "nonce-shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Peers) != 2 || run.Peers[0].Start.RunID != "run-shared" || run.Peers[1].Start.RunID != "run-shared" || run.Peers[0].Start.Epoch != run.Peers[1].Start.Epoch {
		t.Fatalf("starts=%#v", run.Peers)
	}
	peers, validity := hub.Finish(ctx, run, 1024)
	if validity != "valid" || len(peers) != 2 {
		t.Fatalf("validity=%s peers=%#v", validity, peers)
	}
	for _, result := range peers {
		if result.Local == nil || result.Local.RunID != "run-shared" || result.Sealed != "ack" || result.StartSendAck[0].IsZero() || result.FinishSendAck[1].IsZero() {
			t.Fatalf("peer=%#v", result)
		}
	}
}

func TestHubThreePeerOptionalFinishFailureIsPartialAndOthersSeal(t *testing.T) {
	_, one := peerServer(t, "123e4567-e89b-42d3-a456-426614174011", []string{"hoststats"}, nil)
	_, two := peerServer(t, "123e4567-e89b-42d3-a456-426614174012", []string{"hoststats"}, nil)
	_, three := peerServer(t, "123e4567-e89b-42d3-a456-426614174013", []string{"hoststats"}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/finish") {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	hub, err := NewHub(HubConfig{Peers: []HubPeerConfig{{Name: "app", Endpoint: one.URL, Token: testToken, Required: true}, {Name: "db", Endpoint: two.URL, Token: testToken, Required: true}, {Name: "proxy", Endpoint: three.URL, Token: testToken, Required: false}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	run, err := hub.Start(ctx, "run-three", "nonce-three")
	if err != nil {
		t.Fatal(err)
	}
	peers, validity := hub.Finish(ctx, run, 1024)
	if validity != "partial" {
		t.Fatalf("validity=%s peers=%#v", validity, peers)
	}
	if peers[2].Failure == nil || peers[2].Failure.Phase != "finish" {
		t.Fatalf("optional failure=%#v", peers[2])
	}
	if peers[0].Sealed != "ack" || peers[1].Sealed != "ack" {
		t.Fatalf("required peers not sealed: %#v", peers)
	}
}

func TestHubRequiredPreflightMismatchFailsBeforeStarting(t *testing.T) {
	peer, server := peerServer(t, "123e4567-e89b-42d3-a456-426614174021", []string{"hoststats"}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/peer/info" {
				_ = json.NewEncoder(w).Encode(PeerInfoDTO{ProtocolVersion: 99, SchemaVersion: 1, AgentID: "123e4567-e89b-42d3-a456-426614174021", Form: "agent", Role: "agent", StartedAt: time.Now()})
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	hub, err := NewHub(HubConfig{Peers: []HubPeerConfig{{Name: "db", Endpoint: server.URL, Token: testToken, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := hub.Start(context.Background(), "run-bad", "nonce-bad")
	if err == nil || run.Validity != "invalid" || run.Peers[0].Failure.Code != "protocol-mismatch" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	runs, _ := peer.DebugCounts()
	if runs != 0 {
		t.Fatalf("peer started despite failed preflight: runs=%d", runs)
	}
}

func TestHubTransportRequiresLiteralLoopbackAndStrictResponses(t *testing.T) {
	if _, err := NewHub(HubConfig{Peers: []HubPeerConfig{{Name: "bad", Endpoint: "http://example.com:19192", Token: testToken, Required: true}}}); err == nil {
		t.Fatal("public peer endpoint accepted")
	}
	_, server := peerServer(t, "123e4567-e89b-42d3-a456-426614174031", nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/peer/info" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"protocol_version":1,"unknown":"secret"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	hub, err := NewHub(HubConfig{Peers: []HubPeerConfig{{Name: "bad", Endpoint: server.URL, Token: testToken, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	results := hub.Preflight(context.Background())
	if len(results) != 1 || results[0].Failure == nil || results[0].Failure.Code != "malformed" {
		t.Fatalf("results=%#v", results)
	}
}
