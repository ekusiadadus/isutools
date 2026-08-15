package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/counters"
	"github.com/ekusiadadus/isutools/dbinspect"
	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/timeline"
	"github.com/ekusiadadus/isutools/netstats"
	"github.com/ekusiadadus/isutools/procstats"
	"github.com/ekusiadadus/isutools/queryplan"
	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
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
	for _, forbidden := range []string{"http://", "https://", "src="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("snapshot.html must be self-contained, found %q", forbidden)
		}
	}
	if !strings.Contains(body, `href="#profiles"`) {
		t.Error("snapshot.html must keep internal navigation usable without JavaScript")
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

type resetCountingHTTP struct{ resets int }

func (c *resetCountingHTTP) Snapshot() httpstats.Snapshot { return nil }
func (c *resetCountingHTTP) Reset() httpstats.Snapshot {
	c.resets++
	return nil
}

type resetCountingCounters struct{ resets int }

func (c *resetCountingCounters) Snapshot() []counters.Entry { return nil }
func (c *resetCountingCounters) Reset()                     { c.resets++ }

func TestCoordinatorManagedResetDoesNotRotateCollectorsTwice(t *testing.T) {
	httpCollector := &resetCountingHTTP{}
	counterCollector := &resetCountingCounters{}
	sqlRotations := 0
	generation := int64(1)
	h := NewHandler(Provider{
		SQL:                  agg.NewTable(agg.DefaultMaxKeys),
		SQLGeneration:        func() int64 { return generation },
		RotateSQL:            func() (int64, []agg.Entry) { sqlRotations++; generation++; return generation, nil },
		HTTP:                 httpCollector,
		Counters:             counterCollector,
		RunGenerationManaged: true,
		StartRun: func(context.Context) (RunStart, error) {
			generation++ // models the coordinator's BeginBoundary.
			return RunStart{RunID: "run-managed-reset", State: "started", Validity: "valid", StartedAt: time.Now()}, nil
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d: %s", rec.Code, rec.Body.String())
	}
	if sqlRotations != 0 || httpCollector.resets != 0 || counterCollector.resets != 0 {
		t.Fatalf("legacy rotations: sql=%d http=%d counters=%d", sqlRotations, httpCollector.resets, counterCollector.resets)
	}
	if generation != 2 {
		t.Fatalf("generation=%d, want exactly one coordinator advance", generation)
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
		File                  string `json:"file"`
		SnapshotBase          string `json:"snapshot_base"`
		SnapshotFile          string `json:"snapshot_file"`
		SnapshotSchemaVersion int    `json:"snapshot_schema_version"`
		RunID                 string `json:"run_id"`
		SnapshotSHA256        string `json:"snapshot_sha256"`
		Visibility            string `json:"visibility"`
		Durability            string `json:"durability"`
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
	wantHash := sha256.Sum256(jsonBytes)
	if saved.SnapshotFile != strings.TrimSuffix(saved.File, ".html")+".json" ||
		saved.SnapshotSHA256 != hex.EncodeToString(wantHash[:]) {
		t.Errorf("save provenance = %#v, want JSON file and exact bytes hash", saved)
	}
	if saved.SnapshotBase != strings.TrimSuffix(saved.File, ".html") || saved.SnapshotSchemaVersion != schemaVersion {
		t.Errorf("save ledger identity = %#v", saved)
	}
	if got := rec.Header().Get("X-Isutools-Snapshot-SHA256"); got != saved.SnapshotSHA256 {
		t.Errorf("snapshot hash header = %q, want %q", got, saved.SnapshotSHA256)
	}
	if saved.Visibility != "visible" || (saved.Durability != "durable" && saved.Durability != "unknown") {
		t.Errorf("save publication state = visibility %q durability %q", saved.Visibility, saved.Durability)
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

// fakeRunSections is the section map a completed run hands the transport.
func fakeRunSections() map[string]any {
	return map[string]any{
		"hoststats": &hoststats.Section{Partial: true, Codes: []string{"not-captured:psi"}},
		"network": &netstats.NetworkStats{
			TCP:        netstats.TCPSummary{InUse: 12, TimeWait: 340},
			Interfaces: []netstats.Interface{{Name: "eth0", RxBytes: 1 << 20}},
		},
		"sqlrows": &sqlrows.Section{
			Limit:   sqlrows.DigestTextFetchLimit,
			Targets: []sqlrows.TargetSection{{TargetID: "app", Usable: true}},
		},
		"dbpool": []dbpool.Entry{{TargetID: "app", Display: "tcp(db:3306)/isuconp", WaitCount: 7}},
	}
}

func TestSnapshotIncludesRunSections(t *testing.T) {
	h := NewHandler(Provider{
		SQL:      agg.NewTable(agg.DefaultMaxKeys),
		Sections: fakeRunSections,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Host == nil || !got.Host.Partial {
		t.Errorf("hoststats section = %+v, want the collector's value", got.Host)
	}
	if got.Network == nil || got.Network.TCP.TimeWait != 340 {
		t.Errorf("network section = %+v, want the collector's value", got.Network)
	}
	if got.SQLRows == nil || len(got.SQLRows.Targets) != 1 {
		t.Errorf("sqlrows section = %+v, want the collector's value", got.SQLRows)
	}
	if len(got.DBPool) != 1 || got.DBPool[0].WaitCount != 7 {
		t.Errorf("dbpool section = %+v, want the collector's value", got.DBPool)
	}
	// The keys are additive: a v1.0 reader must still find the old ones.
	if !strings.Contains(rec.Body.String(), `"sql"`) {
		t.Error("existing snapshot keys must survive the new sections")
	}
}

// TestQueryPlanSectionReachesTheSnapshotAndItsHealth covers the additive
// section end to end: the capture's own output lands under its collector name,
// and the reasons it recorded for the targets it could not explain reach the
// reader instead of being dropped with the section that carried them.
func TestQueryPlanSectionReachesTheSnapshotAndItsHealth(t *testing.T) {
	sections := fakeRunSections()
	sections[queryplan.Name] = &queryplan.Section{
		Top: queryplan.DefaultTop,
		Targets: []queryplan.TargetSection{
			{
				TargetID: "app", Schema: "isuconp", Explained: true,
				Plans: []queryplan.Plan{{
					Digest: "d1", Query: "SELECT plan_marker FROM posts WHERE id = ?",
					Freshness: queryplan.FreshnessFresh, FreshReason: queryplan.FreshInInterval,
					Rows: []queryplan.PlanRow{{Type: stringPtr("ref"), Key: stringPtr("PRIMARY")}},
				}},
			},
			{
				TargetID: "db2",
				Code:     queryplan.CodePurposeUnregistered,
				Reason:   "EXPLAIN 専用 credential が未登録です",
			},
		},
		Health: []queryplan.HealthNote{{
			Key: queryplan.CodePurposeUnregistered, Message: "queryplan[db2]: skip (credential 未登録)",
		}},
	}
	h := NewHandler(Provider{
		SQL:      agg.NewTable(agg.DefaultMaxKeys),
		Sections: func() map[string]any { return sections },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.QueryPlan == nil || len(got.QueryPlan.Targets) != 2 {
		t.Fatalf("queryplan section = %+v, want the capture's own value", got.QueryPlan)
	}
	if len(got.QueryPlan.Targets[0].Plans) != 1 {
		t.Errorf("target plans = %+v, want the captured plan", got.QueryPlan.Targets[0])
	}
	if !strings.Contains(rec.Body.String(), `"queryplan"`) {
		t.Error(`the section must be published under the collector's own key`)
	}
	entry, found := healthEntryFor(got.Meta.Health, queryplan.Name)
	if !found || entry.Status == health.StatusOK {
		t.Fatalf("queryplan health = %+v (found=%v), want the skip note forwarded", entry, found)
	}
	if !strings.Contains(entry.Message, queryplan.CodePurposeUnregistered) {
		t.Errorf("health message = %q, want the reason ID the capture recorded", entry.Message)
	}
	if !got.Meta.Partial {
		t.Error("a degraded section must mark the snapshot partial")
	}
}

// TestQueryPlansReplaceThePlaceholderAdvisorChecks is the other half of the
// wiring: a captured plan has to be judged, and only a plan the run can vouch
// for may be.
func TestQueryPlansReplaceThePlaceholderAdvisorChecks(t *testing.T) {
	rows := []queryplan.PlanRow{{
		Table: stringPtr("comments"), Type: stringPtr("ALL"), Rows: int64Ptr(20000),
		Extra: stringPtr("Using filesort"),
	}}
	tests := []struct {
		name      string
		freshness queryplan.FreshnessState
		reason    queryplan.FreshReason
		want      advisor.Status
	}{
		{
			name: "fresh plan is judged", freshness: queryplan.FreshnessFresh,
			reason: queryplan.FreshInInterval, want: advisor.StatusWarn,
		},
		{
			name: "stale plan is not", freshness: queryplan.FreshnessStale,
			reason: queryplan.FreshBeforeInterval, want: advisor.StatusInfo,
		},
		{
			name: "unjudgeable plan is not", freshness: queryplan.FreshnessUnknown,
			reason: queryplan.FreshClockAnomaly, want: advisor.StatusInfo,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := Snapshot{
				Advisor: []advisor.Check{
					{ID: "plan-full-scan", Title: "実行計画: type=ALL の全表走査", Status: advisor.StatusSkip},
				},
				QueryPlan: &queryplan.Section{Targets: []queryplan.TargetSection{{
					TargetID: "app", Explained: true,
					Plans: []queryplan.Plan{{
						Digest: "d1", Query: "SELECT plan_marker FROM comments WHERE post_id = ?",
						Freshness: tt.freshness, FreshReason: tt.reason, Rows: rows,
					}},
				}}},
			}
			applyQueryPlanAdvice(&snap)
			check, found := advisorCheck(snap.Advisor, "plan-full-scan")
			if !found {
				t.Fatalf("advisor = %+v, want the plan checks", snap.Advisor)
			}
			if check.Status != tt.want {
				t.Errorf("plan-full-scan = %q (%s), want %q", check.Status, check.Detail, tt.want)
			}
			if _, ok := advisorCheck(snap.Advisor, "plan-filesort"); !ok {
				t.Error("every plan check must be present, not only the one that fired")
			}
		})
	}
}

// TestQueryPlanAdviceIsSkippedWithoutACapture keeps a process with the feature
// off exactly as it was: the placeholder checks advisor.Collect emitted stand.
func TestQueryPlanAdviceIsSkippedWithoutACapture(t *testing.T) {
	snap := Snapshot{Advisor: []advisor.Check{{ID: "plan-full-scan", Status: advisor.StatusSkip}}}
	applyQueryPlanAdvice(&snap)
	if len(snap.Advisor) != 1 || snap.Advisor[0].Status != advisor.StatusSkip {
		t.Errorf("advisor = %+v, want the placeholder checks untouched", snap.Advisor)
	}
}

func advisorCheck(checks []advisor.Check, id string) (advisor.Check, bool) {
	for _, check := range checks {
		if check.ID == id {
			return check, true
		}
	}
	return advisor.Check{}, false
}

// TestQueryPlanAdviceCopiesNullableColumns pins the one thing a shared pointer
// would break: the snapshot and the advisor must not be able to observe each
// other's edits, and a NULL column must survive as a NULL.
func TestQueryPlanAdviceCopiesNullableColumns(t *testing.T) {
	key := "PRIMARY"
	section := &queryplan.Section{Targets: []queryplan.TargetSection{{
		TargetID: "app",
		Plans: []queryplan.Plan{{
			Digest: "d1", Freshness: queryplan.FreshnessFresh,
			Rows: []queryplan.PlanRow{{Key: &key, Rows: int64Ptr(3)}},
		}},
	}}}
	plans := advisorQueryPlans(section)
	if len(plans) != 1 || len(plans[0].Rows) != 1 {
		t.Fatalf("plans = %+v, want one row", plans)
	}
	row := plans[0].Rows[0]
	if row.Key == &key {
		t.Error("the advisor row must not alias the section's column")
	}
	if row.Key == nil || *row.Key != key || row.Rows == nil || *row.Rows != 3 {
		t.Errorf("row = %+v, want the same values", row)
	}
	if row.Type != nil || row.Extra != nil {
		t.Errorf("row = %+v, want NULL columns to stay NULL", row)
	}

	// A digest with no plan still reaches the advisor, carrying the closed
	// class that says why: "nothing was judged" and "nothing went wrong" are
	// different reports.
	failed := advisorQueryPlans(&queryplan.Section{Targets: []queryplan.TargetSection{{
		TargetID: "app",
		Plans: []queryplan.Plan{{
			Digest: "d2", Freshness: queryplan.FreshnessFresh,
			Err: &queryplan.PlanError{Class: queryplan.PlanErrPermission, Errno: 1142},
		}},
	}}})
	if len(failed) != 1 || failed[0].ErrClass != string(queryplan.PlanErrPermission) || failed[0].Rows != nil {
		t.Errorf("failed plan = %+v, want the error class and no rows", failed)
	}
}

// TestQueryPlanSectionMessagesTolerateAnAbsentSection keeps the health
// forwarding from depending on a section that a coordinator never produced.
func TestQueryPlanSectionMessagesTolerateAnAbsentSection(t *testing.T) {
	if got := queryPlanSectionMessages(nil); got != nil {
		t.Errorf("messages = %v, want none for an absent section", got)
	}
	if got := queryPlanSectionMessages(&queryplan.Section{}); len(got) != 0 {
		t.Errorf("messages = %v, want none for a section with no notes", got)
	}
}

func TestSnapshotOmitsAbsentRunSections(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	for _, key := range []string{"hoststats", "network", "sqlrows", "dbpool"} {
		if strings.Contains(rec.Body.String(), `"`+key+`"`) {
			t.Errorf("key %q must be omitted until a run produced it", key)
		}
	}
}

func TestApplyRunSectionsSkipsUnusableEntries(t *testing.T) {
	tests := []struct {
		name     string
		sections map[string]any
	}{
		{name: "nil map"},
		{name: "unknown collector", sections: map[string]any{"whatever": &hoststats.Section{}}},
		{name: "wrong type", sections: map[string]any{"hoststats": "not a section"}},
		{name: "nil value", sections: map[string]any{"network": nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var snap Snapshot
			applyRunSections(&snap, tt.sections)
			if snap.Host != nil || snap.Network != nil || snap.SQLRows != nil || snap.DBPool != nil {
				t.Errorf("snapshot = %+v, want every unusable section skipped", snap)
			}
		})
	}
	// A nil snapshot must be tolerated: rendering is never allowed to panic.
	applyRunSections(nil, fakeRunSections())
}

// fakeRunStarter records the runs a handler opened.
type fakeRunStarter struct {
	calls int
	id    string
	err   error
}

func (f *fakeRunStarter) start(context.Context) (RunStart, error) {
	f.calls++
	if f.err != nil {
		return RunStart{}, f.err
	}
	return RunStart{RunID: f.id, State: "started", StartedAt: time.Unix(1_700_000_000, 0), Validity: "valid"}, nil
}

func TestResetReportsTheRunItOpened(t *testing.T) {
	starter := &fakeRunStarter{id: "run-abcdef0123456789"}
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), StartRun: starter.start})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("X-Isutools-Run-Id"); got != starter.id {
		t.Errorf("X-Isutools-Run-Id = %q, want %q", got, starter.id)
	}
	if starter.calls != 1 {
		t.Errorf("StartRun called %d times, want once per reset", starter.calls)
	}
}

func TestResetRunsInspectionBeforeOpeningTheMeasuredInterval(t *testing.T) {
	started := false
	contaminated := false
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys),
		DB: func(context.Context) *dbinspect.Schema {
			if started {
				contaminated = true
			}
			return &dbinspect.Schema{}
		},
		StartRun: func(context.Context) (RunStart, error) {
			started = true
			return RunStart{RunID: "run-clean", State: "started", StartedAt: time.Now(), Validity: "valid"}, nil
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if contaminated {
		t.Fatal("DB inspection ran after StartRun and contaminated the measured interval")
	}
}

func TestResetFailsClosedWhenRunStartFails(t *testing.T) {
	starter := &fakeRunStarter{err: errors.New("another run is active")}
	registry := health.NewRegistry()
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), StartRun: starter.start, Health: registry,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := rec.Header().Get("X-Isutools-Run-Id"); got != "" {
		t.Errorf("X-Isutools-Run-Id = %q, want no id when no run was opened", got)
	}
	entries, partial := registry.Snapshot()
	if !partial {
		t.Errorf("health = %+v, want the failed run recorded as degraded", entries)
	}
}

// fakeRunTerminator records the calls a transport makes to end a run and
// supplies the finished run's sections only once it has actually been ended.
type fakeRunTerminator struct {
	finishes  int
	completes int
	id        string
	err       error
}

func (f *fakeRunTerminator) finish(context.Context) (RunFinish, error) {
	f.finishes++
	if f.err != nil {
		return RunFinish{}, f.err
	}
	return RunFinish{RunID: f.id, Validity: "valid", AcceptedAt: time.Unix(1_700_000_100, 0)}, nil
}

func (f *fakeRunTerminator) complete(context.Context) (RunFinish, error) {
	f.completes++
	if f.err != nil {
		return RunFinish{}, f.err
	}
	return RunFinish{RunID: f.id, Validity: "valid", AcceptedAt: time.Unix(1_700_000_100, 0)}, nil
}

// sections mirrors the coordinator: a run that has not been ended has produced
// no immutable snapshot, so there is nothing to report.
func (f *fakeRunTerminator) sections() map[string]any {
	if f.finishes == 0 && f.completes == 0 {
		return nil
	}
	return fakeRunSections()
}

// TestCollectIsNonTerminal pins the compatibility guarantee the reset/collect/
// save loop rests on: POST /collect flushes the buffered access log and
// nothing else. It must not open, advance or end a run, and it must not cause
// a run snapshot to appear.
func TestCollectIsNonTerminal(t *testing.T) {
	starter := &fakeRunStarter{id: "run-0123456789abcdef"}
	terminator := &fakeRunTerminator{id: starter.id}
	log := &fakeAccessLog{}
	tbl := agg.NewTable(agg.DefaultMaxKeys)
	tbl.Observe("SELECT 1", time.Millisecond)
	h := NewHandler(Provider{
		SQL: tbl, AccessLog: log, StartRun: starter.start,
		FinishRun: terminator.finish, CompleteRun: terminator.complete,
		Sections: terminator.sections,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}
	before := generationOf(t, h)

	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("collect status = %d, want 204", rec.Code)
		}
		if got := rec.Header().Get("X-Isutools-Run-Id"); got != "" {
			t.Errorf("collect reported run %q; it does not touch the run", got)
		}
	}
	if starter.calls != 1 {
		t.Errorf("StartRun called %d times, want only the reset to open a run", starter.calls)
	}
	if terminator.finishes != 0 || terminator.completes != 0 {
		t.Errorf("collect ended the run (finish=%d complete=%d); only /finish and /save may",
			terminator.finishes, terminator.completes)
	}
	if got := generationOf(t, h); got != before {
		t.Errorf("generation moved from %d to %d; collect must not advance it", before, got)
	}
	if log.collects != 2 {
		t.Errorf("access log flushed %d times, want one per collect", log.collects)
	}

	// No run snapshot may exist while the run is still in flight, so none of
	// its sections may show up in the report either.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	for _, key := range []string{"hoststats", "network", "sqlrows", "dbpool"} {
		if strings.Contains(rec.Body.String(), `"`+key+`"`) {
			t.Errorf("collect produced a run snapshot: %q reached /json", key)
		}
	}
}

// TestFinishEndsTheRunAndSaveCompletesIt covers the two terminal endpoints:
// /finish fixes the boundary and answers 202, /save ends the run and persists
// the sections that ending it produced.
func TestFinishEndsTheRunAndSaveCompletesIt(t *testing.T) {
	dir := t.TempDir()
	starter := &fakeRunStarter{id: "run-fedcba9876543210"}
	terminator := &fakeRunTerminator{id: starter.id}
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir,
		StartRun: starter.start, FinishRun: terminator.finish,
		CompleteRun: terminator.complete, Sections: terminator.sections,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("finish status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Isutools-Run-Id"); got != starter.id {
		t.Errorf("finish run id header = %q, want %q", got, starter.id)
	}
	var accepted RunFinish
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("finish body %q: %v", rec.Body.String(), err)
	}
	if accepted.RunID != starter.id || accepted.Validity != "valid" {
		t.Errorf("finish body = %+v, want the accepted boundary", accepted)
	}
	if terminator.finishes != 1 {
		t.Errorf("FinishRun called %d times, want once", terminator.finishes)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save?score=99", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	if terminator.completes != 1 {
		t.Errorf("CompleteRun called %d times, want once per save", terminator.completes)
	}
	if got := rec.Header().Get("X-Isutools-Run-Id"); got != starter.id {
		t.Errorf("save run id header = %q, want %q", got, starter.id)
	}

	var saved struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("save body: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(saved.File, ".html")+".json"))
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	var payload Snapshot
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode persisted snapshot: %v", err)
	}
	if payload.Network == nil || payload.Network.TCP.TimeWait != 340 {
		t.Errorf("persisted network section = %+v, want the finished run's value", payload.Network)
	}
	if len(payload.DBPool) != 1 {
		t.Errorf("persisted dbpool section = %+v, want the finished run's value", payload.DBPool)
	}
}

func TestSaveRecoversStartedTTLAndOrphansOpenProfiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	startedAt := time.Date(2026, 8, 6, 11, 53, 50, 0, time.UTC)
	expiredAt := startedAt.Add(31 * time.Minute)
	runID := "run-b23e5eab6ac3cdf0"
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir, RuntimeProfiles: []string{"heap"},
		StartRun: func(context.Context) (RunStart, error) {
			return RunStart{RunID: runID, Epoch: 7, State: "started", Validity: "valid", StartedAt: startedAt}, nil
		},
		FinishRun: func(context.Context) (RunFinish, error) {
			return RunFinish{}, errors.New("runctl: unknown run")
		},
		CompleteRun: func(context.Context) (RunFinish, error) {
			return RunFinish{
				RunID: runID, Epoch: 7, State: "aborted", Validity: "invalid",
				StartedAt: startedAt, AcceptedAt: expiredAt,
				Recovered: true, RecoveryReason: "started-ttl",
			}, nil
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save?score=237979", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("recovery save status = %d: %s", rec.Code, rec.Body.String())
	}
	var saved SaveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("save response: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, saved.SnapshotFile))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(body, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Meta.Score != "237979" || !snapshot.Meta.Partial || snapshot.Meta.Run == nil {
		t.Fatalf("recovery provenance = %#v", snapshot.Meta)
	}
	if got := snapshot.Meta.Run; got.RunID != runID || got.Epoch != 7 || got.State != "aborted" ||
		got.Validity != "invalid" || !got.Recovered || got.RecoveryReason != "started-ttl" ||
		!got.StartedAt.Equal(startedAt) || !got.FinishedAt.Equal(expiredAt) {
		t.Fatalf("recovered run = %#v", got)
	}
	if snapshot.Meta.Profiles == nil || len(snapshot.Meta.Profiles.Captures) == 0 || len(snapshot.Meta.Profiles.Pairs) != 0 {
		t.Fatalf("recovered profile manifest = %#v", snapshot.Meta.Profiles)
	}
	for _, capture := range snapshot.Meta.Profiles.Captures {
		if capture.Point == ProfilePointOpen && !capture.Orphan {
			t.Fatalf("open profile remained ambiguous: %#v", capture)
		}
	}
}

func TestSaveDoesNotRecoverAnUnprovenFinishError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir,
		FinishRun: func(context.Context) (RunFinish, error) {
			return RunFinish{}, errors.New("collector freeze failed")
		},
		CompleteRun: func(context.Context) (RunFinish, error) {
			return RunFinish{RunID: "run-not-recovered", Epoch: 1, Validity: "partial"}, nil
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("unproven recovery status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unproven recovery published files: %v", entries)
	}
}

