package web

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/procstats"
)

func candidateByTitle(candidates []diagnosticCandidate, title string) (diagnosticCandidate, bool) {
	for _, candidate := range candidates {
		if candidate.Title == title {
			return candidate, true
		}
	}
	return diagnosticCandidate{}, false
}

func TestDiagnosticCandidatesOldHTTPDoesNotClaimMissingContext(t *testing.T) {
	snapshot := Snapshot{
		SQL:  []agg.Entry{{Count: 20}},
		HTTP: httpstats.Snapshot{{Count: 10}}, // saved before request SQL fields existed
	}
	candidates := diagnosticCandidates(snapshot)
	got, ok := candidateByTitle(candidates, "SQLのリクエスト紐付けを確認")
	if !ok || !strings.Contains(got.Evidence, "旧snapshot") || !strings.Contains(got.NextStep, "新しいrun") {
		t.Fatalf("old snapshot attribution candidate = %+v", candidates)
	}
	if strings.Contains(got.NextStep, "QueryContext") || strings.Contains(got.Evidence, "20回未追跡") {
		t.Fatalf("old snapshot was misread as missing context: %+v", got)
	}
	if got := diagnosticCandidates(Snapshot{}); len(got) != 0 {
		t.Fatalf("empty snapshot produced diagnosis: %+v", got)
	}
}

func TestDiagnosticCandidatesZeroAttributionSuggestsContextExperiment(t *testing.T) {
	snapshot := Snapshot{
		SQL:  []agg.Entry{{Count: 20}},
		HTTP: httpstats.Snapshot{{Count: 10, SQLTrackedRequests: 10}},
	}
	got, ok := candidateByTitle(diagnosticCandidates(snapshot), "SQLのリクエスト紐付けを確認")
	if !ok || !strings.Contains(got.NextStep, "QueryContext / ExecContext") || !strings.Contains(got.Evidence, "差分を未追跡SQLの正確な件数とは扱えません") {
		t.Fatalf("zero attribution candidate = %+v", got)
	}
	snapshot.HTTP[0].SQLCount = 20
	if got := diagnosticCandidates(snapshot); len(got) != 0 {
		t.Fatalf("reconciled counts produced candidate: %+v", got)
	}
}

func TestDiagnosticCandidatesFrequentShortSQLPrioritizesDSNExperiment(t *testing.T) {
	snapshot := Snapshot{
		SQL: []agg.Entry{{Key: "SELECT id FROM posts WHERE user_id = ?", Count: 120, Avg: 2 * time.Millisecond}},
		Advisor: []advisor.Check{{
			ID: "dsn-interpolate-params", Status: advisor.StatusMissing,
		}},
	}
	got, ok := candidateByTitle(diagnosticCandidates(snapshot), "短いSQLの反復を調査")
	if !ok || !strings.Contains(got.Evidence, "heuristic") || !strings.Contains(got.Evidence, "120回") ||
		!strings.Contains(got.Evidence, "interpolateParams未設定") || !strings.Contains(got.Evidence, "SELECT id FROM posts") || !strings.Contains(got.NextStep, "改善は未測定") {
		t.Fatalf("frequent SQL candidate = %+v", got)
	}
	if strings.Contains(got.Evidence, "N+1です") || strings.Contains(got.NextStep, "往復半減") {
		t.Fatalf("candidate asserted an unmeasured cause or gain: %+v", got)
	}
	snapshot.SQL[0].Count = 99
	if _, ok := candidateByTitle(diagnosticCandidates(snapshot), "短いSQLの反復を調査"); ok {
		t.Fatal("below-threshold SQL count produced frequent SQL candidate")
	}
	snapshot.SQL[0].Count = 120
	snapshot.SQL[0].Avg = 0
	if _, ok := candidateByTitle(diagnosticCandidates(snapshot), "短いSQLの反復を調査"); ok {
		t.Fatal("missing SQL duration produced short SQL candidate")
	}
}

func TestDiagnosticSQLKeyIsBoundedByBytesAndValidUTF8(t *testing.T) {
	key := "SELECT " + strings.Repeat("在", 100)
	got := boundedDiagnosticSQLKey(key)
	if len(got) > 200 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded key is %d bytes, valid UTF-8=%t: %q", len(got), utf8.ValidString(got), got)
	}
	if got := boundedDiagnosticSQLKey("SELECT ?"); got != "SELECT ?" {
		t.Fatalf("short key changed to %q", got)
	}
}

func TestDiagnosticCandidatesBusyProcessOnIdleHost(t *testing.T) {
	snapshot := Snapshot{Proc: &procstats.Snapshot{
		TopCPU:   []procstats.Process{{PID: 42, CPUPercent: 100}},
		CPUTotal: &procstats.CPUTotal{BusyPercent: 27, IdlePercent: 73},
	}}
	got, ok := candidateByTitle(diagnosticCandidates(snapshot), "プロセス負荷とホスト余力を分けて調査")
	if !ok || !strings.Contains(got.Evidence, "PID 42") || !strings.Contains(got.Evidence, "100.0%") || !strings.Contains(got.Evidence, "idle 73.0%") ||
		!strings.Contains(got.NextStep, "他のapp host") {
		t.Fatalf("CPU candidate = %+v", got)
	}
	if strings.Contains(got.Evidence, "CPU飽和") || strings.Contains(got.Evidence, "システム上限です") {
		t.Fatalf("CPU candidate asserted saturation: %+v", got)
	}
	snapshot.Proc.CPUTotal = nil
	if got := diagnosticCandidates(snapshot); len(got) != 0 {
		t.Fatalf("missing host CPU produced candidate: %+v", got)
	}
	snapshot.Proc = nil
	if got := diagnosticCandidates(snapshot); len(got) != 0 {
		t.Fatalf("missing proc produced candidate: %+v", got)
	}
}
