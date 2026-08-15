package web

import (
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/internal/timeline"
	"github.com/ekusiadadus/isutools/netstats"
	"github.com/ekusiadadus/isutools/queryplan"
	"github.com/ekusiadadus/isutools/sqlrows"
)

// The fixtures below mirror a real private-isu benchmark capture: the field
// mix (which values are populated and which stay zero) is what the renderer
// actually has to survive, so inventing a fully-populated section would test
// a page that never occurs.

const (
	slowQuery    = "SELECT slow_marker FROM comments WHERE post_id = ?"
	tidyQuery    = "SELECT tidy_marker FROM users WHERE id = ?"
	emptyQuery   = "SELECT empty_marker FROM posts WHERE id = ?"
	insertQuery  = "INSERT INTO comments (post_id, user_id, comment) VALUES (?, ?, ?)"
	skippedTgt   = "mysql://db2/isuconp"
	skipCode     = "probe-skip"
	skipReason   = "performance_schema is off"
	poolEndpoint = "tcp(127.0.0.1:3306)/isuconp"
)

func float64Ptr(v float64) *float64 { return &v }

func uint64Ptr(v uint64) *uint64 { return &v }

func TestBarrierWindow(t *testing.T) {
	start := time.Date(2026, 8, 14, 0, 13, 11, 792751000, time.UTC)
	got := barrierWindow([2]time.Time{start, start.Add(1844 * time.Microsecond)})
	if !strings.Contains(got, "00:13:11.792751Z") || !strings.Contains(got, "1.8 ms") {
		t.Fatalf("barrierWindow=%q", got)
	}
	if got := barrierWindow([2]time.Time{}); got != "-" {
		t.Fatalf("empty barrierWindow=%q", got)
	}
}

// fullSnapshot is a snapshot carrying all four new sections.
func fullSnapshot() Snapshot {
	baseline := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	final := baseline.Add(72500 * time.Millisecond)
	return Snapshot{
		SQLRows: &sqlrows.Section{
			Limit: 200,
			Targets: []sqlrows.TargetSection{
				{
					TargetID: "mysql://db1/isuconp",
					Schema:   "isuconp",
					Usable:   true,
					Shown:    27,
					Total:    27,
					Dropped:  3,
					Digests: []sqlrows.DigestStat{
						// Deliberately not in total-time order: the template
						// must impose the order itself.
						{
							Digest: "d2", Query: tidyQuery, Kind: sqlrows.KindSelect,
							Count: 500, TotalTime: 40 * time.Millisecond,
							RowsExamined: 300, RowsSent: 100,
							ExaminedPerSent: 3, HasRatio: true,
						},
						{
							Digest: "d3", Query: emptyQuery, Kind: sqlrows.KindSelect,
							Count: 12, TotalTime: 3 * time.Millisecond,
							RowsExamined: 9000, RowsSent: 0,
						},
						{
							Digest: "d4", Query: insertQuery, Kind: sqlrows.KindDML,
							Count: 80, TotalTime: 2 * time.Millisecond,
							RowsExamined: 0, RowsSent: 0, RowsAffected: 80,
						},
						{
							Digest: "d1", Query: slowQuery, Kind: sqlrows.KindSelect,
							Count: 1200, TimerWaitPicos: 2500000000000,
							TotalTime:    2500 * time.Millisecond,
							RowsExamined: 900, RowsSent: 100,
							ExaminedPerSent: 9, HasRatio: true,
							NoIndexUsed: 1200, SortMergePasses: 4,
							CreatedTmpDiskTables: 2,
						},
					},
				},
				{
					TargetID: skippedTgt,
					Usable:   false,
					Code:     skipCode,
					Reason:   skipReason,
				},
			},
			Health: []sqlrows.HealthNote{{
				Key: sqlrows.HealthSkip, Message: skippedTgt + " (" + skipReason + ")",
			}},
		},
		Host: &hoststats.Section{
			Identity: hoststats.Identity{
				Hostname: "isu-app-1", BootIDHash: "a1b2c3d4e5f60718",
				PIDNS: "pid:[4026531836]", NetNS: "net:[4026531840]",
				MntNS: "mnt:[4026531841]", CgroupNS: "cgroup:[4026531835]",
				AgentVersion: "v1.2.0",
			},
			Interval: hoststats.Interval{BaselineAt: baseline, FinalAt: final, Seconds: 72.5},
			Memory: hoststats.Memory{
				TotalBytes:        20969086976,
				AvailableBaseline: 18000000000,
				AvailableFinal:    15000000000,
				CachedBaseline:    2095624192,
				CachedFinal:       3481604096,
				DirtyBaseline:     1048576,
				DirtyFinal:        5242880,
			},
			Disks: []hoststats.Disk{{
				Device: "vda", ReadBytes: 1073741824, WriteBytes: 2147483648,
				ReadMBPerSec: float64Ptr(14.8), WriteMBPerSec: float64Ptr(29.6),
				IOTimeMillis: 12000, UtilPercent: float64Ptr(16.6),
				QueueAvg: float64Ptr(0.4),
			}},
			PSI: &hoststats.PSI{
				CPU: hoststats.PSIResource{SomeAvg10: 3.2, SomeAvg60: 1.1,
					SomeStallRatio: float64Ptr(2.5)},
				IO: hoststats.PSIResource{SomeAvg10: 0.5},
			},
			Filesystems: []hoststats.FSUsage{{
				Path: "/", TotalBytes: 107374182400,
				AvailBaseline: 53687091200, AvailFinal: 52613349376,
			}},
			CGroup: &hoststats.CGroup{
				Scope: hoststats.ScopeVisibleRoot, Path: "/",
				CPUMaxCores: float64Ptr(4), MemoryMaxBytes: uint64Ptr(8589934592),
				MemoryCurrentBaseline: 1073741824, MemoryCurrentFinal: 2147483648,
			},
		},
		Network: &netstats.NetworkStats{
			TCP: netstats.TCPSummary{InUse: 14, TimeWait: 1984, Orphan: 0, InUse6: 154},
			Interfaces: []netstats.Interface{{
				Name: "eth0", RxBytes: 1449551462, TxBytes: 1862270054,
				RxPackets: 3211055, TxPackets: 2903118,
				RxErrors: 0, TxErrors: 0, RxDropped: 7, TxDropped: 0,
				RxMbitPerSec: float64Ptr(159.9), TxMbitPerSec: float64Ptr(205.5),
				SpeedMbit: 10000, MTU: 1500,
			}},
		},
		DBPool: []dbpool.Entry{{
			TargetID: "mysql://db1/isuconp", Display: poolEndpoint,
			MaxOpen: 24, Open: 24, InUse: 19, Idle: 5,
			WaitCount: 4, WaitDuration: 2 * time.Second,
			MaxIdleClosed: 130, MaxIdleTimeClosed: 0, MaxLifetimeClosed: 11,
			BaselineAt: baseline, FinalAt: final,
		}},
		QueryPlan: fullQueryPlanSection(),
	}
}