func TestSavePersistsBenchmarkOutcomeIntoTimeline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runID := "run-timeline-outcome"
	started := time.Unix(1_700_000_000, 0)
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir,
		StartRun: func(context.Context) (RunStart, error) {
			return RunStart{RunID: runID, Epoch: 7, State: "started", Validity: "valid", StartedAt: started}, nil
		},
		CompleteRun: func(context.Context) (RunFinish, error) {
			return RunFinish{RunID: runID, Epoch: 7, State: "finished", Validity: "valid", AcceptedAt: started.Add(time.Second)}, nil
		},
		Timeline: func(id string, epoch uint64) *timeline.Section {
			if id != runID || epoch != 7 {
				t.Fatalf("timeline lookup = %q/%d", id, epoch)
			}
			return &timeline.Section{Schema: timeline.SchemaV1, RunID: id, Epoch: epoch,
				Analysis: timeline.Analysis{Available: false, Reason: timeline.ReasonInsufficientBuckets}}
		},
	})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/save?score=236006&pass=false", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response SaveResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, response.SnapshotFile))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Meta.BenchmarkPass == nil || *snapshot.Meta.BenchmarkPass || snapshot.Meta.Score != "236006" ||
		snapshot.Timeline == nil || snapshot.Timeline.Outcome.Pass == nil || *snapshot.Timeline.Outcome.Pass ||
		snapshot.Timeline.Outcome.Score != "236006" || snapshot.Timeline.Outcome.Validity != "valid" {
		t.Fatalf("persisted outcome = meta %#v timeline %#v", snapshot.Meta, snapshot.Timeline)
	}
}

