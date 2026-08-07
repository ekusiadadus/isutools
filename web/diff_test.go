package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/procstats"
)

func boolPointer(value bool) *bool { return &value }

func TestDetectContradictionWhenResourceImprovesButScoreAndPassRegress(t *testing.T) {
	a := Snapshot{
		Meta:   Meta{Score: "236006", BenchmarkPass: boolPointer(true), Run: &RunInfo{Validity: "valid"}},
		DBPool: []dbpool.Entry{{TargetID: "main", WaitDuration: 4 * time.Second}},
		HTTP:   httpstats.Snapshot{{Status: 200, Count: 100}},
	}
	b := Snapshot{
		Meta:   Meta{Score: "189398", BenchmarkPass: boolPointer(false), Run: &RunInfo{Validity: "valid"}},
		DBPool: []dbpool.Entry{{TargetID: "main", WaitDuration: time.Second}},
		HTTP:   httpstats.Snapshot{{Status: 200, Count: 100}},
	}
	got := detectContradictions(a, b)
	if len(got) != 2 {
		t.Fatalf("contradictions = %#v, want score and pass regressions", got)
	}
	for _, row := range got {
		if len(row.Improvements) != 1 || row.Improvements[0].Metric != "dbpool.wait_duration_ns" ||
			row.Outcome.Formula == "" || row.Outcome.Limitation == "" {
			t.Fatalf("untraceable contradiction = %#v", row)
		}
	}
}

func TestDetectContradictionRequiresBothSides(t *testing.T) {
	a := Snapshot{Meta: Meta{Score: "100"}, DBPool: []dbpool.Entry{{WaitDuration: time.Second}}}
	b := Snapshot{Meta: Meta{Score: "200"}, DBPool: []dbpool.Entry{{WaitDuration: time.Millisecond}}}
	if got := detectContradictions(a, b); len(got) != 0 {
		t.Fatalf("resource-only change should not warn: %#v", got)
	}
	b.Meta.Score = "50"
	b.DBPool[0].WaitDuration = 2 * time.Second
	if got := detectContradictions(a, b); len(got) != 0 {
		t.Fatalf("outcome-only regression should not warn: %#v", got)
	}
}

func TestResourceImprovementsCoverCPUAndComparableSQLHTTPLatency(t *testing.T) {
	a := Snapshot{
		Proc: &procstats.Snapshot{CPUTotal: &procstats.CPUTotal{BusyPercent: 95}},
		SQL:  []agg.Entry{{Key: "SELECT ?", Count: 10, Total: 2 * time.Second}},
		HTTP: httpstats.Snapshot{{Key: "GET /items/{id}", Count: 20, Total: time.Second}},
	}
	b := Snapshot{
		Proc: &procstats.Snapshot{CPUTotal: &procstats.CPUTotal{BusyPercent: 60}},
		SQL:  []agg.Entry{{Key: "SELECT ?", Count: 10, Total: time.Second}},
		HTTP: httpstats.Snapshot{{Key: "GET /items/{id}", Count: 20, Total: 500 * time.Millisecond}},
	}
	got := resourceImprovements(a, b)
	metrics := make(map[string]diffEvidence, len(got))
	for _, row := range got {
		metrics[row.Metric] = row
	}
	for _, metric := range []string{"proc.cpu_total.busy_percent", "sql.avg_latency_ms", "http.avg_latency_ms"} {
		row, ok := metrics[metric]
		if !ok || row.Formula == "" || row.Limitation == "" || row.B >= row.A {
			t.Errorf("improvement %q = %#v, present=%v", metric, row, ok)
		}
	}

	// A lower total caused only by lower traffic is not an efficiency claim.
	b.SQL[0].Count = 5
	b.HTTP[0].Count = 10
	for _, row := range resourceImprovements(a, b) {
		if row.Metric == "sql.avg_latency_ms" || row.Metric == "http.avg_latency_ms" {
			t.Fatalf("non-comparable latency was called an improvement: %#v", row)
		}
	}
}