// The query-plan fixture mirrors one enrich phase of a private-isu run: a
// full scan that also sorts, an index lookup whose sample predates the run, a
// digest whose EXPLAIN never made it, and a target that was skipped whole.
const (
	planScanQuery  = "SELECT scan_marker FROM comments WHERE post_id = ?"
	planStaleQuery = "SELECT stale_marker FROM users WHERE id = ?"
	planErrQuery   = "SELECT failed_marker FROM posts WHERE user_id = ?"
	planTargetID   = "mysql://db1/isuconp"
	// A target of its own: the sqlrows fixture already renders db2, and the
	// point of the assertion below is that a plan-less target is absent from
	// the query-plan tables specifically.
	planSkippedTgt = "mysql://db3/isuconp"
)

func stringPtr(v string) *string { return &v }

func int64Ptr(v int64) *int64 { return &v }

func fullQueryPlanSection() *queryplan.Section {
	seen := time.Date(2026, 8, 4, 12, 0, 30, 0, time.UTC)
	return &queryplan.Section{
		Top: queryplan.DefaultTop,
		Targets: []queryplan.TargetSection{
			{
				TargetID: planTargetID, Schema: "isuconp", Explained: true,
				Plans: []queryplan.Plan{
					{
						Digest: "p1", Query: planScanQuery, SampleSeen: seen,
						Freshness: queryplan.FreshnessFresh, FreshReason: queryplan.FreshInInterval,
						Rows: []queryplan.PlanRow{{
							SelectType: stringPtr("SIMPLE"), Table: stringPtr("comments"),
							Type: stringPtr("ALL"), Rows: int64Ptr(20000),
							Extra: stringPtr("Using where; Using filesort"),
						}},
					},
					{
						Digest: "p2", Query: planStaleQuery, SampleSeen: seen.Add(-time.Hour),
						Freshness: queryplan.FreshnessStale, FreshReason: queryplan.FreshBeforeInterval,
						Rows: []queryplan.PlanRow{{
							SelectType: stringPtr("SIMPLE"), Table: stringPtr("users"),
							Type: stringPtr("ref"), Key: stringPtr("PRIMARY"),
							PossibleKeys: stringPtr("PRIMARY"), Rows: int64Ptr(1),
						}},
					},
					{
						Digest: "p3", Query: planErrQuery,
						Freshness: queryplan.FreshnessFresh, FreshReason: queryplan.FreshInInterval,
						Err: &queryplan.PlanError{Class: queryplan.PlanErrTimeout, Errno: 3024},
					},
				},
			},
			{
				TargetID: planSkippedTgt,
				Code:     queryplan.CodePurposeUnregistered,
				Reason:   "EXPLAIN 専用 credential が未登録です",
			},
		},
		Health: []queryplan.HealthNote{{
			Key: queryplan.CodePurposeUnregistered, Message: "queryplan[" + planSkippedTgt + "]: skip",
		}},
	}
}

func renderReport(t *testing.T, snap Snapshot) string {
	t.Helper()
	var out strings.Builder
	if err := reportTmpl.Execute(&out, page{Snapshot: snap, Sortable: true}); err != nil {
		t.Fatalf("render: %v", err)
	}
	return out.String()
}