func TestSaveRejectsInvalidBenchmarkPassBeforeEndingRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ended := false
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir,
		CompleteRun: func(context.Context) (RunFinish, error) { ended = true; return RunFinish{}, nil },
	})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/save?pass=maybe", nil))
	if recorder.Code != http.StatusBadRequest || ended {
		t.Fatalf("status=%d ended=%v body=%s", recorder.Code, ended, recorder.Body.String())
	}
}

func TestSaveDirectlyClosesProfilesAndPersistsManifest(t *testing.T) {
	dir := t.TempDir()
	starter := &fakeRunStarter{id: "run-profile-save"}
	terminator := &fakeRunTerminator{id: starter.id}
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir,
		StartRun: starter.start, CompleteRun: terminator.complete,
		Sections: terminator.sections, RuntimeProfiles: []string{"heap"},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("save body: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(saved.File, ".html")+".json"))
	if err != nil {
		t.Fatalf("read persisted snapshot: %v", err)
	}
	var payload Snapshot
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode persisted snapshot: %v", err)
	}
	if payload.Meta.Profiles == nil || len(payload.Meta.Profiles.Pairs) != 1 {
		t.Fatalf("profiles = %+v, want one completed heap pair", payload.Meta.Profiles)
	}
}

func TestCompletedRunEnvelopeIsPersistedWithBoundaryEvidence(t *testing.T) {
	finishedAt := time.Unix(1_700_000_100, 0)
	run := &RunSnapshot{
		Info: RunInfo{
			RunID: "run-evidence", Epoch: 7, Validity: "invalid", Trigger: "reset",
			StartedAt: time.Unix(1_700_000_000, 0), FinishedAt: finishedAt,
			GenerationWindow: BoundaryWindow{Min: finishedAt.Add(-time.Millisecond), Max: finishedAt, Spread: time.Millisecond},
			BoundaryWindow:   BoundaryWindow{Min: finishedAt.Add(-2 * time.Millisecond), Max: finishedAt, Spread: 2 * time.Millisecond},
			Collectors: []RunCollectorBoundary{{
				Name: "sqlrows", Kind: "baseline", Phase: "collect", Required: true,
				At: finishedAt, Code: "collect-failed", Err: "fixture failure", Dropped: true,
			}},
		},
		Sections: map[string]any{},
	}
	h := NewHandler(Provider{
		SQL:         agg.NewTable(agg.DefaultMaxKeys),
		RunSnapshot: func() *RunSnapshot { return run },
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var payload jsonPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Meta.Run == nil || payload.Meta.Run.RunID != run.Info.RunID || payload.Meta.Run.Epoch != 7 {
		t.Fatalf("run metadata = %+v, want %+v", payload.Meta.Run, run.Info)
	}
	if !payload.Meta.Partial {
		t.Fatal("invalid run was not marked partial")
	}
	entry, ok := healthEntryFor(payload.Meta.Health, "sqlrows")
	if !ok || entry.Status != health.StatusFailed || !strings.Contains(entry.Message, "fixture failure") {
		t.Fatalf("sqlrows health = %+v found=%v", entry, ok)
	}
}

func TestFinishWithoutACoordinatorIsUnavailable(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when nothing can end a run", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/finish", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /finish = %d, want 405", rec.Code)
	}
}

func TestAbortEndsRunAndOrphansOpeningProfiles(t *testing.T) {
	dir := t.TempDir()
	const runID = "run-abort012345"
	aborts := 0
	h := newHandler(Provider{
		SQL:             agg.NewTable(agg.DefaultMaxKeys),
		DataDir:         dir,
		RuntimeProfiles: []string{"heap"},
		StartRun: func(context.Context) (RunStart, error) {
			return RunStart{RunID: runID, Epoch: 7, State: "started", StartedAt: time.Now(), Validity: "valid"}, nil
		},
		AbortRun: func(context.Context) (RunAbort, error) {
			aborts++
			return RunAbort{RunID: runID, Epoch: 7, Reason: "explicit", AbortedAt: time.Now()}, nil
		},
	})
	routes := h.routes()

	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/abort", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("abort = %d: %s", rec.Code, rec.Body.String())
	}
	if aborts != 1 || rec.Header().Get(runIDHeader) != runID {
		t.Fatalf("aborts=%d header=%q", aborts, rec.Header().Get(runIDHeader))
	}
	if !h.runEnded.Load() {
		t.Fatal("aborted run must close the collect window")
	}
	manifest := h.profileManifest(runID)
	if manifest == nil || len(manifest.Pairs) != 0 {
		t.Fatalf("manifest = %+v, want an orphan without a pair", manifest)
	}
	foundAbort := false
	for _, capture := range manifest.Captures {
		if capture.Point == ProfilePointClose && capture.Code == profileCodeAborted && capture.Orphan {
			foundAbort = true
		}
	}
	if !foundAbort {
		t.Fatalf("captures = %+v, want explicit aborted close record", manifest.Captures)
	}
}

