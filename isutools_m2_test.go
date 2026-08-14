package isutools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/sqlstats"
	"github.com/ekusiadadus/isutools/web"
)

func TestHTTPFacadeUsesDefaultCollector(t *testing.T) {
	httpstats.Default.Reset()
	t.Cleanup(func() { httpstats.Default.Reset() })
	h := HTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/users/42?token=secret", nil))
	got := httpstats.Default.Snapshot()
	if len(got) != 1 || got[0].Path != "/users/*" || got[0].Status != http.StatusCreated {
		t.Fatalf("http snapshot = %#v", got)
	}
}

func TestHTTPFacadeIsZeroOverheadWhenOff(t *testing.T) {
	setHardOffForTest(t)
	next := &fixedHandler{}
	if got := HTTP(next); got != next {
		t.Fatal("HTTP must return the original handler when ISUTOOLS=off")
	}
}

type fixedHandler struct{}

func (*fixedHandler) ServeHTTP(http.ResponseWriter, *http.Request) {}

func TestHandlerUsesAtomicSQLGenerationAndHTTP(t *testing.T) {
	h := Handler()
	// The stores are run-generation managed, so /reset owns the rotation: a
	// raw Store.Reset here would advance the store past the coordinator's
	// epoch and the reset below would then publish the stale generation. Open
	// a run through the handler instead — it also clears the process-wide prev
	// that an earlier test's saved run leaves behind.
	postReset(t, h)
	t.Cleanup(func() { postReset(t, h) })

	sqlstats.Default.Observe("SELECT old", time.Millisecond)
	HTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/old", nil))

	reset := httptest.NewRecorder()
	h.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if reset.Code != http.StatusNoContent {
		t.Fatalf("reset = %d: %s", reset.Code, reset.Body.String())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload struct {
		web.Snapshot
		Prev *web.Snapshot `json:"prev"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Meta.Generation != sqlstats.Default.CurrentGeneration() {
		t.Fatalf("generation = %d, store = %d", payload.Meta.Generation, sqlstats.Default.CurrentGeneration())
	}
	if payload.Prev == nil || len(payload.Prev.SQL) != 1 || payload.Prev.SQL[0].Key != "SELECT old" {
		t.Fatalf("prev SQL = %#v", payload.Prev)
	}
	if len(payload.Prev.HTTP) != 1 || payload.Prev.HTTP[0].Path != "/old" {
		t.Fatalf("prev HTTP = %#v", payload.Prev.HTTP)
	}
}

func TestHandlerConfiguresNginxLogFromEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.ltsv")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ISUTOOLS_NGINX_LOG", path)
	h := Handler()
	line := "method:GET\turi:/posts/1\tstatus:200\treqtime:0.125\tupstime:0.100\tbytes:42\tcache:-\tctype:text/html\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload web.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.AccessLog == nil || payload.AccessLog.Lines != 1 {
		t.Fatalf("accesslog = %#v", payload.AccessLog)
	}
}

func TestSQLRegistrationFailureIsVisibleInSnapshotHealth(t *testing.T) {
	t.Cleanup(func() { collectorHealth.Set("sql", health.StatusOK, "") })
	if got := SQLDriverName("mysql-health-test-not-registered"); got != "mysql-health-test-not-registered" {
		t.Fatalf("driver = %q", got)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload web.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !payload.Meta.Partial {
		t.Fatal("registration failure must mark snapshot partial")
	}
	found := false
	for _, entry := range payload.Meta.Health {
		if entry.Collector == "sql" && entry.Status == "failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sql failure missing from health: %#v", payload.Meta.Health)
	}
}
