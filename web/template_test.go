package web

import (
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/netstats"
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

func TestReportOmitsAbsentRunSections(t *testing.T) {
	// A darwin run has no hoststats and no netstats at all, and a run without
	// a database has neither sqlrows nor dbpool. None of them may leave a
	// heading behind.
	body := renderReport(t, Snapshot{})
	for _, unwanted := range []string{
		"SQL 行効率", "<h2>Host", "<h2>Network", "<h2>DB Pool",
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

func TestReportWithRunSectionsStaysSelfContained(t *testing.T) {
	body := renderReport(t, fullSnapshot())
	for _, forbidden := range []string{"http://", "https://", "src=", "href="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the report must stay self-contained, found %q", forbidden)
		}
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