func TestAbortContract(t *testing.T) {
	t.Run("POST only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewHandler(Provider{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/abort", nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("status=%d allow=%q", rec.Code, rec.Header().Get("Allow"))
		}
	})
	t.Run("coordinator required", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NewHandler(Provider{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/abort", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d, want 503", rec.Code)
		}
	})
}

func TestFinishReportsAConflictWhenThereIsNoRun(t *testing.T) {
	registry := health.NewRegistry()
	terminator := &fakeRunTerminator{err: errors.New("no measurement run is in flight")}
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), Health: registry, FinishRun: terminator.finish,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if _, partial := registry.Snapshot(); !partial {
		t.Error("a refused finish must be recorded as degraded health")
	}
}

// TestSaveSurvivesAFailedRunCompletion keeps the v1.0 contract: the report is
// persisted even when the run bookkeeping around it failed, because the
// measurements themselves are real either way.
func TestSaveSurvivesAFailedRunCompletion(t *testing.T) {
	dir := t.TempDir()
	registry := health.NewRegistry()
	terminator := &fakeRunTerminator{err: errors.New("the run was aborted")}
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir, Health: registry,
		CompleteRun: terminator.complete,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the save to succeed anyway: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Isutools-Run-Id"); got != "" {
		t.Errorf("run id header = %q, want none when no run was completed", got)
	}
	if _, partial := registry.Snapshot(); !partial {
		t.Error("a failed run completion must be recorded as degraded health")
	}
}