// rowContaining returns the table row that holds marker, so an assertion can
// be made about that row rather than about the page as a whole.
func rowContaining(t *testing.T, body, marker string) string {
	t.Helper()
	at := strings.Index(body, marker)
	if at < 0 {
		t.Fatalf("marker %q is not rendered", marker)
	}
	start := strings.LastIndex(body[:at], "<tr")
	end := strings.Index(body[at:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatalf("no table row around %q", marker)
	}
	return body[start : at+end]
}

func TestReportRendersAllRunSections(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	for _, want := range []string{
		"<h2>SQL 行効率",
		"<h2>Host",
		"<h2>Network",
		"<h2>DB Pool",
		// sqlrows: truncation counters, the skipped target and a digest row
		"shown 27 / total 27",
		"dropped 3",
		slowQuery,
		"2.5 s",
		// hoststats: interval, memory movement, disk, PSI, filesystem, cgroup
		"72.5s",
		"+1.3 GB",
		"19.5 GB",
		"14.8",
		hoststats.DiskUtilNote,
		hoststats.CGroupScopeNote,
		"isu-app-1",
		"cgroup:[4026531835]",
		// netstats: TCP summary, the caveat, and the interface row
		"time_wait 1984",
		"in_use6 154",
		"eth0",
		"159.9",
		"205.5",
		"10000",
		"1500",
		// dbpool: counters and the derived average wait
		poolEndpoint,
		"500.0 ms",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

func TestReportNotesTimeWaitDirectionIsUnknown(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	if !strings.Contains(body, "time_wait は inbound と outbound を区別せず") {
		t.Error("the TIME_WAIT direction caveat must be rendered next to the TCP summary")
	}
}

func TestReportLabelsPoolWaitAsSumAcrossWaiters(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	for _, want := range []string{"待ち時間の総和", "経過時間ではありません", "平均 wait"} {
		if !strings.Contains(body, want) {
			t.Errorf("DB Pool section must explain wait_duration; missing %q", want)
		}
	}
	row := rowContaining(t, body, poolEndpoint)
	if !strings.Contains(row, "2.0 s") || !strings.Contains(row, "500.0 ms") {
		t.Errorf("pool row must show the summed wait and the derived average: %s", row)
	}
}

func TestReportFlagsHighExaminedPerSentRatio(t *testing.T) {
	body := renderReport(t, fullSnapshot())

	hot := rowContaining(t, body, slowQuery)
	if !strings.Contains(hot, `class="hot"`) {
		t.Errorf("a ratio above %.0f must be visually flagged: %s", sqlRowsRatioWarn, hot)
	}
	if !strings.Contains(hot, `class="flag"`) {
		t.Errorf("the ratio cell of a hot row must be flagged: %s", hot)
	}
	if !strings.Contains(hot, "9.0") {
		t.Errorf("hot row must show its ratio: %s", hot)
	}

	tidy := rowContaining(t, body, tidyQuery)
	if strings.Contains(tidy, "hot") {
		t.Errorf("a ratio of 3 is within the target and must not be flagged: %s", tidy)
	}
}

func TestReportFlagsIndexAndSortSignals(t *testing.T) {
	snap := fullSnapshot()
	// Strip the ratio so only the index/sort signals can flag the row.
	digests := snap.SQLRows.Targets[0].Digests
	for i := range digests {
		if digests[i].Query == slowQuery {
			digests[i].HasRatio = false
			digests[i].ExaminedPerSent = 0
		}
	}
	row := rowContaining(t, renderReport(t, snap), slowQuery)
	if !strings.Contains(row, `class="hot"`) {
		t.Errorf("no_index_used / sort_merge_passes / tmp disk tables must flag the row: %s", row)
	}
}

func TestReportShowsRatioNotAvailableWithoutSentRows(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	for _, marker := range []string{emptyQuery, insertQuery} {
		row := rowContaining(t, body, marker)
		if !strings.Contains(row, "N/A") {
			t.Errorf("ratio must read N/A when it is undefined: %s", row)
		}
		if strings.Contains(row, "+Inf") || strings.Contains(row, "NaN") {
			t.Errorf("ratio must never be a division by zero: %s", row)
		}
	}
}

func TestReportSortsDigestsByTotalTimeDescending(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	slow := strings.Index(body, slowQuery)
	tidy := strings.Index(body, tidyQuery)
	if slow < 0 || tidy < 0 {
		t.Fatal("digest rows are missing")
	}
	if slow > tidy {
		t.Error("digests must be ordered by total time descending")
	}
}

func TestReportOmitsCountsForUnusableTarget(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	at := strings.Index(body, skippedTgt)
	if at < 0 {
		t.Fatal("a skipped target must still be listed")
	}
	line := body[at:]
	if end := strings.Index(line, "</p>"); end >= 0 {
		line = line[:end]
	}
	if strings.Contains(line, "shown") {
		t.Errorf("a target that contributes no numbers must not print counts: %s", line)
	}
}

func TestReportRatesAreSortableAndAbsentRatesReadAsUnknown(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	if !strings.Contains(body, `data-v="159.9"`) {
		t.Error("a derived rate must carry a numeric sort key")
	}

	snap := fullSnapshot()
	// An interface that appeared mid-run has no derivable rate. Zero would
	// read as "idle" when the truth is "unknown".
	snap.Network.Interfaces[0].Appeared = true
	snap.Network.Interfaces[0].RxMbitPerSec = nil
	snap.Network.Interfaces[0].TxMbitPerSec = nil
	row := rowContaining(t, renderReport(t, snap), "eth0")
	if strings.Contains(row, "159.9") || strings.Contains(row, "0.0") {
		t.Errorf("an underivable rate must not render as a number: %s", row)
	}
	if !strings.Contains(row, "<td>-</td>") {
		t.Errorf("an underivable rate must render as unknown: %s", row)
	}
	if !strings.Contains(row, "appeared mid-run") {
		t.Errorf("an interface absent at the baseline must say so: %s", row)
	}
}

func TestReportExplainsUnusableSQLRowsTarget(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	at := strings.Index(body, skippedTgt)
	if at < 0 {
		t.Fatal("a skipped target must still be listed")
	}
	rest := body[at:]
	for _, want := range []string{skipCode, skipReason} {
		if !strings.Contains(rest, want) {
			t.Errorf("a skipped target must explain itself; missing %q", want)
		}
	}
}

// TestReportRendersQueryPlans covers the section's whole job: the EXPLAIN
// columns, the highlighting of the three access-path defects, a NULL column
// rendered as an em dash, and a sample the run cannot vouch for greyed out
// with the reason it cannot.
func TestReportRendersQueryPlans(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	if !strings.Contains(body, "<h2>Query Plans") {
		t.Fatal("a captured section must render a Query Plans heading")
	}
	for _, want := range []string{planScanQuery, planStaleQuery, planErrQuery, "isuconp"} {
		if !strings.Contains(body, want) {
			t.Errorf("query plans are missing %q", want)
		}
	}

	scan := rowContaining(t, body, planScanQuery)
	for _, want := range []string{
		`class="hot"`,    // the row carries a defect
		`class="l flag"`, // ... on the cells that hold the evidence
		"ALL",            // type
		"20000",          // rows
		"Using filesort", // Extra
		"comments",       // table
		"計測区間内",          // freshness
		planNullCell,     // key and possible_keys are NULL here
	} {
		if !strings.Contains(scan, want) {
			t.Errorf("full-scan row = %q, want it to contain %q", scan, want)
		}
	}

	stale := rowContaining(t, body, planStaleQuery)
	if !strings.Contains(stale, `class="stale"`) {
		t.Errorf("a sample from outside the interval must be greyed out: %q", stale)
	}
	if !strings.Contains(stale, "区間より前") {
		t.Errorf("a stale row must say why it is stale: %q", stale)
	}
	if strings.Contains(stale, `class="hot"`) {
		t.Errorf("a plan the run cannot vouch for must not be highlighted: %q", stale)
	}

	failed := rowContaining(t, body, planErrQuery)
	if !strings.Contains(failed, "タイムアウト") {
		t.Errorf("a digest with no plan must say why: %q", failed)
	}

	// A NULL column is an em dash, never Go's rendering of a nil pointer.
	if strings.Contains(body, "<nil>") {
		t.Error("a NULL EXPLAIN column leaked as <nil>")
	}
	// A target that produced no plan gets no table; its reason travels through
	// the Collector Health section instead.
	if strings.Contains(body, planSkippedTgt) {
		t.Error("a target with no plans must not render a query-plan table")
	}
	// The two rules the reader has to know to use the section at all.
	for _, want := range []string{
		"画面を開いても再実行はしません",
		"advisor の判定対象からは外しています",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("query plans are missing the note %q", want)
		}
	}
}

func TestReportOmitsQueryPlanSectionWithoutPlans(t *testing.T) {
	// Capture ran and every target was skipped. A heading over an empty table
	// would read as "these statements have no plan", which is not what
	// happened, so the section renders nothing at all.
	body := renderReport(t, Snapshot{QueryPlan: &queryplan.Section{
		Top: queryplan.DefaultTop,
		Targets: []queryplan.TargetSection{{
			TargetID: planTargetID,
			Code:     queryplan.CodePurposeUnregistered,
			Reason:   "EXPLAIN 専用 credential が未登録です",
		}},
	}})
	if strings.Contains(body, "Query Plans") {
		t.Error("a capture with no plans must render no section")
	}
}

// TestReportProfilesAlwaysDeclareTheApproximation fixes the wording plan 07
// requires: a pair is a difference of process-wide cumulative profiles, and a
// reader who is not told so will read it as the run.
func TestReportProfilesAlwaysDeclareTheApproximation(t *testing.T) {
	snap := fullSnapshot()
	snap.Meta.Profiles = &ProfileManifest{
		RunID: "run-a1b2c3d4", Validity: "valid",
		Captures: []ProfileCapture{
			{Kind: "mutex", Point: ProfilePointOpen, Status: profileStatusOK,
				File: "20260804-120000_gen7_run-a1b2_mutex_open.pprof", LagFromRefNs: 41_000_000},
			{Kind: "block", Point: ProfilePointClose, Status: profileStatusSkipped,
				Code: profileCodeLeaseExceeded},
		},
		Pairs: []ProfilePair{{
			Kind:      "mutex",
			OpenFile:  "20260804-120000_gen7_run-a1b2_mutex_open.pprof",
			CloseFile: "20260804-120000_gen7_run-a1b2_mutex_close.pprof",
			OpenGate:  openGatePostStartReturn,
			RunSpanNs: 59_676_000_000, HeadLossNs: 41_000_000,
			TailExcessNs: 2_118_000_000, ApproxErrorNs: 2_159_000_000,
			DiffCommand: "go tool pprof -base open.pprof close.pprof",
		}},
	}
	body := renderReport(t, snap)
	for _, want := range []string{
		"<h2>Profiles",
		"run 冒頭の",
		"finish freeze 後の",
		"run 単位のプロファイルではありません",
		"欠落 41ms・余剰 2.118s",
		"go tool pprof -base",
		// A capture that never happened is reported with its code rather than
		// left out, and a lagging pair is badged.
		profileCodeLeaseExceeded,
		"⚠ 採取遅延",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("profiles section is missing %q", want)
		}
	}
}

func TestReportOmitsAbsentRunSections(t *testing.T) {
	// A darwin run has no hoststats and no netstats at all, and a run without
	// a database has neither sqlrows nor dbpool. None of them may leave a
	// heading behind.
	body := renderReport(t, Snapshot{})
	for _, unwanted := range []string{
		"SQL 行効率", "<h2>Host", "<h2>Network", "<h2>DB Pool",
		"Query Plans", "<h2>Profiles",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("absent section left %q on the page", unwanted)
		}
	}
}

