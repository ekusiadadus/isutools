package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/dbinspect"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
)

func newTestHandler(t *testing.T) (http.Handler, *agg.Table) {
	t.Helper()
	tbl := agg.NewTable(agg.DefaultMaxKeys)
	tbl.Observe("SELECT big", 100*time.Millisecond)
	tbl.Observe("SELECT small", 1*time.Millisecond)
	return NewHandler(Provider{SQL: tbl}), tbl
}

func TestApplyProtocolAdvicePrefersClientFacingAccessLog(t *testing.T) {
	snap := Snapshot{
		Advisor: []advisor.Check{{ID: "http3-server", Status: advisor.StatusOK}},
		HTTP:    httpstats.Snapshot{{Protocol: "HTTP/1.1", Status: 200, Count: 100, P95: time.Millisecond}},
		AccessLog: &accesslog.Snapshot{Protocols: []accesslog.ProtocolEntry{
			{Protocol: "HTTP/3.0", Count: 80, Status5xx: 1, RequestP95: 20 * time.Millisecond},
			{Protocol: "HTTP/2.0", Count: 20, RequestP95: 30 * time.Millisecond},
		}},
	}
	applyProtocolAdvice(&snap)
	var traffic advisor.Check
	for _, check := range snap.Advisor {
		if check.ID == "http3-traffic" {
			traffic = check
		}
	}
	if traffic.Status != advisor.StatusOK {
		t.Fatalf("traffic = %#v", traffic)
	}
	if !strings.Contains(traffic.Detail, "source=proxy access log") || !strings.Contains(traffic.Detail, "HTTP/3.0=80") {
		t.Errorf("traffic detail = %q", traffic.Detail)
	}
}

func TestApplyProtocolAdviceDoesNotTreatOriginLogAsClientEvidence(t *testing.T) {
	snap := Snapshot{AccessLog: &accesslog.Snapshot{Protocols: []accesslog.ProtocolEntry{
		{Protocol: "HTTP/3.0", Count: 100, RequestP95: time.Millisecond},
	}}}
	applyProtocolAdviceWithSource(&snap, false)
	if len(snap.Advisor) != 1 || snap.Advisor[0].Status != advisor.StatusInfo {
		t.Errorf("advisor = %#v", snap.Advisor)
	}
}

func TestApplyProtocolAdviceFallsBackToApplicationProtocol(t *testing.T) {
	snap := Snapshot{HTTP: httpstats.Snapshot{{Protocol: "HTTP/2.0", Status: 503, Count: 5, P95: 10 * time.Millisecond}}}
	applyProtocolAdvice(&snap)
	if len(snap.Advisor) != 1 || !strings.Contains(snap.Advisor[0].Detail, "source=application middleware") {
		t.Errorf("advisor = %#v", snap.Advisor)
	}
}

func TestApplyQUICTelemetryReplacesStaticSkip(t *testing.T) {
	snap := Snapshot{Advisor: advisor.WithQUICTelemetry(nil, nil, nil)}
	applyQUICTelemetry(&snap, &advisor.QUICTelemetry{
		PacketsSent: 100, PacketsRetransmitted: 10, UDPDatagramsDropped: 1,
	}, nil)
	if len(snap.Advisor) != 1 || snap.Advisor[0].Status != advisor.StatusWarn {
		t.Errorf("advisor = %#v", snap.Advisor)
	}
}

func TestApplyCacheTelemetryReplacesStaticSkip(t *testing.T) {
	snap := Snapshot{Advisor: advisor.WithCacheTelemetry(nil, nil, nil)}
	applyCacheTelemetry(&snap, &advisor.CacheTelemetry{
		Hits: 900, Misses: 100, Evictions: 3,
	}, nil)
	if len(snap.Advisor) != 1 || snap.Advisor[0].Status != advisor.StatusWarn {
		t.Errorf("advisor = %#v", snap.Advisor)
	}
}