// TestRunIntervalSectionsOverlayTheLiveCollectors proves the frozen output of
// a finished run reaches the report. A generation boundary leaves the live
// collector empty, so without the overlay a report rendered after a run was
// finished would show nothing at all.
func TestRunIntervalSectionsOverlayTheLiveCollectors(t *testing.T) {
	sections := map[string]any{
		sqlstats.SectionName: sqlstats.Frozen{
			Generation: 7,
			Entries:    []agg.Entry{{Key: "SELECT * FROM items WHERE id = ?", Count: 12}},
		},
		httpstats.CollectorName: httpstats.Result{
			HTTP:        httpstats.Snapshot{{Method: http.MethodGet, Path: "/items/*", Status: 200, Count: 12}},
			Connections: httpstats.ConnSnapshot{Total: 4, BytesWritten: 900},
		},
		accesslog.SectionName: accesslog.Snapshot{
			Lines:   3,
			Entries: []accesslog.Entry{{Method: http.MethodGet, URI: "/items/1", Count: 3}},
		},
		counters.SectionName: counters.Frozen{Entries: []counters.Entry{{Name: "cache-hit", Count: 5}}},
		procstats.CollectorName: procstats.Snapshot{
			StartedAt: time.Unix(100, 0), EndedAt: time.Unix(160, 0), CPUs: 8,
		},
	}
	var snap Snapshot
	applyRunIntervalSections(&snap, sections)
	if len(snap.SQL) != 1 || snap.SQL[0].Count != 12 {
		t.Errorf("sql = %+v, want the frozen generation", snap.SQL)
	}
	if len(snap.HTTP) != 1 || snap.HTTP[0].Count != 12 {
		t.Errorf("http = %+v, want the frozen generation", snap.HTTP)
	}
	if snap.Connections == nil || snap.Connections.Total != 4 {
		t.Errorf("connections = %+v, want the frozen generation", snap.Connections)
	}
	if snap.AccessLog == nil || snap.AccessLog.Lines != 3 {
		t.Errorf("accesslog = %+v, want the frozen generation", snap.AccessLog)
	}
	if len(snap.Counters) != 1 || snap.Counters[0].Count != 5 {
		t.Errorf("counters = %+v, want the frozen generation", snap.Counters)
	}
	if snap.Proc == nil || snap.Proc.CPUs != 8 || snap.Proc.StartedAt != time.Unix(100, 0) {
		t.Errorf("proc = %+v, want the frozen run boundary", snap.Proc)
	}

	// A present frozen section is authoritative even when it is empty. Keeping
	// live values here would attribute post-boundary traffic to the closed run.
	live := Snapshot{
		SQL:      []agg.Entry{{Key: "SELECT 1", Count: 1}},
		HTTP:     httpstats.Snapshot{{Path: "/live", Count: 1}},
		Counters: []counters.Entry{{Name: "live", Count: 1}},
	}
	applyRunIntervalSections(&live, map[string]any{
		sqlstats.SectionName:    sqlstats.Frozen{},
		httpstats.CollectorName: httpstats.Result{},
		accesslog.SectionName:   accesslog.Snapshot{},
		counters.SectionName:    counters.Frozen{},
		procstats.CollectorName: procstats.Snapshot{},
	})
	if len(live.SQL) != 0 || len(live.HTTP) != 0 || len(live.Counters) != 0 || live.AccessLog == nil || live.Connections == nil || live.Proc == nil {
		t.Errorf("empty frozen run did not replace live data: %+v", live)
	}

	// An access log that read nothing and said why still has to reach the
	// report: "the log could not be parsed" is a finding, not an absence.
	var complained Snapshot
	applyRunIntervalSections(&complained, map[string]any{
		accesslog.SectionName: accesslog.Snapshot{Health: accesslog.Health{
			Status: accesslog.StatusPartial, Message: "malformed access-log records were dropped", Dropped: 3,
		}},
	})
	if complained.AccessLog == nil {
		t.Fatal("an access log generation that only carries a complaint must still be reported")
	}
	entry, ok := healthEntryFor(complained.Meta.Health, "accesslog")
	if !ok || !strings.Contains(entry.Message, "malformed access-log records") {
		t.Errorf("accesslog health = %+v (found=%v), want the parser's complaint", entry, ok)
	}
	// Neither a nil snapshot nor a nil map may panic: this is a render path.
	applyRunIntervalSections(nil, sections)
	applyRunIntervalSections(&snap, nil)
}

// TestRunSectionNotesReachSnapshotHealth proves the collectors' own notes are
// forwarded. Every one of them is produced inside a Collect that must stay
// pure, so a note nobody forwards is a note nobody ever sees — and the reader
// is left with a missing section and no reason for it.
func TestRunSectionNotesReachSnapshotHealth(t *testing.T) {
	sections := map[string]any{
		hoststats.CollectorName: &hoststats.Section{Partial: true, Codes: []string{"not-captured:psi"}},
		netstats.Default.Name(): &netstats.NetworkStats{
			Health: []netstats.HealthNote{{Key: netstats.HealthProcUnreadable, Detail: "/proc/net/dev"}},
		},
		sqlrows.Name: &sqlrows.Section{
			Health:  []sqlrows.HealthNote{{Key: sqlrows.HealthSkip, Message: "app"}},
			Targets: []sqlrows.TargetSection{{TargetID: "app", Code: "probe-skip", Reason: "no performance_schema"}},
		},
		dbpool.Name: []dbpool.Entry{{TargetID: "app", Partial: true, Code: dbpool.CodeUnwatchedMidRun}},
	}
	h := NewHandler(Provider{
		SQL:      agg.NewTable(agg.DefaultMaxKeys),
		Sections: func() map[string]any { return sections },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var payload jsonPayload
	decodeJSON(t, rec, &payload)

	if !payload.Meta.Partial {
		t.Error("a run whose sections carry degradation notes must be marked partial")
	}
	for collector, want := range map[string]string{
		hoststats.CollectorName: "not-captured:psi",
		netstats.Default.Name(): "/proc/net/dev",
		sqlrows.Name:            "no performance_schema",
		dbpool.Name:             dbpool.CodeUnwatchedMidRun,
	} {
		entry, ok := healthEntryFor(payload.Meta.Health, collector)
		if !ok || entry.Status == health.StatusOK || !strings.Contains(entry.Message, want) {
			t.Errorf("%s health = %+v (found=%v), want a note containing %q",
				collector, entry, ok, want)
		}
	}
}

func healthEntryFor(entries []health.Entry, collector string) (health.Entry, bool) {
	for _, entry := range entries {
		if entry.Collector == collector {
			return entry, true
		}
	}
	return health.Entry{}, false
}

func TestResetCapturesBoundaryProfiles(t *testing.T) {
	dir := t.TempDir()
	starter := &fakeRunStarter{id: "run-a1b2c3d4e5f60718"}
	h := NewHandler(Provider{
		SQL:             agg.NewTable(agg.DefaultMaxKeys),
		DataDir:         dir,
		StartRun:        starter.start,
		RuntimeProfiles: []string{"heap"},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*_heap_open.pprof"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("heap artifacts = %v (err %v), want exactly one", matches, err)
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Error("captured profile is empty")
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %v, want 0600", mode)
	}
	// The publication is atomic: nothing may be left behind under the
	// temporary suffix, which /files/ refuses to serve.
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) > 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}

	name := filepath.Base(matches[0])
	if !strings.Contains(name, "_gen") || !strings.Contains(name, "run-a1b2") {
		t.Errorf("artifact %q must carry the generation and the run id", name)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("profile download status = %d", rec.Code)
	}
}

// TestFinishCapturesTheClosingHalfAndSaveRecordsThePair drives plan 07's
// close point through the endpoint that owns it.
//
// POST /finish is where the closing capture belongs, because FinishRun returns
// the accepted boundary without waiting for the drain, the collect or the
// snapshot build. The save that follows must not take a second one — it would
// be taken after all of that — and it is what publishes the pair into the
// persisted report, since that is the first moment both halves exist.
func TestFinishCapturesTheClosingHalfAndSaveRecordsThePair(t *testing.T) {
	dir := t.TempDir()
	starter := &fakeRunStarter{id: "run-close012345"}
	terminator := &fakeRunTerminator{id: starter.id}
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir,
		StartRun: starter.start, FinishRun: terminator.finish,
		CompleteRun: terminator.complete, Sections: terminator.sections,
		RuntimeProfiles: []string{"heap"},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*_heap_close.pprof")); len(matches) != 0 {
		t.Fatalf("closing artifacts = %v, want none before the run is finished", matches)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("finish status = %d: %s", rec.Code, rec.Body.String())
	}
	closing, _ := filepath.Glob(filepath.Join(dir, "*_heap_close.pprof"))
	if len(closing) != 1 {
		t.Fatalf("closing artifacts = %v, want exactly one written by /finish", closing)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/save", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	if again, _ := filepath.Glob(filepath.Join(dir, "*_heap_close.pprof")); len(again) != 1 {
		t.Errorf("closing artifacts = %v, want the save to reuse the one /finish took", again)
	}

	var saved struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("save body %q: %v", rec.Body.String(), err)
	}
	body, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(saved.File, ".html")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload Snapshot
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("saved json: %v", err)
	}
	manifest := payload.Meta.Profiles
	if manifest == nil || len(manifest.Pairs) != 1 {
		t.Fatalf("manifest = %+v, want the run's differenceable pair", manifest)
	}
	pair := manifest.Pairs[0]
	if pair.Kind != "heap" || pair.OpenFile == "" || pair.CloseFile != filepath.Base(closing[0]) {
		t.Errorf("pair = %+v, want the two halves that were written", pair)
	}
	// The residual is what makes the pair readable as an approximation rather
	// than as the run itself, so it has to survive into the saved report.
	if pair.ApproxErrorNs != pair.HeadLossNs+pair.TailExcessNs {
		t.Errorf("pair residual = %+v, want head + tail", pair)
	}
	if !strings.Contains(pair.DiffCommand, "-base") || strings.Contains(pair.DiffCommand, "-diff_base") {
		t.Errorf("diff command = %q", pair.DiffCommand)
	}
	// The Runs detail view is the saved HTML; the pair has to be visible there
	// with its approximation notice.
	page, err := os.ReadFile(filepath.Join(dir, saved.File))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<h2>Profiles", "run 冒頭の", "finish freeze 後の"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("saved run page is missing %q", want)
		}
	}
}

