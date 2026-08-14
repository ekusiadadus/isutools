package web

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/procstats"
	"github.com/ekusiadadus/isutools/sqlrows"
)

func TestBottleneckOverviewRanksDemandAndCapacitySignals(t *testing.T) {
	started := time.Unix(100, 0)
	snapshot := Snapshot{
		Meta: Meta{Run: &RunInfo{StartedAt: started, FinishedAt: started.Add(time.Minute)}},
		HTTP: httpstats.Snapshot{
			{Key: "GET /fast HTTP/1.1 200", Count: 10, Total: time.Second, Avg: 100 * time.Millisecond, P95: 128 * time.Millisecond},
			{Key: "GET /api/app/notification HTTP/1.1 200", Count: 1000, Total: 12 * time.Second, Avg: 12 * time.Millisecond, P95: 16 * time.Millisecond},
		},
		SQL: []agg.Entry{
			{Key: "SELECT small", Count: 10, Total: time.Second, Avg: 100 * time.Millisecond, P95: 128 * time.Millisecond},
			{Key: "SELECT status FROM ride_statuses", Count: 2000, Total: 9 * time.Second, Avg: 4500 * time.Microsecond, P95: 8 * time.Millisecond},
		},
		AccessLog: &accesslog.Snapshot{Entries: []accesslog.Entry{{Count: 1000, Status5xx: 3, Status499: 1}}},
		Proc: &procstats.Snapshot{
			StartedAt: started.Add(time.Second), EndedAt: started.Add(time.Minute - time.Second),
			CPUTotal: &procstats.CPUTotal{BusyPercent: 24, UserPercent: 18, SystemPercent: 6, IdlePercent: 76},
		},
		DBPool: []dbpool.Entry{{TargetID: "app", MaxOpen: 100, Open: 71}},
		SQLRows: &sqlrows.Section{Targets: []sqlrows.TargetSection{{
			TargetID: "app", Usable: true, Digests: []sqlrows.DigestStat{{
				Query: "SELECT nearby chairs", TotalTime: 4 * time.Second, RowsExamined: 6400, RowsSent: 10,
				HasRatio: true, ExaminedPerSent: 640,
			}},
		}}},
	}

	rows := bottleneckOverview(snapshot)
	bySignal := make(map[string]bottleneckSignal, len(rows))
	for _, row := range rows {
		bySignal[row.Signal] = row
	}
	if got := bySignal["HTTP demand"]; got.Order != "1" || !strings.Contains(got.Evidence, "/api/app/notification") {
		t.Fatalf("HTTP signal = %+v, want the largest total demand first", got)
	}
	if got := bySignal["SQL demand"]; got.Order != "2" || !strings.Contains(got.Evidence, "ride_statuses") {
		t.Fatalf("SQL signal = %+v, want the largest total demand second", got)
	}
	if got := bySignal["unserved / failed"]; got.Level != "hot" || !strings.Contains(got.Evidence, "5xx 3") {
		t.Fatalf("failure signal = %+v, want observed proxy failures", got)
	}
	if got := bySignal["CPU"]; got.Level != "ok" || !strings.Contains(got.NextAction, "余力") {
		t.Fatalf("CPU signal = %+v, want low run-aligned CPU to redirect the investigation", got)
	}
	if got := bySignal["DB pool"]; got.Level != "ok" || !strings.Contains(got.Evidence, "waits 0") {
		t.Fatalf("DB pool signal = %+v, want zero waits reported", got)
	}
	if got := bySignal["SQL row efficiency"]; got.Level != "hot" || !strings.Contains(got.Evidence, "640.0") {
		t.Fatalf("row-efficiency signal = %+v, want the costly inefficient digest", got)
	}
}

func TestBottleneckOverviewRejectsCPUFromWrongInterval(t *testing.T) {
	started := time.Unix(1000, 0)
	snapshot := Snapshot{
		Meta: Meta{Run: &RunInfo{StartedAt: started, FinishedAt: started.Add(time.Minute)}},
		Proc: &procstats.Snapshot{
			StartedAt: started.Add(-time.Hour), EndedAt: started.Add(time.Minute),
			CPUTotal: &procstats.CPUTotal{BusyPercent: 5, IdlePercent: 95},
		},
	}

	rows := bottleneckOverview(snapshot)
	if len(rows) != 1 || rows[0].Signal != "CPU interval" || rows[0].Level != "warn" || !strings.Contains(rows[0].Evidence, "区間不一致") {
		t.Fatalf("overview = %+v, want stale CPU labeled unjudgeable", rows)
	}
	if strings.Contains(rows[0].NextAction, "余力") {
		t.Fatalf("stale CPU was interpreted as spare capacity: %+v", rows[0])
	}
}

