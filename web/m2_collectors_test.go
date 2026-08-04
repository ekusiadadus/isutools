package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/counters"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/procstats"
)

type fakeAccessLog struct {
	current    accesslog.Snapshot
	resets     int
	collects   int
	collectErr error
}

type contextAccessLog struct {
	contextCalled bool
	legacyCalled  bool
	deadline      bool
}

func (f *contextAccessLog) Snapshot() accesslog.Snapshot { return accesslog.Snapshot{} }
func (f *contextAccessLog) Reset() error                 { return nil }
func (f *contextAccessLog) Collect() error {
	f.legacyCalled = true
	return nil
}
func (f *contextAccessLog) CollectContext(ctx context.Context) error {
	f.contextCalled = true
	_, f.deadline = ctx.Deadline()
	return nil
}

func (f *fakeAccessLog) Snapshot() accesslog.Snapshot { return f.current }
func (f *fakeAccessLog) Collect() error {
	f.collects++
	return f.collectErr
}
func (f *fakeAccessLog) Reset() error {
	f.resets++
	f.current = accesslog.Snapshot{}
	return nil
}

func TestCollectEndpointCollectsAccessLogAndReportsFailure(t *testing.T) {
	collector := &fakeAccessLog{}
	h := NewHandler(Provider{AccessLog: collector})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if rec.Code != http.StatusNoContent || collector.collects != 1 {
		t.Fatalf("collect status=%d calls=%d", rec.Code, collector.collects)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/collect", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET collect status=%d, want 405", rec.Code)
	}

	collector.collectErr = errors.New("log unavailable")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed collect status=%d, want 503", rec.Code)
	}
}