// TestSaveWithoutProfilesCarriesNoManifest keeps the default configuration
// byte-identical: nothing captured, nothing claimed.
func TestSaveWithoutProfilesCarriesNoManifest(t *testing.T) {
	dir := t.TempDir()
	starter := &fakeRunStarter{id: "run-noprof0123"}
	terminator := &fakeRunTerminator{id: starter.id}
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir,
		StartRun: starter.start, FinishRun: terminator.finish,
		CompleteRun: terminator.complete, Sections: terminator.sections,
	})
	for _, path := range []string{"/reset", "/finish", "/save"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code >= http.StatusBadRequest {
			t.Fatalf("POST %s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof")); len(matches) > 0 {
		t.Fatalf("profiles = %v, want none by default", matches)
	}
	for _, name := range []string{"*.json"} {
		files, _ := filepath.Glob(filepath.Join(dir, name))
		for _, file := range files {
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), `"profiles"`) {
				t.Errorf("%s carries a profile manifest with profiling off", file)
			}
		}
	}
}

func TestResetWithoutRuntimeProfilesWritesNothing(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", rec.Code)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof")); len(matches) > 0 {
		t.Fatalf("profiles = %v, want none captured by default", matches)
	}
}

func TestCaptureRuntimeProfilesSkipsUnusableRequests(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		provider Provider
	}{
		{name: "no data dir", provider: Provider{RuntimeProfiles: []string{"heap"}}},
		{name: "unknown profile", provider: Provider{DataDir: dir, RuntimeProfiles: []string{"nonexistent"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &handler{p: tt.provider, operation: make(chan struct{}, 1)}
			if got := h.captureRuntimeProfiles(RunStart{RunID: "run-1"}, ProfilePointOpen, 1); got != nil {
				t.Errorf("written = %v, want nothing", got)
			}
			if matches, _ := filepath.Glob(filepath.Join(dir, "*.pprof")); len(matches) > 0 {
				t.Errorf("artifacts = %v, want none", matches)
			}
		})
	}
}

