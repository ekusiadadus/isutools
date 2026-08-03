package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
)

func newTestHandler(t *testing.T) (http.Handler, *agg.Table) {
	t.Helper()
	tbl := agg.NewTable(agg.DefaultMaxKeys)
	tbl.Observe("SELECT big", 100*time.Millisecond)
	tbl.Observe("SELECT small", 1*time.Millisecond)
	return NewHandler(Provider{SQL: tbl}), tbl
}

func TestJSONSortedByTotal(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Meta struct {
			SchemaVersion int    `json:"schema_version"`
			Generation    int64  `json:"generation"`
			Revision      string `json:"revision"`
			Host          struct {
				CPUModel      string `json:"cpu_model"`
				NumCPU        int    `json:"num_cpu"`
				MemTotalBytes uint64 `json:"mem_total_bytes"`
			} `json:"host"`
		} `json:"meta"`
		SQL []struct {
			Key   string `json:"key"`
			Count int64  `json:"count"`
		} `json:"sql"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if len(got.SQL) != 2 {
		t.Fatalf("sql entries = %d, want 2", len(got.SQL))
	}
	if got.SQL[0].Key != "SELECT big" {
		t.Errorf("first entry = %q, want the biggest total first", got.SQL[0].Key)
	}
	if got.Meta.Revision == "" {
		t.Error("meta.revision must always be present")
	}
	if got.Meta.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", got.Meta.SchemaVersion)
	}
	if got.Meta.Generation < 1 {
		t.Errorf("generation = %d, want >= 1", got.Meta.Generation)
	}
	if got.Meta.Host.NumCPU <= 0 {
		t.Errorf("host.num_cpu = %d, want > 0", got.Meta.Host.NumCPU)
	}
	if got.Meta.Host.CPUModel == "" {
		t.Error("host.cpu_model must always be present")
	}
}

func TestReportShowsHostInfo(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "cores") {
		t.Error("report must always show host CPU/memory info")
	}
}

func TestLiveReportSorted(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	big := strings.Index(body, "SELECT big")
	small := strings.Index(body, "SELECT small")
	if big < 0 || small < 0 {
		t.Fatalf("rows missing in body:\n%s", body)
	}
	if big > small {
		t.Error("rows must be pre-sorted by total desc (big before small)")
	}
}

func TestSnapshotHTMLIsSelfContainedAndSortable(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/snapshot.html", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<script>") {
		t.Error("snapshot.html must embed the sort script")
	}
	for _, forbidden := range []string{"http://", "https://", "src=", "href="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("snapshot.html must be self-contained, found %q", forbidden)
		}
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("snapshot.html should download as a file, Content-Disposition = %q", cd)
	}
}

func TestResetKeepsPreviousGeneration(t *testing.T) {
	h, tbl := newTestHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}
	if entries := tbl.Snapshot(); len(entries) != 0 {
		t.Fatalf("table not cleared: %v", entries)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var got struct {
		SQL  []json.RawMessage `json:"sql"`
		Prev *struct {
			SQL []json.RawMessage `json:"sql"`
		} `json:"prev"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.SQL) != 0 {
		t.Errorf("current sql should be empty after reset")
	}
	if got.Prev == nil || len(got.Prev.SQL) != 2 {
		t.Errorf("prev generation must be kept after reset: %+v", got.Prev)
	}
}

func TestResetRequiresPOST(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/reset", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /reset status = %d, want 405", rec.Code)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