func TestDiffRendersISUCON14RegressionAsGenericTraceableContradiction(t *testing.T) {
	dir := t.TempDir()
	a := Snapshot{
		Meta:   Meta{Score: "236006", BenchmarkPass: boolPointer(true), Run: &RunInfo{Validity: "valid"}},
		DBPool: []dbpool.Entry{{TargetID: "main", WaitDuration: 4 * time.Second}},
		HTTP:   httpstats.Snapshot{{Status: 200, Count: 100}},
	}
	b := Snapshot{
		Meta:   Meta{Score: "189398", BenchmarkPass: boolPointer(false), Run: &RunInfo{Validity: "valid"}},
		DBPool: []dbpool.Entry{{TargetID: "main", WaitDuration: time.Second}},
		HTTP:   httpstats.Snapshot{{Status: 200, Count: 100}},
	}
	write := func(base string, snapshot Snapshot) {
		t.Helper()
		body, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		writeRunJSON(t, dir, base, string(body))
	}
	write("20260806-120000_gen7_aaa_score236006", a)
	write("20260806-121000_gen8_bbb_score189398", b)
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir})
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/diff?a=20260806-120000&b=20260806-121000", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("diff status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{
		"resource-improved / outcome-regressed (correlation warning)",
		"benchmark score", "meta.score", "numeric B score &lt; numeric A score", "one pair is not a causal estimate",
		"database/sql pool wait", "dbpool.wait_duration_ns", "B total wait duration &lt; A total wait duration", "concurrency-weighted",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generic contradiction report missing %q", want)
		}
	}
	for _, forbidden := range []string{"ISURIDE", "chair", "ride", "owner"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("case-study report embedded application semantics %q", forbidden)
		}
	}
}

func writeRunJSON(t *testing.T, dir, base, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, base+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+".html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newDiffHandler(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	writeRunJSON(t, dir, "20260804-010000_gen2_aaa_score100",
		`{"meta":{"score":"100"},"sql":[{"key":"SELECT slow","count":100,"total_ns":5000000000},{"key":"SELECT fixed","count":50,"total_ns":9000000000}],"http":[{"key":"GET / HTTP/1.1 200","count":10,"total_ns":1000000000}]}`)
	writeRunJSON(t, dir, "20260804-020000_gen2_bbb_score200",
		`{"meta":{"score":"200"},"sql":[{"key":"SELECT slow","count":100,"total_ns":8000000000}],"http":[{"key":"GET / HTTP/1.1 200","count":10,"total_ns":500000000}]}`)
	h := NewHandler(Provider{SQL: agg.NewTable(agg.DefaultMaxKeys), DataDir: dir})
	return h, dir
}

func TestDiffComparesTwoRuns(t *testing.T) {
	h, _ := newDiffHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/diff?a=20260804-010000&b=20260804-020000", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"SELECT slow", "SELECT fixed", "score", "100", "200"} {
		if !strings.Contains(body, want) {
			t.Errorf("diff missing %q", want)
		}
	}
	for _, want := range []string{"A count", "B count", "avg(ms)", "件数が異なる行"} {
		if !strings.Contains(body, want) {
			t.Errorf("diff missing comparability information %q", want)
		}
	}
	if !strings.Contains(body, "build provenance unverified") {
		t.Error("diff of legacy/unknown builds must carry a provenance warning")
	}
	// "SELECT fixed" disappeared in b: its delta (-9000ms) must be shown.
	if !strings.Contains(body, "-9000.0") {
		t.Errorf("expected resolved-query negative delta in body")
	}
}

func TestDiffUnknownRun404(t *testing.T) {
	h, _ := newDiffHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/diff?a=20260804-010000&b=20990101-000000", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDiffInvalidParams400(t *testing.T) {
	h, _ := newDiffHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/diff?a=..&b=x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestIndexLinksDiffToPreviousRun(t *testing.T) {
	h, _ := newDiffHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), "diff?a=20260804-010000&amp;b=20260804-020000") &&
		!strings.Contains(rec.Body.String(), "diff?a=20260804-010000&b=20260804-020000") {
		t.Error("index must link each run to a diff against the previous run")
	}
}

func TestLoadRunRejectsOversizedStoredJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20260804-010000_gen1_rev.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxSnapshotBytes+1); err != nil {
		t.Fatal(err)
	}
	h := &handler{p: Provider{DataDir: dir}}
	if _, err := h.loadRun("20260804-010000"); !errors.Is(err, errSnapshotTooLarge) {
		t.Fatalf("loadRun error = %v, want size limit", err)
	}
}

func TestLoadRunRejectsAmbiguousLegacySecondID(t *testing.T) {
	dir := t.TempDir()
	writeRunJSON(t, dir, "20260804-010000_gen1_a", `{"meta":{"score":"1"}}`)
	writeRunJSON(t, dir, "20260804-010000_gen1_b", `{"meta":{"score":"2"}}`)
	h := &handler{p: Provider{DataDir: dir}}
	if _, err := h.loadRun("20260804-010000"); err == nil {
		t.Fatal("ambiguous legacy run id must not select an arbitrary snapshot")
	}
	router := NewHandler(Provider{DataDir: dir})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/20260804-010000", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("ambiguous detail status = %d, want 409", rec.Code)
	}
}