func TestProfileArtifactName(t *testing.T) {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, reportTZ)
	tests := []struct {
		name string
		run  RunStart
		kind string
		want string
	}{
		{
			name: "open capture",
			run:  RunStart{RunID: "run-a1b2c3d4e5f6", StartedAt: at},
			kind: "mutex",
			want: "20260805-120000_gen7_run-a1b2_mutex_open.pprof",
		},
		{
			// Without a coordinator there is no run to name, but the artifact
			// still has to be written somewhere unambiguous.
			name: "no run",
			run:  RunStart{StartedAt: at},
			kind: "heap",
			want: "20260805-120000_gen7_norun_heap_open.pprof",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := profileArtifactName(tt.run, ProfilePointOpen, 7, tt.kind); got != tt.want {
				t.Errorf("name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProfileArtifactNameFallsBackToTheCaptureMoment(t *testing.T) {
	// A run with no recorded boundary still has to produce a usable, sortable
	// name rather than one stamped with the zero time.
	got := profileArtifactName(RunStart{RunID: "run-1"}, ProfilePointClose, 2, "heap")
	if !strings.HasSuffix(got, "_gen2_run-1_heap_close.pprof") {
		t.Fatalf("name = %q", got)
	}
	if strings.HasPrefix(got, "00010101") {
		t.Errorf("name = %q, want the capture moment instead of the zero time", got)
	}
}

func TestWriteRuntimeProfileReportsFailures(t *testing.T) {
	h := &handler{p: Provider{DataDir: filepath.Join(t.TempDir(), "missing")}}
	if _, err := h.writeRuntimeProfile("heap", RunStart{RunID: "run-1"}, ProfilePointOpen, 1); err == nil {
		t.Fatal("writing into a missing directory must report an error")
	}
	if _, err := h.writeRuntimeProfile("nonexistent", RunStart{RunID: "run-1"}, ProfilePointOpen, 1); err == nil {
		t.Fatal("an unknown profile kind must report an error")
	}
}

// TestProfileArtifactPairsShareOnePrefix pins the naming rule the difference
// between two profiles depends on: both ends of one run must be reassemblable
// from a directory listing, so only the point may differ.
func TestProfileArtifactPairsShareOnePrefix(t *testing.T) {
	run := RunStart{RunID: "run-a1b2c3d4e5f6", StartedAt: time.Date(2026, 8, 5, 12, 0, 0, 0, reportTZ)}
	open := profileArtifactName(run, ProfilePointOpen, 3, "mutex")
	closed := profileArtifactName(run, ProfilePointClose, 3, "mutex")
	prefix := "20260805-120000_gen3_run-a1b2_mutex_"
	if !strings.HasPrefix(open, prefix) || !strings.HasPrefix(closed, prefix) {
		t.Fatalf("names %q and %q must share the prefix %q", open, closed, prefix)
	}
	if open == closed {
		t.Fatal("the two boundaries must produce distinct names")
	}
}

func generationOf(t *testing.T, h http.Handler) int64 {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	var got Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got.Meta.Generation
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

func TestSnapshotCarriesBuildProvenanceVerdict(t *testing.T) {
	snap := newHandler(Provider{}).take()
	if snap.Meta.BuildSource == "" {
		t.Fatal("build source must distinguish unknown from an omitted field")
	}
	wantValid := snap.Meta.BuildSource != "unknown" && snap.Meta.Revision != "unknown" && !snap.Meta.Dirty
	if snap.Meta.ProvenanceValid != wantValid {
		t.Fatalf("provenance valid=%v for source=%q revision=%q dirty=%v",
			snap.Meta.ProvenanceValid, snap.Meta.BuildSource, snap.Meta.Revision, snap.Meta.Dirty)
	}
}

// TestCollectAfterFinishIsRefusedUntilReset protects the interval a closing
// boundary just fixed. A flush reads the log to end of file, so running one
// after the freeze would pull the next generation's lines into the run the
// coordinator is still cutting.
func TestCollectAfterFinishIsRefusedUntilReset(t *testing.T) {
	starter := &fakeRunStarter{id: "run-aaaabbbbccccdddd"}
	terminator := &fakeRunTerminator{id: starter.id}
	log := &fakeAccessLog{}
	h := NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), AccessLog: log,
		StartRun: starter.start, FinishRun: terminator.finish,
		CompleteRun: terminator.complete, Sections: terminator.sections,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("collect during the run = %d, want 204", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("finish = %d, want 202", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("collect after finish = %d, want 409", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a refused collect must say when to retry")
	}
	if log.collects != 1 {
		t.Errorf("access log flushed %d times, want the refused collect to read nothing", log.collects)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("collect after the next reset = %d, want 204", rec.Code)
	}
}

// logGeneration is one access-log generation: the lines a flush has moved into
// it. A frozen run section and the live collector share the pointer until the
// next reset rotates it, which is why a late flush is visible inside a section
// that was supposedly already fixed.
type logGeneration struct {
	lines []string
}

// generationalAccessLog models the part of the real collector that the freeze
// point protects. Traffic reaches the log file first (write); only a flush
// (Collect) moves it into the live generation; freeze pins that generation as
// the finished run's section; Reset rotates to a fresh one.
type generationalAccessLog struct {
	mu       sync.Mutex
	pending  []string
	current  *logGeneration
	frozen   *logGeneration
	collects int
}

func newGenerationalAccessLog() *generationalAccessLog {
	return &generationalAccessLog{current: &logGeneration{}}
}

// write is traffic appended to the log file by the application.
func (l *generationalAccessLog) write(line string) {
	l.mu.Lock()
	l.pending = append(l.pending, line)
	l.mu.Unlock()
}

func (l *generationalAccessLog) Collect() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.collects++
	l.current.lines = append(l.current.lines, l.pending...)
	l.pending = nil
	return nil
}

func (l *generationalAccessLog) Snapshot() accesslog.Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return accesslog.Snapshot{Lines: int64(len(l.current.lines))}
}

func (l *generationalAccessLog) Reset() error {
	l.mu.Lock()
	l.current = &logGeneration{}
	l.mu.Unlock()
	return nil
}

// freeze is the coordinator's closing boundary: the run's section is the
// generation the live collector holds at this instant.
func (l *generationalAccessLog) freeze() {
	l.mu.Lock()
	l.frozen = l.current
	l.mu.Unlock()
}

// runSection is the finished run's access-log data.
func (l *generationalAccessLog) runSection() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.frozen == nil {
		return nil
	}
	return append([]string(nil), l.frozen.lines...)
}

func (l *generationalAccessLog) flushes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.collects
}

// TestCollectResumingAfterFinishCannotCrossTheFreezePoint drives the
// interleaving that /collect's guard alone does not cover: the flush tests the
// run-ended flag, is descheduled before it claims the operation slot, a
// /finish takes the slot and fixes the closing boundary, and only then does the
// flush resume. Reading the log to end of file there would pull traffic
// produced after measurement stopped into the section the coordinator just
// froze, so the boundary has to be re-tested inside the mutually exclusive
// region rather than only before it.
func TestCollectResumingAfterFinishCannotCrossTheFreezePoint(t *testing.T) {
	log := newGenerationalAccessLog()
	starter := &fakeRunStarter{id: "run-0f0f0f0f0f0f0f0f"}
	terminator := &fakeRunTerminator{id: starter.id}
	h := newHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), AccessLog: log,
		StartRun: starter.start,
		FinishRun: func(ctx context.Context) (RunFinish, error) {
			log.freeze()
			return terminator.finish(ctx)
		},
	})
	mux := h.routes()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204", rec.Code)
	}
	log.write("inside-the-run")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("collect during the run = %d, want 204", rec.Code)
	}
	if got := log.flushes(); got != 1 {
		t.Fatalf("flushes during the run = %d, want 1", got)
	}

	// Park the next flush exactly where the race lives: past its own check of
	// the run-ended flag, owning nothing, so /finish can still run.
	parked, release := make(chan struct{}), make(chan struct{})
	h.collectPause = func() {
		close(parked)
		<-release
	}
	collected := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/collect", nil))
		collected <- rec
	}()
	<-parked

	finished := httptest.NewRecorder()
	mux.ServeHTTP(finished, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if finished.Code != http.StatusAccepted {
		t.Fatalf("finish status = %d, want 202: %s", finished.Code, finished.Body.String())
	}
	// Traffic the benchmarker produced after measurement stopped. It belongs to
	// the next generation and to no run.
	log.write("after-the-boundary")
	close(release)

	resumed := <-collected
	if resumed.Code != http.StatusConflict {
		t.Errorf("collect resuming after the boundary = %d, want 409", resumed.Code)
	}
	if resumed.Header().Get("Retry-After") == "" {
		t.Error("a refused collect must say when to retry")
	}
	if got := log.flushes(); got != 1 {
		t.Errorf("access log flushed %d times, want the post-boundary flush refused", got)
	}
	if got := log.runSection(); len(got) != 1 || got[0] != "inside-the-run" {
		t.Fatalf("the finished run's access-log section = %q, want only the traffic inside the run", got)
	}
}

// tailingAccessLog models the two reads the real collector offers over a file
// it tails.
//
// Lines reach the file first (write). Only a read that flushes moves them into
// the live generation: Snapshot always does, Peek does so only while no freeze
// point is outstanding. freeze fixes the closing boundary — the run's section
// is the generation the collector holds, and the drain that follows may still
// consume the lines that were already in the file at that moment, and only
// those. That is what makes an unguarded flush destructive: it hands the whole
// file to a generation the drain is about to seal.
type tailingAccessLog struct {
	mu sync.Mutex
	// pending is in the file but not yet consumed.
	pending []string
	current *logGeneration
	frozen  *logGeneration
	// freezeAt is how many pending lines lie below the freeze point, i.e. how
	// many the drain is still entitled to consume.
	freezeAt int
	boundary bool
	resets   int
}

func newTailingAccessLog() *tailingAccessLog {
	return &tailingAccessLog{current: &logGeneration{}}
}

// write is traffic the application appended to the log file.
func (l *tailingAccessLog) write(line string) {
	l.mu.Lock()
	l.pending = append(l.pending, line)
	l.mu.Unlock()
}

// flushLocked reads the file to end of file, which is precisely what leaves
// the drain nothing of its own to consume.
func (l *tailingAccessLog) flushLocked() {
	l.current.lines = append(l.current.lines, l.pending...)
	l.pending = nil
	l.freezeAt = 0
}

func (l *tailingAccessLog) Collect() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	return nil
}

func (l *tailingAccessLog) Snapshot() accesslog.Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushLocked()
	return accesslog.Snapshot{Lines: int64(len(l.current.lines))}
}

func (l *tailingAccessLog) Peek() accesslog.Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.boundary {
		l.flushLocked()
	}
	return accesslog.Snapshot{Lines: int64(len(l.current.lines))}
}

// Reset is the legacy rotation: the aggregate is dropped and the tail
// re-baselined at the current end of file.
func (l *tailingAccessLog) Reset() error {
	l.mu.Lock()
	l.resets++
	l.current.lines = nil
	l.current = &logGeneration{}
	l.pending = nil
	l.freezeAt = 0
	l.mu.Unlock()
	return nil
}

