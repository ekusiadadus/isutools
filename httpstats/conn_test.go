package httpstats

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebSocketUpgradeExcludedFromLatencyTable(t *testing.T) {
	c := New()
	var activeDuring int64
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeDuring = c.Connections().Active
		w.WriteHeader(http.StatusSwitchingProtocols)
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
