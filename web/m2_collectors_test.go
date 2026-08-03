package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/accesslog"
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
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, want := range []string{
		"<h2>Collector Health</h2>",
		"<h2>HTTP</h2>",
		"<h2>nginx Access Log</h2>",
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