// freeze is the coordinator's closing boundary.
func (l *tailingAccessLog) freeze() {
	l.mu.Lock()
	l.frozen = l.current
	l.freezeAt = len(l.pending)
	l.boundary = true
	l.mu.Unlock()
}

// drain consumes exactly the lines below the freeze point and seals the run's
// section, the way GenerationCollector.Drain does.
func (l *tailingAccessLog) drain() {
	l.mu.Lock()
	n := min(l.freezeAt, len(l.pending))
	l.current.lines = append(l.current.lines, l.pending[:n]...)
	l.pending = append([]string(nil), l.pending[n:]...)
	l.current = &logGeneration{}
	l.freezeAt = 0
	l.boundary = false
	l.mu.Unlock()
}

// runSection is the finished run's access-log data.
func (l *tailingAccessLog) runSection() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.frozen == nil {
		return nil
	}
	return append([]string(nil), l.frozen.lines...)
}

func (l *tailingAccessLog) resetCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resets
}

// newBoundaryHandler wires a handler around log with a coordinator whose
// closing boundary freezes it, which is the shape POST /finish and POST /save
// see in production.
func newBoundaryHandler(log *tailingAccessLog) (http.Handler, *fakeRunTerminator) {
	starter := &fakeRunStarter{id: "run-1234abcd5678ef90"}
	terminator := &fakeRunTerminator{id: starter.id}
	freeze := func(ctx context.Context) (RunFinish, error) {
		log.freeze()
		return terminator.finish(ctx)
	}
	return NewHandler(Provider{
		SQL: agg.NewTable(agg.DefaultMaxKeys), AccessLog: log,
		AccessLogGenerationManaged: true,
		StartRun:                   starter.start,
		FinishRun:                  freeze,
		CompleteRun:                freeze,
		Sections:                   terminator.sections,
	}), terminator
}

// TestDashboardReadBetweenFinishAndDrainKeepsThePostBoundaryTrafficOut covers
// the window POST /finish opens on purpose: it answers 202 as soon as the
// closing boundary exists and drains in the background. GET /, /live, /json
// and /snapshot.html take neither resetMu nor the operation slot and never
// consult the run-ended flag, so a dashboard refresh reaches the collector
// there routinely. Reading the log to end of file at that point advances the
// offset past the freeze point, and the drain then seals traffic produced
// after measurement stopped into the section it is cutting.
func TestDashboardReadBetweenFinishAndDrainKeepsThePostBoundaryTrafficOut(t *testing.T) {
	log := newTailingAccessLog()
	h, _ := newBoundaryHandler(log)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d, want 204", rec.Code)
	}
	log.write("inside-the-run")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("finish = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	// Traffic the benchmarker produced after measurement stopped, and the
	// dashboard refresh that lands before the background drain.
	log.write("after-the-boundary")
	for _, path := range []string{"/", "/live", "/json", "/snapshot.html"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
	}
	log.drain()

	if got := log.runSection(); len(got) != 1 || got[0] != "inside-the-run" {
		t.Fatalf("the finished run's access-log section = %q, want only the traffic inside the run", got)
	}
}

// TestResetLeavesAFrozenAccessLogGenerationToItsDrain covers the other half of
// the same window: a bench script that finishes a run and immediately resets
// for the next one. The legacy rotation re-baselines the tail at the current
// end of file, drops the aggregate and zeroes the counters the drain's
// file-replacement guard reads, so the drain arrives to find nothing left and
// seals the run's section empty. The generation adapter already cuts the
// aggregate at the boundary's own offset, so the reset has nothing to do.
func TestResetLeavesAFrozenAccessLogGenerationToItsDrain(t *testing.T) {
	log := newTailingAccessLog()
	h, _ := newBoundaryHandler(log)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d, want 204", rec.Code)
	}
	log.write("inside-the-run")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("finish = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	// The next run is opened before the previous one's drain has landed.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second reset = %d, want 204", rec.Code)
	}
	log.drain()

	if got := log.resetCalls(); got != 0 {
		t.Errorf("access log re-baselined %d times, want the generation adapter left in charge", got)
	}
	if got := log.runSection(); len(got) != 1 || got[0] != "inside-the-run" {
		t.Fatalf("the finished run's access-log section = %q, want the traffic the run measured", got)
	}
}

// TestResetWithoutAGenerationAdapterStillRotatesTheAccessLog is the guard on
// the other side of that flag: an integration wired without a run coordinator
// has nobody else to rotate its access log, so the legacy reset must still run.
func TestResetWithoutAGenerationAdapterStillRotatesTheAccessLog(t *testing.T) {
	log := newTailingAccessLog()
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), AccessLog: log})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d, want 204", rec.Code)
	}
	if got := log.resetCalls(); got != 1 {
		t.Errorf("access log re-baselined %d times, want once per reset", got)
	}
}

// cutShortHTTP is an httpstats.Collector whose Reset gives up waiting for
// in-flight requests, the bounded wait the real collector performs when a
// handler has not returned by the time the generation is closed.
type cutShortHTTP struct {
	cutShort int64
	// cutsPerReset is how much the counter moves on each rotation; 0 models a
	// rotation that waited out every in-flight request.
	cutsPerReset int64
}

func (c *cutShortHTTP) Snapshot() httpstats.Snapshot { return nil }

func (c *cutShortHTTP) Reset() httpstats.Snapshot {
	c.cutShort += c.cutsPerReset
	return httpstats.Snapshot{{Method: http.MethodGet, Path: "/items", Status: 200, Count: 1}}
}

func (c *cutShortHTTP) ResetsCutShort() int64 { return c.cutShort }

// TestResetMarksACutShortHTTPSectionPartial reads the signal httpstats.Reset
// records when its drain budget expires. The snapshot it returns is missing
// every request that was still running at the boundary; nothing else in the
// report says so, so a reader would compare a section with a hole in it
// against a complete one as if the two measured the same thing.
func TestResetMarksACutShortHTTPSectionPartial(t *testing.T) {
	collector := &cutShortHTTP{cutsPerReset: 1}
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), HTTP: collector})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d, want 204", rec.Code)
	}
	prev := previousSnapshot(t, h)
	if !prev.Meta.Partial {
		t.Errorf("meta.partial = false, want a section missing in-flight requests marked partial")
	}
	entry, found := findHealth(prev.Meta.Health, "http")
	if !found || entry.Status != health.StatusDegraded {
		t.Fatalf("http health = %+v (found=%v), want a degraded entry", entry, found)
	}
	if !strings.Contains(entry.Message, "in-flight") {
		t.Errorf("http health message = %q, want it to name the missing in-flight requests", entry.Message)
	}
}

// TestResetLeavesACompleteHTTPSectionAlone is the negative: a rotation that
// waited out every request produced a complete section, and marking that
// partial would make the signal worthless.
func TestResetLeavesACompleteHTTPSectionAlone(t *testing.T) {
	collector := &cutShortHTTP{}
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), HTTP: collector})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset = %d, want 204", rec.Code)
	}
	prev := previousSnapshot(t, h)
	if entry, found := findHealth(prev.Meta.Health, "http"); found && entry.Status != health.StatusOK {
		t.Errorf("http health = %+v, want no complaint about a complete section", entry)
	}
	if prev.Meta.Partial {
		t.Errorf("meta.partial = true, want a complete section left alone")
	}
}

// previousSnapshot reads the snapshot the last reset froze, which is the one
// the reset endpoint itself built.
func previousSnapshot(t *testing.T, h http.Handler) Snapshot {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /json = %d", rec.Code)
	}
	var payload jsonPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode /json: %v", err)
	}
	if payload.Prev == nil {
		t.Fatal("GET /json carried no previous snapshot")
	}
	return *payload.Prev
}

func findHealth(entries []health.Entry, collector string) (health.Entry, bool) {
	for _, entry := range entries {
		if entry.Collector == collector {
			return entry, true
		}
	}
	return health.Entry{}, false
}