func TestReportOmitsEmptySQLRowsSection(t *testing.T) {
	body := renderReport(t, Snapshot{SQLRows: &sqlrows.Section{Limit: 200}})
	if strings.Contains(body, "SQL 行効率") {
		t.Error("an sqlrows section with no targets and no health must render nothing")
	}
}

func TestReportRendersTraceableTimelineAndInsufficientEvidenceFallback(t *testing.T) {
	start := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rule := timeline.Rule{ID: timeline.PhaseErrorOnset, Formula: "previous errors == 0 and current errors > 0", Limitation: "observed HTTP only"}
	section := &timeline.Section{
		Schema: timeline.SchemaV1, RunID: "run-timeline", Epoch: 7, IntervalNs: int64(time.Second), MaxBuckets: 10,
		Buckets: []timeline.Bucket{{Index: 1, Start: start, End: start.Add(time.Second)}},
		Analysis: timeline.Analysis{Available: true, Rules: []timeline.Rule{rule},
			Phases: []timeline.Phase{{Kind: timeline.PhaseErrorOnset, RuleID: rule.ID, WindowStart: start, WindowEnd: start.Add(time.Second), Evidence: []timeline.EvidenceRef{{
				BucketIndex: 1, WindowStart: start, WindowEnd: start.Add(time.Second), Signal: "http", Metric: "error_count", Value: 3,
				Formula: rule.Formula, Limitation: rule.Limitation,
			}}}},
			Suspects: []timeline.Suspect{{Signal: "low-volume critical-path candidate", Key: "POST /gate", Kind: "http", Label: "correlation-suspect", Score: 100, Evidence: []timeline.EvidenceRef{{
				BucketIndex: 0, WindowStart: start.Add(-time.Second), WindowEnd: start, Signal: "http:POST /gate", Metric: "p95_ns", Value: float64(200 * time.Millisecond),
				Formula: "count <= 20% of busiest operation", Limitation: "correlation, not causality",
			}}}},
		},
	}
	body := renderReport(t, Snapshot{Timeline: section})
	for _, want := range []string{"Run Timeline", "error-onset", "error_count", "previous errors == 0 and current errors &gt; 0", rule.Limitation, "correlation-suspect", "POST /gate", "p95_ns", "correlation, not causality"} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline report missing %q", want)
		}
	}
	if !strings.Contains(body, "<details><summary>時系列の詳細") {
		t.Fatalf("dense timeline evidence must be collapsed behind a summary")
	}

	section.Analysis = timeline.Analysis{Available: false, Reason: timeline.ReasonInsufficientBuckets}
	body = renderReport(t, Snapshot{Timeline: section})
	if !strings.Contains(body, "time-aware analysis unavailable: "+timeline.ReasonInsufficientBuckets) ||
		!strings.Contains(body, "Aggregate SQL/HTTP/resource tables remain authoritative") {
		t.Fatalf("timeline fallback missing: %s", body)
	}
}

