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