func TestSuccessfulCollectClearsRuntimeDegradation(t *testing.T) {
	registry := health.NewRegistry()
	collector := &fakeAccessLog{collectErr: errors.New("temporary failure")}
	h := NewHandler(Provider{AccessLog: collector, Health: registry})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	collector.collectErr = nil
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("recovery collect status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	decodeJSON(t, rec, &payload)
	for _, entry := range payload.Meta.Health {
		if entry.Collector == "accesslog" && entry.Status != health.StatusOK {
			t.Fatalf("recovered accesslog health is sticky: %#v", entry)
		}
	}
}

func TestCollectEndpointUsesContextAwareCollector(t *testing.T) {
	collector := &contextAccessLog{}
	h := NewHandler(Provider{AccessLog: collector, CollectTimeout: time.Second})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if rec.Code != http.StatusNoContent || !collector.contextCalled || collector.legacyCalled || !collector.deadline {
		t.Fatalf("collect context used=%v legacy=%v deadline=%v status=%d",
			collector.contextCalled, collector.legacyCalled, collector.deadline, rec.Code)
	}
}

type fakeProc struct {
	current procstats.Snapshot
	resets  int
}

func (f *fakeProc) Snapshot() procstats.Snapshot { return f.current }
func (f *fakeProc) Reset() error {
	f.resets++
	f.current = procstats.Snapshot{}
	return nil
}

func TestSnapshotIncludesM2Collectors(t *testing.T) {
	httpCollector := httpstats.New()
	instrumented := httpCollector.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	instrumented.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/42?secret=x", nil))

	accessCollector := &fakeAccessLog{current: accesslog.Snapshot{
		Lines:   1,
		Entries: []accesslog.Entry{{Method: http.MethodGet, URI: "/items/42", Count: 1}},
	}}
	procCollector := &fakeProc{current: procstats.Snapshot{
		CPUs:   2,
		TopCPU: []procstats.Process{{PID: 42, Command: "app", CPUPercent: 100}},
	}}
	h := NewHandler(Provider{HTTP: httpCollector, AccessLog: accessCollector, Proc: procCollector})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	decodeJSON(t, rec, &payload)
	if len(payload.HTTP) != 1 || payload.HTTP[0].Path != "/items/*" {
		t.Fatalf("http = %#v", payload.HTTP)
	}
	if payload.AccessLog == nil || payload.AccessLog.Lines != 1 {
		t.Fatalf("accesslog = %#v", payload.AccessLog)
	}
	if payload.Proc == nil || len(payload.Proc.TopCPU) != 1 {
		t.Fatalf("proc = %#v", payload.Proc)
	}
}

func TestDashboardRendersM2SectionsAndHealth(t *testing.T) {
	registry := health.NewRegistry()
	registry.Set("accesslog", health.StatusDegraded, "one malformed line")
	h := NewHandler(Provider{
		Health:    registry,
		HTTP:      httpstats.New(),
		AccessLog: &fakeAccessLog{},
		Proc:      &fakeProc{},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/live", nil))
	for _, want := range []string{
		"<h2>Collector Health</h2>",
		"<h2>HTTP</h2>",
		"<h2>Proxy Access Log</h2>",
		"<h2>Processes</h2>",
		"one malformed line",
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

func TestRuntimeCollectorHealthOverridesStartupOK(t *testing.T) {
	registry := health.NewRegistry()
	registry.Set("accesslog", health.StatusOK, "")
	h := NewHandler(Provider{
		Health: registry,
		AccessLog: &fakeAccessLog{current: accesslog.Snapshot{Health: accesslog.Health{
			Status: accesslog.StatusError, LastError: "read failed", Dropped: 2,
		}}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	decodeJSON(t, rec, &payload)
	if !payload.Meta.Partial {
		t.Fatal("runtime accesslog error must mark partial")
	}
	found := false
	for _, entry := range payload.Meta.Health {
		if entry.Collector == "accesslog" {
			found = entry.Status == health.StatusFailed && entry.Dropped == 2 && entry.Message == "read failed"
		}
	}
	if !found {
		t.Fatalf("health = %#v", payload.Meta.Health)
	}
}

func TestCounterAndFlowOverflowMarkSnapshotPartial(t *testing.T) {
	registry := counters.NewRegistry()
	for i := 0; i < counters.DefaultMaxNames+1; i++ {
		registry.Add(fmt.Sprintf("counter-%d", i), 1)
	}
	h := NewHandler(Provider{
		Counters:  registry,
		AccessLog: &fakeAccessLog{current: accesslog.Snapshot{FlowDropped: 2}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	decodeJSON(t, rec, &payload)
	if !payload.Meta.Partial {
		t.Fatal("counter/flow overflow must mark the snapshot partial")
	}
	seen := map[string]bool{}
	for _, entry := range payload.Meta.Health {
		if entry.Status == health.StatusDegraded {
			seen[entry.Collector] = true
		}
	}
	if !seen["counters"] || !seen["accesslog"] {
		t.Fatalf("overflow health = %#v", payload.Meta.Health)
	}
}

func TestAccessLogOverflowHealthPreservesCollectorDiagnostics(t *testing.T) {
	h := NewHandler(Provider{AccessLog: &fakeAccessLog{current: accesslog.Snapshot{
		Health:       accesslog.Health{Status: accesslog.StatusPartial, Message: "one malformed line", Dropped: 2},
		StoryDropped: 3,
	}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	decodeJSON(t, rec, &payload)
	for _, entry := range payload.Meta.Health {
		if entry.Collector != "accesslog" {
			continue
		}
		if entry.Status != health.StatusDegraded || entry.Dropped != 5 ||
			!strings.Contains(entry.Message, "one malformed line") ||
			!strings.Contains(entry.Message, "scenario-story limit exceeded") {
			t.Fatalf("merged accesslog health = %#v", entry)
		}
		return
	}
	t.Fatal("accesslog health entry is missing")
}

func TestResetStoresCompletedM2GenerationInPrev(t *testing.T) {
	httpCollector := httpstats.New()
	httpCollector.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/old", nil))
	accessCollector := &fakeAccessLog{current: accesslog.Snapshot{Lines: 3}}
	procCollector := &fakeProc{current: procstats.Snapshot{CPUs: 4}}
	h := NewHandler(Provider{HTTP: httpCollector, AccessLog: accessCollector, Proc: procCollector})

	reset := httptest.NewRecorder()
	h.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if reset.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", reset.Code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	decodeJSON(t, rec, &payload)
	if payload.Prev == nil || len(payload.Prev.HTTP) != 1 || payload.Prev.HTTP[0].Path != "/old" {
		t.Fatalf("prev http = %#v", payload.Prev)
	}
	if payload.Prev.AccessLog == nil || payload.Prev.AccessLog.Lines != 3 {
		t.Fatalf("prev accesslog = %#v", payload.Prev.AccessLog)
	}
	if payload.Prev.Proc == nil || payload.Prev.Proc.CPUs != 4 {
		t.Fatalf("prev proc = %#v", payload.Prev.Proc)
	}
	if accessCollector.resets != 1 || procCollector.resets != 1 {
		t.Fatalf("resets: access=%d proc=%d", accessCollector.resets, procCollector.resets)
	}
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