func TestReportWithoutTimelineKeepsAggregateFallbackAndStatesUnavailable(t *testing.T) {
	body := renderReport(t, Snapshot{})
	for _, want := range []string{
		"Run Timeline", "time-aware analysis unavailable: timeline not captured",
		"Aggregate SQL/HTTP/resource tables remain authoritative", "ISUTOOLS_TIMELINE=1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("timeline-disabled fallback missing %q", want)
		}
	}
}

func TestReportWithRunSectionsStaysSelfContained(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	for _, forbidden := range []string{`href="http://`, `href="https://`, `href="//`, `src=`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the report must stay self-contained, found %q", forbidden)
		}
	}
}

// TestPlanLines pins the decisions the Query Plans table is built out of,
// away from the template syntax that renders them.
func TestPlanLines(t *testing.T) {
	fresh := queryplan.Plan{
		Digest: "d", Query: "SELECT 1",
		Freshness: queryplan.FreshnessFresh, FreshReason: queryplan.FreshInInterval,
	}
	tests := []struct {
		name  string
		plan  queryplan.Plan
		want  planLine
		lines int
	}{
		{
			name: "full scan on a fresh sample is highlighted",
			plan: func() queryplan.Plan {
				p := fresh
				p.Rows = []queryplan.PlanRow{{Type: stringPtr("ALL"), Rows: int64Ptr(4200)}}
				return p
			}(),
			lines: 1,
			want: planLine{
				Freshness: "計測区間内", Fresh: true, FullScan: true,
				Type: "ALL", Rows: "4200", RowsSort: 4200,
				SelectType: planNullCell, Table: planNullCell,
				Key: planNullCell, PossibleKeys: planNullCell, Extra: planNullCell,
			},
		},
		{
			name: "the same plan on a stale sample is not",
			plan: queryplan.Plan{
				Digest: "d", Query: "SELECT 1",
				Freshness: queryplan.FreshnessStale, FreshReason: queryplan.FreshAfterInterval,
				Rows: []queryplan.PlanRow{{
					Type: stringPtr("ALL"), Rows: int64Ptr(4200),
					Extra: stringPtr("Using temporary; Using filesort"),
				}},
			},
			lines: 1,
			want: planLine{
				Freshness: "区間より後", Fresh: false,
				Type: "ALL", Rows: "4200", RowsSort: 4200,
				Extra:      "Using temporary; Using filesort",
				SelectType: planNullCell, Table: planNullCell,
				Key: planNullCell, PossibleKeys: planNullCell,
			},
		},
		{
			name: "an unjudgeable sample carries the reason it could not be judged",
			plan: queryplan.Plan{
				Digest: "d", Query: "SELECT 1",
				Freshness:   queryplan.FreshnessUnknown,
				FreshReason: queryplan.FreshClockAnomaly,
			},
			lines: 1,
			want: planLine{
				Freshness: "DB 時計異常のため判定不能", Note: "実行計画の行なし",
				RowsSort: -1, Rows: planNullCell, Type: planNullCell,
				SelectType: planNullCell, Table: planNullCell,
				Key: planNullCell, PossibleKeys: planNullCell, Extra: planNullCell,
			},
		},
		{
			name: "a failed EXPLAIN reports its class, never a driver message",
			plan: func() queryplan.Plan {
				p := fresh
				p.Err = &queryplan.PlanError{Class: queryplan.PlanErrPermission, Errno: 1142}
				return p
			}(),
			lines: 1,
			want: planLine{
				Freshness: "計測区間内", Fresh: true, Note: "権限不足",
				RowsSort: -1, Rows: planNullCell, Type: planNullCell,
				SelectType: planNullCell, Table: planNullCell,
				Key: planNullCell, PossibleKeys: planNullCell, Extra: planNullCell,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := planLines([]queryplan.Plan{tt.plan})
			if len(lines) != tt.lines {
				t.Fatalf("lines = %d, want %d", len(lines), tt.lines)
			}
			got := lines[0]
			// The statement travels with every line so a sorted table cannot
			// separate a plan row from the query it belongs to.
			if got.Query != tt.plan.Query || got.Digest != tt.plan.Digest {
				t.Errorf("line identity = (%q, %q), want (%q, %q)",
					got.Query, got.Digest, tt.plan.Query, tt.plan.Digest)
			}
			got.Query, got.Digest = "", ""
			if got != tt.want {
				t.Errorf("line = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestPlanLinesFlattensEveryExplainRow keeps a multi-row plan whole: a join
// produces one EXPLAIN row per table, and dropping any of them would hide the
// table that is actually being scanned.
func TestPlanLinesFlattensEveryExplainRow(t *testing.T) {
	lines := planLines([]queryplan.Plan{{
		Digest: "d", Query: "SELECT 1", Freshness: queryplan.FreshnessFresh,
		Rows: []queryplan.PlanRow{
			{Table: stringPtr("posts"), Type: stringPtr("ref")},
			{Table: stringPtr("users"), Type: stringPtr("ALL"), Rows: int64Ptr(10)},
		},
	}})
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want one per EXPLAIN row", len(lines))
	}
	if lines[0].Table != "posts" || lines[1].Table != "users" {
		t.Errorf("lines = %+v, want the server's row order preserved", lines)
	}
	if lines[1].FullScan != true || lines[0].FullScan != false {
		t.Errorf("the scan flag must follow the row that carries type=ALL: %+v", lines)
	}
}

// TestPlanLabelsCoverEveryProducedValue walks both closed enums the capture
// can emit. The rule they encode is that nothing unrecognized is ever echoed
// onto the page — a producer that grew a new value must show up as a generic
// label here rather than as unvetted text.
func TestPlanLabelsCoverEveryProducedValue(t *testing.T) {
	freshness := []struct {
		plan queryplan.Plan
		want string
	}{
		{queryplan.Plan{Freshness: queryplan.FreshnessFresh, FreshReason: queryplan.FreshInInterval}, "計測区間内"},
		{queryplan.Plan{Freshness: queryplan.FreshnessStale, FreshReason: queryplan.FreshBeforeInterval}, "区間より前"},
		{queryplan.Plan{Freshness: queryplan.FreshnessStale, FreshReason: queryplan.FreshAfterInterval}, "区間より後"},
		{queryplan.Plan{Freshness: queryplan.FreshnessUnknown, FreshReason: queryplan.FreshClockAnomaly}, "DB 時計異常のため判定不能"},
		{queryplan.Plan{Freshness: queryplan.FreshnessUnknown, FreshReason: queryplan.FreshClockMissing}, "DB 側時計情報なしのため判定不能"},
		{queryplan.Plan{Freshness: queryplan.FreshnessUnknown, FreshReason: queryplan.FreshRunPartial}, "区間が partial のため判定不能"},
		{queryplan.Plan{Freshness: queryplan.FreshnessUnknown, FreshReason: queryplan.FreshIntervalShort}, "区間が短すぎて判定不能"},
		// A reason with no verdict, and a verdict with no reason.
		{queryplan.Plan{Freshness: queryplan.FreshnessStale, FreshReason: queryplan.FreshInInterval}, "計測区間外"},
		{queryplan.Plan{Freshness: queryplan.FreshnessFresh}, "計測区間内"},
		{queryplan.Plan{}, "判定不能"},
		{queryplan.Plan{Freshness: "future-value", FreshReason: "future-reason"}, "判定不能"},
	}
	for _, tt := range freshness {
		if got := planFreshnessLabel(tt.plan); got != tt.want {
			t.Errorf("planFreshnessLabel(%+v) = %q, want %q", tt.plan, got, tt.want)
		}
	}

	errors := []struct {
		err  *queryplan.PlanError
		want string
	}{
		{nil, "実行計画の行なし"},
		{&queryplan.PlanError{Class: queryplan.PlanErrTimeout}, "タイムアウト"},
		{&queryplan.PlanError{Class: queryplan.PlanErrBudgetExhausted}, "時間予算切れ"},
		{&queryplan.PlanError{Class: queryplan.PlanErrPermission}, "権限不足"},
		{&queryplan.PlanError{Class: queryplan.PlanErrSyntax}, "構文エラーまたはサンプル切り詰め"},
		{&queryplan.PlanError{Class: queryplan.PlanErrObjectMissing}, "対象オブジェクトなし"},
		{&queryplan.PlanError{Class: queryplan.PlanErrSampleUnavail}, "サンプルなし"},
		{&queryplan.PlanError{Class: queryplan.PlanErrSampleTruncated}, "サンプル切り詰めの疑い"},
		{&queryplan.PlanError{Class: queryplan.PlanErrConnection}, "接続エラー"},
		{&queryplan.PlanError{Class: queryplan.PlanErrOther}, "その他"},
		{&queryplan.PlanError{Class: "SELECT secret_literal FROM users"}, "その他"},
	}
	for _, tt := range errors {
		if got := planErrorLabel(tt.err); got != tt.want {
			t.Errorf("planErrorLabel(%+v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestPlanTablesDropsTargetsWithoutPlans(t *testing.T) {
	tests := []struct {
		name    string
		section *queryplan.Section
		want    int
	}{
		{name: "nil section"},
		{name: "no targets", section: &queryplan.Section{}},
		{
			name: "skipped target only",
			section: &queryplan.Section{Targets: []queryplan.TargetSection{
				{TargetID: "db1", Code: queryplan.CodeBudgetExhausted},
			}},
		},
		{
			name: "one target with plans, one without",
			section: &queryplan.Section{Targets: []queryplan.TargetSection{
				{TargetID: "db1", Code: queryplan.CodeNoDigests},
				{TargetID: "db2", Plans: []queryplan.Plan{{Digest: "d", Query: "SELECT 1"}}},
			}},
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planTables(tt.section)
			if len(got) != tt.want {
				t.Fatalf("tables = %d (%+v), want %d", len(got), got, tt.want)
			}
			if tt.want == 1 && got[0].TargetID != "db2" {
				t.Errorf("table = %+v, want the target that produced plans", got[0])
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KB"},
		{5 * 1 << 20, "5.0 MB"},
		{20969086976, "19.5 GB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanBytesDelta(t *testing.T) {
	for _, tc := range []struct {
		from, to uint64
		want     string
	}{
		{2095624192, 3481604096, "+1.3 GB"},
		{3481604096, 2095624192, "-1.3 GB"},
		{100, 100, "0 B"},
	} {
		if got := string(humanBytesDelta(tc.from, tc.to)); got != tc.want {
			t.Errorf("humanBytesDelta(%d, %d) = %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestHumanBytesDeltaEmitsNoMarkup is the standing justification for
// humanBytesDelta returning template.HTML: its output alphabet is closed, so
// marking it safe cannot introduce markup into the page.
func TestHumanBytesDeltaEmitsNoMarkup(t *testing.T) {
	const allowed = "0123456789.+- BKMG"
	for _, pair := range [][2]uint64{
		{0, 0}, {0, 1}, {1, 0}, {1 << 62, 3}, {3, 1 << 62}, {1<<64 - 1, 0},
	} {
		got := string(humanBytesDelta(pair[0], pair[1]))
		if strings.ContainsAny(got, `<>&"'`) {
			t.Fatalf("humanBytesDelta(%d, %d) = %q contains markup", pair[0], pair[1], got)
		}
		if idx := strings.IndexFunc(got, func(r rune) bool {
			return !strings.ContainsRune(allowed, r)
		}); idx >= 0 {
			t.Fatalf("humanBytesDelta(%d, %d) = %q has an unexpected character %q",
				pair[0], pair[1], got, got[idx:idx+1])
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0"},
		{400 * time.Nanosecond, "400 ns"},
		{1500 * time.Microsecond, "1.5 ms"},
		{2500 * time.Millisecond, "2.5 s"},
		{-2 * time.Second, "-2.0 s"},
	} {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestOptionalValuesRenderUnknownNotZero(t *testing.T) {
	if got := optFloat(nil); got != "-" {
		t.Errorf("optFloat(nil) = %q, want %q", got, "-")
	}
	if got := optFloat(float64Ptr(0)); got != "0.0" {
		t.Errorf("optFloat(0) = %q, want %q", got, "0.0")
	}
	if got := optBytes(nil); got != "-" {
		t.Errorf("optBytes(nil) = %q, want %q", got, "-")
	}
	if got := optBytes(uint64Ptr(1 << 30)); got != "1.0 GB" {
		t.Errorf("optBytes(1GiB) = %q, want %q", got, "1.0 GB")
	}
}

func TestDigestRatioAndFlags(t *testing.T) {
	dml := sqlrows.DigestStat{Kind: sqlrows.KindDML, RowsExamined: 10}
	if got := digestRatio(dml); got != "N/A" {
		t.Errorf("digestRatio(dml) = %q, want N/A", got)
	}
	if digestRatioHot(dml) {
		t.Error("a DML digest has no ratio and must not be flagged as one")
	}
	sel := sqlrows.DigestStat{Kind: sqlrows.KindSelect, ExaminedPerSent: 5, HasRatio: true}
	if digestRatioHot(sel) {
		t.Error("exactly 5 meets the target and must not be flagged")
	}
	sel.ExaminedPerSent = 5.1
	if !digestRatioHot(sel) {
		t.Error("above 5 must be flagged")
	}
	if digestIndexHot(sel) {
		t.Error("a clean digest must not be flagged for index or sort signals")
	}
	for _, hot := range []sqlrows.DigestStat{
		{NoIndexUsed: 1}, {NoGoodIndexUsed: 1}, {SortMergePasses: 1}, {CreatedTmpDiskTables: 1},
	} {
		if !digestIndexHot(hot) {
			t.Errorf("index/sort signal must flag the digest: %#v", hot)
		}
	}
}

func TestDigestsByTotalTimeCopiesAndSorts(t *testing.T) {
	in := []sqlrows.DigestStat{
		{Digest: "a", TotalTime: time.Millisecond},
		{Digest: "b", TotalTime: time.Second},
	}
	out := digestsByTotalTime(in)
	if out[0].Digest != "b" || out[1].Digest != "a" {
		t.Fatalf("not sorted by total time descending: %#v", out)
	}
	if in[0].Digest != "a" {
		t.Error("the input slice must not be reordered in place")
	}
}

func TestTruncateRunesKeepsRuneBoundaries(t *testing.T) {
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Errorf("short text must be untouched, got %q", got)
	}
	if got := truncateRunes("日本語のクエリ", 3); got != "日本語…" {
		t.Errorf("truncateRunes = %q, want %q", got, "日本語…")
	}
}

func TestLongQueryIsTruncatedButKeptInTitle(t *testing.T) {
	long := "SELECT " + strings.Repeat("column_name, ", 40) + "1 FROM posts"
	snap := fullSnapshot()
	snap.SQLRows.Targets[0].Digests[0].Query = long
	body := renderReport(t, snap)
	if !strings.Contains(body, `title="`+long+`"`) {
		t.Error("the full query text must stay available in the title attribute")
	}
	if !strings.Contains(body, truncateRunes(long, queryDisplayRunes)) {
		t.Error("the query cell must show the truncated text")
	}
}

// TestLongPlanQueryIsTruncatedButKeptInTitle is the same guarantee for the
// query-plan table, whose cell is narrower because eight EXPLAIN columns sit
// next to it.
func TestLongPlanQueryIsTruncatedButKeptInTitle(t *testing.T) {
	long := "SELECT " + strings.Repeat("column_name, ", 40) + "1 FROM posts"
	snap := fullSnapshot()
	snap.QueryPlan.Targets[0].Plans[0].Query = long
	body := renderReport(t, snap)
	if !strings.Contains(body, `title="`+long+`"`) {
		t.Error("the full statement must stay available in the title attribute")
	}
	if !strings.Contains(body, truncateRunes(long, planQueryRunes)) {
		t.Error("the plan's query cell must show the truncated text")
	}
}
