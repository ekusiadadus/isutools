package httpstats

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebSocketUpgradeExcludedFromLatencyTable(t *testing.T) {
	c := New()
	var activeDuring int64
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
		activeDuring = c.Connections().Active
	}))

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if activeDuring != 1 {
		t.Errorf("active during handler = %d, want 1", activeDuring)
	}
	if entries := c.Snapshot(); len(entries) != 0 {
		t.Errorf("latency table must not contain the upgrade: %v", entries)
	}
	conns := c.Connections()
	if conns.Total != 1 || conns.Active != 0 {
		t.Errorf("conns = %+v, want total 1 active 0", conns)
	}
}

func TestSSEExcludedFromLatencyTable(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/events", nil))

	if entries := c.Snapshot(); len(entries) != 0 {
		t.Errorf("latency table must not contain SSE: %v", entries)
	}
	if conns := c.Connections(); conns.Total != 1 {
		t.Errorf("conns = %+v, want total 1", conns)
	}
}

func TestSSEResetDoesNotWaitForOpenStream(t *testing.T) {
	c := New()
	started := make(chan struct{})
	release := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-release
	}))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/events", nil))
		close(done)
	}()
	<-started
	active := c.Connections().Active
	resetDone := make(chan Snapshot, 1)
	go func() { resetDone <- c.Reset() }()
	resetReturned := false
	select {
	case <-resetDone:
		resetReturned = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-done
	if !resetReturned {
		<-resetDone
	}
	if active != 1 {
		t.Errorf("active SSE connections = %d, want 1", active)
	}
	if !resetReturned {
		t.Error("Reset waited for an open SSE connection")
	}
}

func TestRejectedWebSocketUpgradeIsNormalHTTP(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upgrade rejected", http.StatusBadRequest)
	}))
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.Header.Set("Upgrade", "websocket")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := c.Connections(); got.Total != 0 || got.Active != 0 {
		t.Errorf("rejected upgrade counted as connection: %+v", got)
	}
	entries := c.Snapshot()
	if len(entries) != 1 || entries[0].Status != http.StatusBadRequest {
		t.Errorf("rejected upgrade missing from latency table: %+v", entries)
	}
}

func TestNormalRequestStaysInLatencyTable(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if entries := c.Snapshot(); len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	if conns := c.Connections(); conns.Total != 0 {
		t.Errorf("conns = %+v, want 0", conns)
	}
}

func TestConnTotalsResetWithGeneration(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/events", nil))
	c.Reset()
	if conns := c.Connections(); conns.Total != 0 {
		t.Errorf("after reset conns = %+v, want total 0", conns)
	}
}

func TestConnectionSnapshotIncludesDurationP95AndWireBytes(t *testing.T) {
	c := New()
	c.connStart()
	c.connFinish(20*time.Minute, 123, 456)
	snap := c.Connections()
	if snap.P95Seconds != 1200 || snap.BytesRead != 123 || snap.BytesWritten != 456 {
		t.Fatalf("connection snapshot = %+v", snap)
	}
}

type pipeHijackWriter struct {
	plainWriter
	server net.Conn
}

func (w *pipeHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.server, bufio.NewReadWriter(bufio.NewReader(w.server), bufio.NewWriter(w.server)), nil
}

func TestHijackedConnectionIsTrackedUntilClose(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	writer := &pipeHijackWriter{plainWriter: plainWriter{header: make(http.Header)}, server: server}
	peerDone := make(chan error, 1)
	go func() {
		out := make([]byte, 3)
		if _, err := io.ReadFull(client, out); err != nil {
			peerDone <- err
			return
		}
		_, err := client.Write([]byte("in"))
		peerDone <- err
	}()

	c := New()
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte("out")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}))
	h.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	if entries := c.Snapshot(); len(entries) != 0 {
		t.Fatalf("hijacked request entered latency table: %+v", entries)
	}
	snap := c.Connections()
	if snap.Total != 1 || snap.Active != 0 || snap.BytesRead != 2 || snap.BytesWritten != 3 {
		t.Fatalf("connection snapshot = %+v", snap)
	}
}

func TestParseRules(t *testing.T) {
	rules, err := ParseRules(`^/@[^/]+$=/@*;^/posts/[0-9]+$=/posts/*`)
	if err != nil {
		t.Fatalf("ParseRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(rules))
	}
	c := New()
	c.SetRules(rules)
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/@morgan", nil))
	entries := c.Snapshot()
	if len(entries) != 1 || entries[0].Key == "" {
		t.Fatalf("entries = %v", entries)
	}
	if got := entries[0].Key; !contains(got, "/@*") {
		t.Errorf("key = %q, want normalized /@*", got)
	}

	if _, err := ParseRules("no-separator"); err == nil {
		t.Error("invalid spec must error")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && (stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
