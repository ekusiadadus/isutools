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

	"github.com/ekusiadadus/isutools/internal/agg"
)

func TestSaveExperimentMetadataRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Provider{SQL: agg.NewTable(10), DataDir: dir})
	body := `{"experiment":{"label":"batch-query","change":"<script>alert(1)</script>","conditions":"seed-42-workload-a","load_parameters":{"available_days":"7"},"penalty":12,"restarted":false,"throughput":321,"p95_ms":9.5,"error_rate":0.01,"hosts":[{"name":"app-2","role":"app","participation":"standby"}]}}`
	r := httptest.NewRequest(http.MethodPost, "/save?score=12345&pass=true", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	var result SaveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, result.SnapshotFile))
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	e := snap.Meta.Experiment
	if e == nil || e.Penalty == nil || *e.Penalty != 12 || e.Restarted == nil || *e.Restarted || e.LoadParameters["available_days"] != "7" || e.ErrorRate == nil || *e.ErrorRate != 0.01 {
		t.Fatalf("metadata not persisted: %+v", e)
	}
	if snap.Meta.BenchmarkPass == nil || !*snap.Meta.BenchmarkPass {
		t.Fatal("lost benchmark pass")
	}
	html := renderReport(t, snap)
	for _, want := range []string{"batch-query", "available_days", "app-2", "standby", "CPU未計測", "&lt;script&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in rendered experiment", want)
		}
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("unescaped metadata")
	}
}

func TestInvalidExperimentCannotEndRun(t *testing.T) {
	for _, body := range []string{
		`{"experiment":{"penalty":-1}}`,
		`{"experiment":{"error_rate":1.1}}`,
		`{"experiment":{"p95_ms":-2}}`,
		`{"experiment":{"unexpected":true}}`,
		`{"experiment":{"restarted":"yes"}}`,
		`{"experiment":{"hosts":[{"name":"a","role":"app","participation":"idle"}]}}`,
		`{"experiment":{"hosts":[{"name":"a","role":"app","participation":"active"},{"name":"a","role":"app","participation":"standby"}]}}`,
		`{} {}`,
		`{"experiment":{"label":"` + strings.Repeat("x", 17<<10) + `"}}`,
	} {
		t.Run(body[:min(55, len(body))], func(t *testing.T) {
			ended := false
			h := NewHandler(Provider{SQL: agg.NewTable(10), DataDir: t.TempDir(), CompleteRun: func(context.Context) (RunFinish, error) { ended = true; return RunFinish{}, nil }})
			r := httptest.NewRequest(http.MethodPost, "/save", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || ended {
				t.Fatalf("status=%d ended=%v", w.Code, ended)
			}
		})
	}
}

func TestExperimentUnknownIsNotZeroOrPass(t *testing.T) {
	values := map[string]string{}
	for _, row := range experimentEvidenceRows(Snapshot{}) {
		values[row.Metric] = row.Value
	}
	for _, metric := range []string{"pass", "penalty (申告)", "負荷パラメータ"} {
		if values[metric] != "未記録" {
			t.Errorf("%s = %q", metric, values[metric])
		}
	}
	if values["HTTPに紐付いたSQL回数"] != "未計測" || values["SQL発行回数 (SQL集計)"] != "未計測" {
		t.Fatal(values)
	}
}

func TestExperimentBodyRequiresJSONBeforeEndingRun(t *testing.T) {
	for _, contentType := range []string{"", "text/plain", "application/json; invalid"} {
		ended := false
		h := NewHandler(Provider{SQL: agg.NewTable(10), DataDir: t.TempDir(), CompleteRun: func(context.Context) (RunFinish, error) { ended = true; return RunFinish{}, nil }})
		r := httptest.NewRequest(http.MethodPost, "/save", strings.NewReader(`{"experiment":{"label":"must-not-be-lost"}}`))
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || ended {
			t.Fatalf("Content-Type %q: status=%d ended=%v", contentType, w.Code, ended)
		}
	}
}

func TestExperimentComparisonConditions(t *testing.T) {
	a := &ExperimentMetadata{Conditions: "workload-v1", LoadParameters: map[string]string{"available_days": "7"}, Hosts: []ExperimentHost{{"app-1", "app", "active"}, {"app-2", "app", "standby"}}}
	b := &ExperimentMetadata{Conditions: "workload-v1", LoadParameters: map[string]string{"available_days": "7"}, Hosts: []ExperimentHost{{"app-2", "app", "standby"}, {"app-1", "app", "active"}}}
	if got := experimentComparisonWarning(a, b); !strings.Contains(got, "一致") || !strings.Contains(got, "未検証") {
		t.Fatal(got)
	}
	b.LoadParameters["available_days"] = "14"
	if got := experimentComparisonWarning(a, b); !strings.Contains(got, "異なります") {
		t.Fatal(got)
	}
	if got := experimentComparisonWarning(nil, b); !strings.Contains(got, "未記録") {
		t.Fatal(got)
	}
	rows := experimentComparison(Snapshot{Meta: Meta{Experiment: a}}, Snapshot{Meta: Meta{Experiment: b}})
	for _, row := range rows {
		if row.Metric == "負荷: available_days" {
			if row.A != "7" || row.B != "14" {
				t.Fatal(row)
			}
			return
		}
	}
	t.Fatal("load parameter missing from comparison")
}

func TestDiffRendersExperimentEvidenceFromSavedRuns(t *testing.T) {
	dir := t.TempDir()
	for i, base := range []string{"20260925-120000_gen1_a", "20260925-121000_gen2_b"} {
		penalty := float64(i * 10)
		snap := Snapshot{Meta: Meta{BenchmarkPass: boolPointer(true), Experiment: &ExperimentMetadata{
			Conditions: "same-seed", Change: "<script>bad</script>", Penalty: &penalty,
			LoadParameters: map[string]string{"available_days": []string{"7", "14"}[i]},
			Hosts:          []ExperimentHost{{Name: "spare-app", Role: "app", Participation: "standby"}},
		}}}
		data, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		writeRunJSON(t, dir, base, string(data))
	}
	h := NewHandler(Provider{SQL: agg.NewTable(10), DataDir: dir})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/diff?a=20260925-120000&b=20260925-121000", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("diff: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"実験条件と結果", "異なります", "available_days", "spare-app", "standby", "penalty", "CPU未計測", "&lt;script&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "<script>bad</script>") {
		t.Fatal("unsafe diff metadata")
	}
}