func TestScenarioStoriesRendered(t *testing.T) {
	h := NewHandler(Provider{AccessLog: &fakeAccessLog{current: accesslog.Snapshot{
		Stories: []accesslog.StoryEntry{{
			Scenario: "login_and_browse", Journey: []string{"POST /login", "GET /"}, Sessions: 3, Requests: 6,
		}},
	}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/snapshot.html", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "login_and_browse") || !strings.Contains(rec.Body.String(), "POST /login") {
		t.Errorf("scenario story not rendered: status=%d body=%s", rec.Code, rec.Body.String())
	}
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
	if got.Meta.SchemaVersion != 3 {
		t.Errorf("schema_version = %d, want 3 (v3 added health/generations)", got.Meta.SchemaVersion)
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
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/live", nil))
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

func newPersistentHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	tbl := agg.NewTable(agg.DefaultMaxKeys)
	tbl.Observe("SELECT persisted", 5*time.Millisecond)
	h := NewHandler(Provider{
		SQL:     tbl,
		DataDir: dir,
		DB: func(context.Context) *dbinspect.Schema {
			return &dbinspect.Schema{
				Flavor: "mysql",
				Tables: []dbinspect.Table{{
					Name: "comments", Engine: "InnoDB", Rows: 100000,
					Indexes: []dbinspect.Index{{Name: "PRIMARY", Columns: "id", Unique: true}},
				}},
			}
		},
	})
	return h, dir
}

func TestLiveShowsDBSchemaAndSections(t *testing.T) {
	h, _ := newPersistentHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/live", nil))
	body := rec.Body.String()
	for _, want := range []string{"DB Schema", "comments", "PRIMARY", "SQL"} {
		if !strings.Contains(body, want) {
			t.Errorf("live report missing %q", want)
		}
	}
}

func TestIndexIsRunList(t *testing.T) {
	h, _ := newPersistentHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{"Runs", `href="live"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	if strings.Contains(body, "DB Schema") {
		t.Error("index must be a run list, not the full report")
	}
}

func TestSavePersistsAndDashboardLists(t *testing.T) {
	h, dir := newPersistentHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save?score=929", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(saved.File, "score929") {
		t.Errorf("file = %q, want score in name", saved.File)
	}
	htmlBytes, err := os.ReadFile(filepath.Join(dir, saved.File))
	if err != nil {
		t.Fatalf("saved html missing: %v", err)
	}
	if !strings.Contains(string(htmlBytes), "score 929") {
		t.Error("stored snapshot must always show the score in its header")
	}
	jsonBytes, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(saved.File, ".html")+".json"))
	if err != nil {
		t.Fatalf("saved json missing: %v", err)
	}
	if !strings.Contains(string(jsonBytes), `"score": "929"`) {
		t.Errorf("stored json meta must carry the score: %s", jsonBytes[:200])
	}

	runID := strings.SplitN(saved.File, "_", 2)[0]

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), runID) {
		t.Error("index must list the saved run by its timestamp id")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/"+runID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("run detail status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "SELECT persisted") {
		t.Error("run detail must serve the stored snapshot content")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+saved.File, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("files status = %d", rec.Code)
	}
}

func TestTwoImmediateSavesHaveDistinctRunIDs(t *testing.T) {
	h, dir := newPersistentHandler(t)
	files := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save?score=1", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("save %d status = %d: %s", i, rec.Code, rec.Body.String())
		}
		var saved struct {
			File string `json:"file"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
			t.Fatal(err)
		}
		files = append(files, saved.File)
	}
	if files[0] == files[1] {
		t.Fatalf("same-second saves collided: %q", files[0])
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("saved files = %d, want two html/json pairs", len(entries))
	}
}

type blockingAccessLog struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingAccessLog) Collect() error {
	close(b.started)
	<-b.release
	return nil
}
func (*blockingAccessLog) Snapshot() accesslog.Snapshot { return accesslog.Snapshot{} }
func (*blockingAccessLog) Reset() error                 { return nil }

func TestConcurrentMutationFailsFastInsteadOfQueueing(t *testing.T) {
	collector := &blockingAccessLog{started: make(chan struct{}), release: make(chan struct{})}
	dir := t.TempDir()
	h := NewHandler(Provider{AccessLog: collector, DataDir: dir})
	collectDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
		collectDone <- rec.Code
	}()
	<-collector.started

	saveDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save", nil))
		saveDone <- rec.Code
	}()
	var code int
	blocked := false
	select {
	case code = <-saveDone:
	case <-time.After(100 * time.Millisecond):
		blocked = true
	}
	close(collector.release)
	if blocked {
		code = <-saveDone
	}
	<-collectDone
	if blocked || code != http.StatusConflict {
		t.Fatalf("concurrent save blocked=%v status=%d, want immediate 409", blocked, code)
	}
}

func TestSaveRejectsOversizedSnapshot(t *testing.T) {
	tbl := agg.NewTable(agg.DefaultMaxKeys)
	tbl.Observe(strings.Repeat("x", 33<<20), time.Millisecond)
	h := NewHandler(Provider{SQL: tbl, DataDir: t.TempDir()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save", nil))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized save status = %d, want 413", rec.Code)
	}
}

func TestSaveBoundsScoreFilenameComponent(t *testing.T) {
	h, _ := newPersistentHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save?score="+strings.Repeat("9", 1000), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("long score save status = %d: %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.File) > 240 {
		t.Fatalf("saved filename is unbounded: %d bytes", len(saved.File))
	}
}

func TestDBAndAdvisorInspectionReceiveDeadlines(t *testing.T) {
	var dbDeadline, advisorDeadline bool
	h := NewHandler(Provider{
		DB: func(ctx context.Context) *dbinspect.Schema {
			_, dbDeadline = ctx.Deadline()
			return &dbinspect.Schema{}
		},
		Advisor: func(ctx context.Context) []advisor.Check {
			_, advisorDeadline = ctx.Deadline()
			return nil
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if !dbDeadline || !advisorDeadline {
		t.Fatalf("inspection deadlines: db=%v advisor=%v", dbDeadline, advisorDeadline)
	}
}

func TestRunDetailUnknownIDIs404(t *testing.T) {
	h, _ := newPersistentHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/20990101-000000", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown run id status = %d, want 404", rec.Code)
	}
}

func TestSaveWithoutDataDirFails(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when no data dir", rec.Code)
	}
}

func TestFilesRejectsTraversal(t *testing.T) {
	h, _ := newPersistentHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/..%2Fsecret.html", nil))
	if rec.Code == http.StatusOK {
		t.Error("path traversal must be rejected")
	}
}

func TestMetaTimeIsJST(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var got struct {
		Meta struct {
			Time string `json:"time"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasSuffix(got.Meta.Time, "+09:00") {
		t.Errorf("meta.time = %q, must always be JST (+09:00)", got.Meta.Time)
	}
}
