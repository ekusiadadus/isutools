package multihost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

const testToken = "0123456789abcdef0123456789abcdef"

type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *fakeClock) Add(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }

func peerRequest(t *testing.T, p *Peer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func startPeerRun(t *testing.T, p *Peer, runID, nonce string) StartResultDTO {
	t.Helper()
	body := fmt.Sprintf(`{"run_id":%q,"nonce":%q,"preempt":false}`, runID, nonce)
	rec := peerRequest(t, p, http.MethodPost, "/peer/runs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start=%d %s", rec.Code, rec.Body.String())
	}
	var dto StartResultDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	return dto
}

func TestPeerProtocolLifecycleAndImmutableSnapshot(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)}
	p, err := NewPeer(PeerOptions{Enabled: true, Token: testToken, Role: "app", Form: "embedded", AgentID: "123e4567-e89b-42d3-a456-426614174000", Sections: []string{"httpstats"}, Capabilities: []string{"run-v1"}, Now: clock.Now, Snapshot: func(_ *runctl.Snapshot) map[string]any {
		return map[string]any{"httpstats": map[string]any{"count": 2}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	start := startPeerRun(t, p, "run-a", "nonce-a")
	if start.State != "started" || start.LocalRunID == "" || start.LeaseExpiresAt.IsZero() {
		t.Fatalf("start=%#v", start)
	}
	finish := peerRequest(t, p, http.MethodPost, "/peer/runs/run-a/finish", "{}")
	if finish.Code != http.StatusOK {
		t.Fatalf("finish=%d %s", finish.Code, finish.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for {
		status := peerRequest(t, p, http.MethodGet, "/peer/runs/run-a", "")
		var dto RunStatusDTO
		_ = json.Unmarshal(status.Body.Bytes(), &dto)
		if dto.SnapshotReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status=%s", status.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	snapshot := peerRequest(t, p, http.MethodGet, "/peer/runs/run-a/snapshot?max_bytes=1048576", "")
	if snapshot.Code != http.StatusOK {
		t.Fatalf("snapshot=%d %s", snapshot.Code, snapshot.Body.String())
	}
	var local LocalSnapshot
	if err := json.Unmarshal(snapshot.Body.Bytes(), &local); err != nil {
		t.Fatal(err)
	}
	if local.RunID != "run-a" || local.LocalRunID != start.LocalRunID || len(local.Sections) != 1 || local.Budget.EncodedBytes <= 0 {
		t.Fatalf("snapshot=%#v", local)
	}
	ack := peerRequest(t, p, http.MethodPost, "/peer/runs/run-a/ack", "{}")
	if ack.Code != http.StatusNoContent {
		t.Fatalf("ack=%d %s", ack.Code, ack.Body.String())
	}
	_ = startPeerRun(t, p, "run-b", "nonce-b")
}

func TestPeerAuthStrictDecodeAndReplayFencing(t *testing.T) {
	p, err := NewPeer(PeerOptions{Enabled: true, Token: testToken, Role: "app", Form: "embedded", AgentID: "123e4567-e89b-42d3-a456-426614174000"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	req := httptest.NewRequest(http.MethodGet, "/peer/info", nil)
	req.Header.Set("Authorization", "Bearer attacker-secret")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "attacker") {
		t.Fatalf("unauthorized=%d %s", rec.Code, rec.Body.String())
	}
	bad := peerRequest(t, p, http.MethodPost, "/peer/runs", `{"run_id":"r","nonce":"n","preempt":false,"unknown":"secret"}`)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "malformed_request") || strings.Contains(bad.Body.String(), "secret") {
		t.Fatalf("strict=%d %s", bad.Code, bad.Body.String())
	}
	first := startPeerRun(t, p, "run-a", "nonce-a")
	replay := peerRequest(t, p, http.MethodPost, "/peer/runs", `{"run_id":"run-a","nonce":"nonce-a","preempt":false}`)
	var second StartResultDTO
	_ = json.Unmarshal(replay.Body.Bytes(), &second)
	if replay.Code != http.StatusOK || second.LocalRunID != first.LocalRunID {
		t.Fatalf("replay=%d %#v", replay.Code, second)
	}
	mismatch := peerRequest(t, p, http.MethodPost, "/peer/runs", `{"run_id":"run-b","nonce":"nonce-a","preempt":true}`)
	if mismatch.Code != http.StatusConflict || !strings.Contains(mismatch.Body.String(), "nonce_mismatch") {
		t.Fatalf("mismatch=%d %s", mismatch.Code, mismatch.Body.String())
	}
}

func TestPeerStartedLeaseExpiresAndUnblocksNextRun(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)}
	p, err := NewPeer(PeerOptions{Enabled: true, Token: testToken, Role: "db", Form: "agent", AgentID: "123e4567-e89b-42d3-a456-426614174000", Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	startPeerRun(t, p, "run-a", "nonce-a")
	clock.Add(PeerStartedLease + time.Second)
	p.Sweep()
	status := peerRequest(t, p, http.MethodGet, "/peer/runs/run-a", "")
	var dto RunStatusDTO
	_ = json.Unmarshal(status.Body.Bytes(), &dto)
	if dto.State != "aborted" || dto.ExpiryReason != "started-lease-expired" {
		t.Fatalf("status=%#v", dto)
	}
	startPeerRun(t, p, "run-b", "nonce-b")
}

func TestPeerBudgetsAndMemoryStayBounded(t *testing.T) {
	p, err := NewPeer(PeerOptions{Enabled: true, Token: testToken, Role: "agent", Form: "agent", AgentID: "123e4567-e89b-42d3-a456-426614174000"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("r-%d", i)
		nonce := fmt.Sprintf("n-%d", i)
		startPeerRun(t, p, id, nonce)
		rec := peerRequest(t, p, http.MethodPost, "/peer/runs/"+id+"/abort", `{"reason":"test"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("abort %d=%d %s", i, rec.Code, rec.Body.String())
		}
	}
	runs, nonces := p.DebugCounts()
	if runs > RetainedRuns || nonces > NonceHistoryMax {
		t.Fatalf("runs=%d nonces=%d", runs, nonces)
	}
	local := &LocalSnapshot{SchemaVersion: 1, RunID: "r", LocalRunID: "l", Epoch: 1, Validity: "valid", Sections: map[string]json.RawMessage{"huge": json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)}}
	bounded := boundSnapshot(local, 32<<10)
	body, _ := json.Marshal(bounded)
	if len(body) > 32<<10 || len(bounded.Sections) != 0 || len(bounded.Budget.DroppedSections) == 0 {
		t.Fatalf("bytes=%d bounded=%#v", len(body), bounded.Budget)
	}
}