func TestReportRendersBottleneckOverviewBeforeCollectorHealth(t *testing.T) {
	snapshot := Snapshot{HTTP: httpstats.Snapshot{{Key: "GET /hot HTTP/1.1 200", Count: 2, Total: time.Second}}}
	var output bytes.Buffer
	if err := reportTmpl.Execute(&output, page{Snapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	overview := strings.Index(html, "Bottleneck Overview")
	health := strings.Index(html, "Collector Health")
	if overview < 0 || health < 0 || overview >= health || !strings.Contains(html, "GET /hot HTTP/1.1 200") {
		t.Fatalf("report did not render the overview first: %s", html[:min(len(html), 1200)])
	}
}

func TestDiagnosisSummarySeparatesFixCandidateFromCodeEvidence(t *testing.T) {
	started := time.Unix(100, 0)
	snapshot := Snapshot{
		Meta: Meta{
			Run: &RunInfo{StartedAt: started, FinishedAt: started.Add(time.Minute)},
			Profiles: &ProfileManifest{
				RunID: "run-1",
				Expected: []ProfileExpectation{{
					Kind: "cpu", Mode: "interval",
					Inputs: []ProfileExpectedInput{{Kind: "cpu", Point: "interval", File: "run_cpu.pprof"}},
				}},
			},
		},
		HTTP: httpstats.Snapshot{{
			Key: "GET /api/app/notification HTTP/1.1 200", Count: 1200,
			Total: 20 * time.Minute, Avg: time.Second, P95: 1100 * time.Millisecond,
		}},
		SQL: []agg.Entry{{
			Key:   "SELECT * FROM rides WHERE user_id = ? ORDER BY created_at DESC",
			Count: 900, Total: 9 * time.Second,
		}},
		DBPool: []dbpool.Entry{{
			TargetID: "app", MaxOpen: 100, Open: 100,
			WaitCount: 59001, WaitDuration: 874 * time.Second,
		}},
	}

	diagnosis := diagnoseBottleneck(snapshot)
	if diagnosis.PrimaryLevel != "hot" ||
		!strings.Contains(diagnosis.Primary, "DB接続プール") ||
		!strings.Contains(diagnosis.PrimaryEvidence, "59,001") {
		t.Fatalf("primary diagnosis = %+v, want pool saturation first", diagnosis)
	}
	if !strings.Contains(diagnosis.Amplifier, "long-poll候補") ||
		!strings.Contains(diagnosis.HTTPSearchKey, "/api/app/notification") {
		t.Fatalf("amplifier diagnosis = %+v, want notification demand with search key", diagnosis)
	}
	if diagnosis.CodeLevel != "warn" ||
		!strings.Contains(diagnosis.CodeEvidence, "CPU profileがありません") ||
		!strings.Contains(diagnosis.CodeEvidence, "行番号を断定できません") {
		t.Fatalf("code evidence = %+v, want explicit missing-profile boundary", diagnosis)
	}
	if !strings.Contains(diagnosis.SQLSearchKey, "SELECT * FROM rides") {
		t.Fatalf("SQL search key = %q", diagnosis.SQLSearchKey)
	}
}

func TestReportPutsDecisionAndCodeEvidenceBeforeDenseTables(t *testing.T) {
	snapshot := Snapshot{
		HTTP: httpstats.Snapshot{{Key: "GET /hot HTTP/1.1 200", Count: 2, Total: time.Second}},
		Meta: Meta{Profiles: &ProfileManifest{RunID: "run-without-cpu"}},
	}
	body := renderReport(t, snapshot)
	decision := strings.Index(body, "結論: 次に修正する場所")
	overview := strings.Index(body, "Bottleneck Overview")
	if decision < 0 || overview < 0 || decision >= overview {
		t.Fatalf("decision must precede evidence tables: decision=%d overview=%d", decision, overview)
	}
	for _, want := range []string{
		`id="diagnosis"`, `data-target="http"`, `data-target="sql"`, `data-target="profiles"`,
		"CPU profileがありません", "ソースコードの行番号を断定できません",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decision section is missing %q", want)
		}
	}
}

func TestPublishedCPUArtifactDiagnosisStaysTrueAfterDerivedAnalysis(t *testing.T) {
	manifest := &ProfileManifest{CPU: &CPUIntervalCapture{
		Status: "published",
		File:   "cpu_verified.pprof",
	}}
	level, title, evidence, action := codeEvidence(manifest)
	if level != "ok" || title != "コード位置: CPU artifactあり" {
		t.Fatalf("code evidence heading = %q/%q", level, title)
	}
	if strings.Contains(title+evidence+action, "解析待ち") {
		t.Fatalf("published diagnosis becomes stale after derived analysis: %q / %q / %q", title, evidence, action)
	}
	for _, want := range []string{"artifactだけでは", "行解析結果", "isutools-pprof"} {
		if !strings.Contains(evidence+action, want) {
			t.Errorf("code evidence is missing %q: %q / %q", want, evidence, action)
		}
	}
}
