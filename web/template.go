package web

import (
	"html/template"
	"sort"
	"strconv"
	"time"

	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/sqlrows"
)

// indexTmpl renders the home page: a list of persisted runs, newest first,
// each linking to /<run-id>. The full live report lives at /live.
var indexTmpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"gb": func(bytes uint64) string {
		if bytes == 0 {
			return "?"
		}
		return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 1, 64)
	},
}).Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>isutools runs</title>
<style>
body { font-family: ui-monospace, monospace; margin: 1.5rem; background: #fff; color: #111; }
h1 { font-size: 1.2rem; margin: 0 0 .25rem; }
h2 { font-size: 1rem; margin: 1.5rem 0 .5rem; border-bottom: 2px solid #333; padding-bottom: .2rem; }
.meta { color: #666; margin: 0 0 .4rem; font-size: .85rem; }
.warn { color: #b45309; }
table { border-collapse: collapse; width: 100%; font-size: .85rem; }
th, td { border: 1px solid #ddd; padding: .35rem .6rem; text-align: right; white-space: nowrap; }
th { background: #f5f5f5; }
td.l { text-align: left; }
tbody tr:nth-child(odd) { background: #fafafa; }
.empty { color: #999; }
a { color: #0b57d0; }
</style>
</head>
<body>
<h1>isutools</h1>
<p class="meta">{{.Snapshot.Meta.Time}} &middot; rev {{.Snapshot.Meta.Revision}} &middot; gen {{.Snapshot.Meta.Generation}}</p>
<p class="meta">{{.Snapshot.Meta.Host.Hostname}} &middot; {{.Snapshot.Meta.Host.CPUModel}} &middot; {{.Snapshot.Meta.Host.NumCPU}} cores &middot; {{gb .Snapshot.Meta.Host.MemTotalBytes}} GB &middot; {{.Snapshot.Meta.Host.OS}}</p>
{{if .Snapshot.Meta.Partial}}<p class="meta warn">partial snapshot: one or more collectors reported incomplete data</p>{{end}}
<p class="meta"><a href="live">live report</a> &middot; <a href="snapshot.html">download current</a> &middot; <a href="json">json</a></p>

<h2>Runs <span class="meta">(newest first)</span></h2>
{{if .Runs}}
<table>
<thead><tr><th>time</th><th>gen</th><th>rev</th><th>score</th><th>raw</th></tr></thead>
<tbody>
{{range .Runs}}<tr>
<td class="l"><a href="{{.ID}}">{{.Label}}</a></td>
<td>{{.Gen}}</td>
<td class="l">{{.Rev}}</td>
<td>{{if .Score}}{{.Score}}{{else}}-{{end}}</td>
<td class="l"><a href="files/{{.File}}">html</a> <a href="files/{{.JSON}}">json</a>{{if .PrevID}} <a href="diff?a={{.PrevID}}&b={{.ID}}">diff</a>{{end}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}<p class="empty">no saved runs yet — bench.sh (POST /save) がベンチ毎にここへ追加します</p>{{end}}

<h2>CPU Profiles <span class="meta">(captured per benchmark; open with: go tool pprof -http :8081 &lt;file&gt;)</span></h2>
{{if .Profiles}}
<ul class="files">
{{range .Profiles}}<li><a href="files/{{.}}">{{.}}</a></li>{{end}}
</ul>
{{else}}<p class="empty">no profiles yet (set ISUTOOLS_PPROF_SECONDS; captured automatically after POST /reset). Live profiling: <a href="pprof/">pprof/</a></p>{{end}}
</body>
</html>
`))

// sqlRowsRatioWarn is the examined/sent ratio above which a SELECT is
// highlighted. It is the ISUCON13 winning team's published target ("5x or
// less"), not a measurement of this workload, so it flags rows for a reader
// rather than deciding anything.
const sqlRowsRatioWarn = 5.0

// queryDisplayRunes bounds the query text shown in a table cell. The full text
// stays reachable through the cell's title attribute, so truncation never
// destroys information.
const queryDisplayRunes = 120

// humanBytes renders a byte count in the largest unit that keeps it readable.
// Raw byte counts are the one thing a reader cannot compare at a glance, and
// every new section reports memory, disk and filesystem sizes in bytes.
func humanBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GB"
	case b >= 1<<20:
		return strconv.FormatFloat(float64(b)/(1<<20), 'f', 1, 64) + " MB"
	case b >= 1<<10:
		return strconv.FormatFloat(float64(b)/(1<<10), 'f', 1, 64) + " KB"
	default:
		return strconv.FormatUint(b, 10) + " B"
	}
}

// humanBytesDelta renders the signed change between two boundary readings.
// hoststats reports point observations at both ends precisely because the
// movement is the signal, and an unsigned pair leaves the reader subtracting.
//
// It returns template.HTML because html/template rewrites a leading "+" in a
// text node to "&#43;". That displays correctly in a browser, but a saved
// snapshot.html is also a file people read and grep, and nobody greps for
// "&#43;1.3 GB". Marking the value safe is sound here and only here: the
// output alphabet is closed at digits, ".", " ", "+", "-" and a fixed unit
// suffix, all produced by this function from two integers, with no caller
// text anywhere in it. TestHumanBytesDeltaEmitsNoMarkup pins that.
func humanBytesDelta(from, to uint64) template.HTML {
	switch {
	case to > from:
		return template.HTML("+" + humanBytes(to-from))
	case to < from:
		return template.HTML("-" + humanBytes(from-to))
	default:
		return template.HTML("0 B")
	}
}

// humanDuration renders a duration in the largest unit that keeps a leading
// digit, so a 3-second total and a 40-microsecond average stay comparable
// without counting zeros.
func humanDuration(d time.Duration) string {
	if d < 0 {
		return "-" + humanDuration(-d)
	}
	switch {
	case d >= time.Second:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
	case d >= time.Millisecond:
		return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64) + " ms"
	case d >= time.Microsecond:
		return strconv.FormatFloat(float64(d)/float64(time.Microsecond), 'f', 1, 64) + " µs"
	case d == 0:
		return "0"
	default:
		return strconv.FormatInt(int64(d), 10) + " ns"
	}
}

// optFloat renders a rate that the collector could not derive as "-" rather
// than as 0. The collectors use a nil pointer exactly to keep "unknown" from
// reading as "idle", and the display has to preserve that distinction.
func optFloat(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'f', 1, 64)
}

// optBytes is optFloat for an absent byte limit (an unlimited cgroup, say).
func optBytes(v *uint64) string {
	if v == nil {
		return "-"
	}
	return humanBytes(*v)
}

// clockTime renders a boundary timestamp as a wall clock, which is all a
// reader needs to line two sections' intervals up.
func clockTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("15:04:05")
}

// truncateRunes shortens display text on a rune boundary so a multi-byte
// query never renders as a broken code point.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// digestsByTotalTime returns a copy ordered by total time descending. The
// collector already emits this order, but the template must not depend on an
// upstream ordering guarantee to put the expensive query at the top.
func digestsByTotalTime(digests []sqlrows.DigestStat) []sqlrows.DigestStat {
	out := append([]sqlrows.DigestStat(nil), digests...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TotalTime != out[j].TotalTime {
			return out[i].TotalTime > out[j].TotalTime
		}
		return out[i].TimerWaitPicos > out[j].TimerWaitPicos
	})
	return out
}

// digestRatio renders the examined/sent ratio, or "N/A" where the ratio is
// undefined. HasRatio is false for DML and for a SELECT that sent no rows;
// dividing there would either divide by zero or invent the worst possible
// score for a query that simply found nothing.
func digestRatio(d sqlrows.DigestStat) string {
	if !d.HasRatio {
		return "N/A"
	}
	return strconv.FormatFloat(d.ExaminedPerSent, 'f', 1, 64)
}

// digestRatioHot reports a defined ratio above the ISUCON13 target.
func digestRatioHot(d sqlrows.DigestStat) bool {
	return d.HasRatio && d.ExaminedPerSent > sqlRowsRatioWarn
}

// digestIndexHot reports a digest that hit the index or sort quality signals:
// a missing index, an on-disk temporary table, or a merge sort pass.
func digestIndexHot(d sqlrows.DigestStat) bool {
	return d.NoIndexUsed > 0 || d.NoGoodIndexUsed > 0 ||
		d.SortMergePasses > 0 || d.CreatedTmpDiskTables > 0
}

// reportFuncs is the report template's function set. The formatting helpers
// are named functions rather than closures so each one can be tested on its
// own, without rendering a page to find out what it prints.
var reportFuncs = template.FuncMap{
	"ms": func(d time.Duration) string {
		return strconv.FormatFloat(float64(d.Nanoseconds())/1e6, 'f', 1, 64)
	},
	"gb": func(bytes uint64) string {
		if bytes == 0 {
			return "?"
		}
		return strconv.FormatFloat(float64(bytes)/(1<<30), 'f', 1, 64)
	},
	"mb": func(bytes int64) string {
		return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 1, 64)
	},
	"mib": func(bytes uint64) string {
		return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 1, 64)
	},
	"f1": func(value float64) string {
		return strconv.FormatFloat(value, 'f', 1, 64)
	},
	"size":         humanBytes,
	"sizeDelta":    humanBytesDelta,
	"dur":          humanDuration,
	"pf1":          optFloat,
	"psize":        optBytes,
	"clock":        clockTime,
	"cut":          truncateRunes,
	"byTime":       digestsByTotalTime,
	"ratio":        digestRatio,
	"ratioHot":     digestRatioHot,
	"indexHot":     digestIndexHot,
	"diskUtilNote": func() string { return hoststats.DiskUtilNote },
	"cgroupNote":   func() string { return hoststats.CGroupScopeNote },
}

var reportTmpl = template.Must(template.New("report").Funcs(reportFuncs).Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>isutools {{.Snapshot.Meta.Revision}} gen{{.Snapshot.Meta.Generation}}</title>
<style>
body { font-family: ui-monospace, monospace; margin: 1.5rem; background: #fff; color: #111; }
h1 { font-size: 1.2rem; margin: 0 0 .25rem; }
h2 { font-size: 1rem; margin: 1.5rem 0 .5rem; border-bottom: 2px solid #333; padding-bottom: .2rem; }
.meta { color: #666; margin: 0 0 .4rem; font-size: .85rem; }
table { border-collapse: collapse; width: 100%; font-size: .8rem; }
th, td { border: 1px solid #ddd; padding: .3rem .5rem; text-align: right; white-space: nowrap; }
th { background: #f5f5f5; cursor: pointer; user-select: none; }
td.l { text-align: left; white-space: normal; word-break: break-all; }
tbody tr:nth-child(odd) { background: #fafafa; }
.empty { color: #999; }
.warn { color: #b45309; }
tr.hot > td { background: #fef3c7; }
td.flag { color: #b45309; font-weight: bold; }
details { font-size: .8rem; margin: .4rem 0; }
summary { cursor: pointer; color: #666; }
ul.files { font-size: .85rem; line-height: 1.7; padding-left: 1.2rem; }
</style>
</head>
<body>
<h1>isutools report{{if .Snapshot.Meta.Score}} — score {{.Snapshot.Meta.Score}}{{end}}</h1>
<p class="meta">{{.Snapshot.Meta.Time}} &middot; rev {{.Snapshot.Meta.Revision}} &middot; gen {{.Snapshot.Meta.Generation}}{{if .Snapshot.Meta.Score}} &middot; score {{.Snapshot.Meta.Score}}{{end}}</p>
<p class="meta">{{.Snapshot.Meta.Host.Hostname}} &middot; {{.Snapshot.Meta.Host.CPUModel}} &middot; {{.Snapshot.Meta.Host.NumCPU}} cores &middot; {{gb .Snapshot.Meta.Host.MemTotalBytes}} GB &middot; {{.Snapshot.Meta.Host.OS}}</p>
<p class="meta">collectors: SQL &middot; DB schema &middot; HTTP &middot; process &middot; nginx access log</p>

<h2>Collector Health</h2>
{{if .Snapshot.Meta.Partial}}<p class="warn">partial snapshot: one or more collectors reported incomplete data</p>{{end}}
{{if .Snapshot.Meta.Health}}
<table>
<thead><tr><th>collector</th><th>status</th><th>dropped</th><th>message</th></tr></thead>
<tbody>{{range .Snapshot.Meta.Health}}<tr>
<td class="l">{{.Collector}}</td><td>{{.Status}}</td><td data-v="{{.Dropped}}">{{.Dropped}}</td><td class="l">{{.Message}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no core collector warnings</p>{{end}}

<h2>Advisor <span class="meta">(ISUCON 定石で未設定のもの)</span></h2>
{{if .Snapshot.Advisor}}
<table>
<thead><tr><th>status</th><th>check</th><th>current</th><th>recommendation</th></tr></thead>
<tbody>
{{range .Snapshot.Advisor}}<tr>
<td class="l">{{if eq (printf "%s" .Status) "missing"}}<strong class="warn">missing</strong>{{else if eq (printf "%s" .Status) "warn"}}<span class="warn">warn</span>{{else}}{{.Status}}{{end}}</td>
<td class="l">{{.Title}}</td>
<td class="l">{{.Detail}}</td>
<td class="l">{{.Recommendation}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}<p class="empty">not captured</p>{{end}}

<h2>DB Schema <span class="meta">(captured at generation start)</span></h2>
{{if .Snapshot.DB}}{{if .Snapshot.DB.Error}}<p class="empty warn">{{.Snapshot.DB.Error}}</p>{{else}}
<table>
<thead><tr><th>table</th><th>engine</th><th>~rows</th><th>data(MB)</th><th>indexes</th></tr></thead>
<tbody>
{{range .Snapshot.DB.Tables}}<tr>
<td class="l">{{.Name}}</td>
<td>{{.Engine}}</td>
<td data-v="{{.Rows}}">{{.Rows}}</td>
<td data-v="{{.DataBytes}}">{{mb .DataBytes}}</td>
<td class="l">{{range $i, $ix := .Indexes}}{{if $i}}, {{end}}{{$ix.Name}}({{$ix.Columns}}){{if $ix.Unique}}&#42;{{end}}{{end}}</td>
</tr>{{end}}
</tbody>
</table>
<p class="meta">&#42; = unique index. captured {{.Snapshot.DB.CapturedAt}}</p>
{{end}}{{else}}<p class="empty">not captured (no DB connection observed yet)</p>{{end}}

<h2>SQL</h2>
{{if .Snapshot.SQL}}
<table>
<thead><tr>
<th>total(ms)</th><th>count</th><th>errors</th><th>avg(ms)</th><th>p95*(ms)</th><th>max(ms)</th><th>query</th>
</tr></thead>
<tbody>
{{range .Snapshot.SQL}}<tr>
<td data-v="{{.Total.Nanoseconds}}">{{ms .Total}}</td>
<td data-v="{{.Count}}">{{.Count}}</td>
<td data-v="{{.ErrorCount}}">{{.ErrorCount}}</td>
<td data-v="{{.Avg.Nanoseconds}}">{{ms .Avg}}</td>
<td data-v="{{.P95.Nanoseconds}}">{{ms .P95}}</td>
<td data-v="{{.Max.Nanoseconds}}">{{ms .Max}}</td>
<td class="l">{{.Key}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}<p class="empty">no observations yet</p>{{end}}
<p class="meta">* p95 is a log2-bucket upper-bound approximation. SQL time = query dispatch until first response (row iteration excluded).</p>

{{with .Snapshot.SQLRows}}{{if or .Targets .Health}}
<h2>SQL 行効率 <span class="meta">(performance_schema の rows examined / rows sent。ISUCON13 優勝チームの基準は「5 倍以下」)</span></h2>
{{range .Targets}}
<p class="meta">target {{.TargetID}}{{if .Schema}} &middot; schema {{.Schema}}{{end}}{{if .Usable}} &middot; shown {{.Shown}} / total {{.Total}}{{if .Dropped}} &middot; <span class="warn">dropped {{.Dropped}}</span>{{end}}{{end}}{{if .Overflow.Detected}} &middot; <span class="warn">digest overflow{{if .Overflow.CountStar}} ({{.Overflow.CountStar}} statements){{end}} — performance_schema_digests_size が不足しており、この target の集計は全数ではありません</span>{{end}}{{if .DBClock.Anomaly}} &middot; <span class="warn">db clock {{.DBClock.Anomaly}}</span>{{end}}</p>
{{if .Usable}}{{if .Digests}}
<table>
<thead><tr>
<th>total</th><th>count</th><th>examined</th><th>sent</th><th>examined/sent</th><th>affected</th><th>no index</th><th>tmp disk</th><th>sort merge</th><th>kind</th><th>query</th>
</tr></thead>
<tbody>
{{range byTime .Digests}}<tr{{if or (ratioHot .) (indexHot .)}} class="hot"{{end}}>
<td data-v="{{.TotalTime.Nanoseconds}}">{{dur .TotalTime}}</td>
<td data-v="{{.Count}}">{{.Count}}</td>
<td data-v="{{.RowsExamined}}">{{.RowsExamined}}</td>
<td data-v="{{.RowsSent}}">{{.RowsSent}}</td>
<td data-v="{{.ExaminedPerSent}}"{{if ratioHot .}} class="flag"{{end}}>{{ratio .}}</td>
<td data-v="{{.RowsAffected}}">{{if eq (printf "%s" .Kind) "dml"}}{{.RowsAffected}}{{else}}-{{end}}</td>
<td data-v="{{.NoIndexUsed}}"{{if .NoIndexUsed}} class="flag"{{end}}>{{.NoIndexUsed}}</td>
<td data-v="{{.CreatedTmpDiskTables}}"{{if .CreatedTmpDiskTables}} class="flag"{{end}}>{{.CreatedTmpDiskTables}}</td>
<td data-v="{{.SortMergePasses}}"{{if .SortMergePasses}} class="flag"{{end}}>{{.SortMergePasses}}</td>
<td>{{.Kind}}</td>
<td class="l" title="{{.Query}}">{{cut .Query 120}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}<p class="empty">この target には区間内に実行された digest がありません</p>{{end}}
{{else}}<p class="empty warn">skipped ({{if .Code}}{{.Code}}{{else}}unknown{{end}}){{if .Reason}}: {{.Reason}}{{end}} — この target は数値を提供しません</p>{{end}}
{{end}}
<p class="meta">examined/sent は SELECT かつ sent &gt; 0 の digest でのみ算出します(DML と sent=0 は N/A)。網掛けの行は比 &gt; 5、または no index / tmp disk table / sort merge が発生した digest です。</p>
{{if .Health}}<p class="meta warn">{{range $i, $note := .Health}}{{if $i}} &middot; {{end}}{{$note.Key}}: {{$note.Message}}{{end}}</p>{{end}}
{{end}}{{end}}

<h2>HTTP</h2>
{{if .Snapshot.HTTP}}
<table>
<thead><tr><th>total(ms)</th><th>count</th><th>avg(ms)</th><th>p95*(ms)</th><th>max(ms)</th><th>bytes</th><th>request</th></tr></thead>
<tbody>{{range .Snapshot.HTTP}}<tr>
<td data-v="{{.Total.Nanoseconds}}">{{ms .Total}}</td><td data-v="{{.Count}}">{{.Count}}</td>
<td data-v="{{.Avg.Nanoseconds}}">{{ms .Avg}}</td><td data-v="{{.P95.Nanoseconds}}">{{ms .P95}}</td>
<td data-v="{{.Max.Nanoseconds}}">{{ms .Max}}</td><td data-v="{{.TotalBytes}}">{{.TotalBytes}}</td><td class="l">{{.Key}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no HTTP observations (wrap the application handler with isutools.HTTP)</p>{{end}}
{{with .Snapshot.Connections}}{{if .Total}}<p class="meta">long-lived connections (WS/SSE, latency 集計から分離): total {{.Total}} &middot; active {{.Active}} &middot; avg {{printf "%.1f" .AvgSeconds}}s &middot; max {{printf "%.1f" .MaxSeconds}}s</p>{{end}}{{end}}

{{if .Snapshot.DBPool}}
<h2>DB Pool <span class="meta">(database/sql のコネクションプール。点の値は終端境界、カウンタは区間デルタ)</span></h2>
<table>
<thead><tr>
<th>max open</th><th>open</th><th>in use</th><th>idle</th><th>waits</th><th>wait 合計*</th><th>平均 wait</th><th>idle closed</th><th>idletime closed</th><th>lifetime closed</th><th>interval</th><th>target</th><th>endpoint</th>
</tr></thead>
<tbody>
{{range .Snapshot.DBPool}}<tr>
<td data-v="{{.MaxOpen}}">{{if .MaxOpen}}{{.MaxOpen}}{{else}}unlimited{{end}}</td>
<td data-v="{{.Open}}">{{.Open}}</td>
<td data-v="{{.InUse}}">{{.InUse}}</td>
<td data-v="{{.Idle}}">{{.Idle}}</td>
<td data-v="{{.WaitCount}}"{{if .WaitCount}} class="flag"{{end}}>{{.WaitCount}}</td>
<td data-v="{{.WaitDuration.Nanoseconds}}">{{dur .WaitDuration}}</td>
<td data-v="{{.AverageWait.Nanoseconds}}">{{if .WaitCount}}{{dur .AverageWait}}{{else}}-{{end}}</td>
<td data-v="{{.MaxIdleClosed}}">{{.MaxIdleClosed}}</td>
<td data-v="{{.MaxIdleTimeClosed}}">{{.MaxIdleTimeClosed}}</td>
<td data-v="{{.MaxLifetimeClosed}}">{{.MaxLifetimeClosed}}</td>
<td data-v="{{.Interval.Nanoseconds}}">{{dur .Interval}}{{if .Partial}} <span class="warn">{{if .Code}}{{.Code}}{{else}}partial{{end}}</span>{{end}}</td>
<td class="l">{{.TargetID}}</td>
<td class="l">{{.Display}}</td>
</tr>{{end}}
</tbody>
</table>
<p class="meta">* wait 合計 (wait_duration) は待たされた goroutine 全員の待ち時間の総和であり、経過時間ではありません。並列待機のぶん run の長さを超えることがあるので、クエリ遅延と比べるときは並列度を含まない「平均 wait」(wait_duration ÷ waits) を使ってください。waits が 0 でなければ、そのぶんの待ち時間を決めたのは DB ではなくプール上限です。</p>
{{end}}

<h2>Counters <span class="meta">(isutools.Count によるアプリ内カウンタ)</span></h2>
{{if .Snapshot.Counters}}
<table>
<thead><tr><th>count</th><th>name</th></tr></thead>
<tbody>{{range .Snapshot.Counters}}<tr><td data-v="{{.Count}}">{{.Count}}</td><td class="l">{{.Name}}</td></tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no counters (アプリで isutools.Count("cache_hit") 等を呼ぶとここに出ます)</p>{{end}}

<h2>Proxy Access Log</h2>
{{if .Snapshot.AccessLog}}
<p class="meta">status {{.Snapshot.AccessLog.Health.Status}} &middot; lines {{.Snapshot.AccessLog.Lines}} &middot; dropped {{.Snapshot.AccessLog.Health.Dropped}} &middot; {{.Snapshot.AccessLog.Health.Message}}</p>
{{if .Snapshot.AccessLog.Entries}}<table>
<thead><tr><th>total(ms)</th><th>count</th><th>avg(ms)</th><th>p95*(ms)</th><th>upstream(ms)</th><th>bytes</th><th>method</th><th>uri</th></tr></thead>
<tbody>{{range .Snapshot.AccessLog.Entries}}<tr>
<td data-v="{{.RequestTotal.Nanoseconds}}">{{ms .RequestTotal}}</td><td data-v="{{.Count}}">{{.Count}}</td>
<td data-v="{{.RequestAvg.Nanoseconds}}">{{ms .RequestAvg}}</td><td data-v="{{.RequestP95.Nanoseconds}}">{{ms .RequestP95}}</td>
<td data-v="{{.UpstreamTotal.Nanoseconds}}">{{ms .UpstreamTotal}}</td><td data-v="{{.BytesTotal}}">{{.BytesTotal}}</td>
<td class="l">{{.Method}}</td><td class="l">{{.URI}}</td>
</tr>{{end}}</tbody>
</table>{{else}}<p class="empty">no access-log observations in this generation</p>{{end}}
{{else}}<p class="empty">not configured (set ISUTOOLS_ACCESS_LOG)</p>{{end}}

<h2>Scenario Stories <span class="meta">(明示scenarioラベル別の実測request列。疑似sessが必要)</span></h2>
{{if and .Snapshot.AccessLog .Snapshot.AccessLog.Stories}}
<table>
<thead><tr><th>sessions</th><th>requests</th><th>scenario</th><th>observed journey</th></tr></thead>
<tbody>{{range .Snapshot.AccessLog.Stories}}<tr>
<td data-v="{{.Sessions}}">{{.Sessions}}</td><td data-v="{{.Requests}}">{{.Requests}}</td><td class="l">{{.Scenario}}</td>
<td class="l">{{range $i, $step := .Journey}}{{if $i}} &rarr; {{end}}{{$step}}{{end}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no scenario story data (proxy logに安全なsess:とscenario:を追加)</p>{{end}}

<h2>User Flow <span class="meta">(セッション毎のページ遷移 上位20。proxy ログの sess: フィールドが必要)</span></h2>
{{if and .Snapshot.AccessLog .Snapshot.AccessLog.Flows}}
<table>
<thead><tr><th>count</th><th>from</th><th></th><th>to</th></tr></thead>
<tbody>{{range .Snapshot.AccessLog.Flows}}<tr>
<td data-v="{{.Count}}">{{.Count}}</td><td class="l">{{.From}}</td><td>&rarr;</td><td class="l">{{.To}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no flow data (proxy log に sess: を追加すると「ユーザーがどうアプリを使っているか」が見えます)</p>{{end}}

<h2>Processes</h2>
{{if .Snapshot.Proc}}
<p class="meta">status {{.Snapshot.Proc.Health.Status}} &middot; interval jiffies {{.Snapshot.Proc.IntervalJiffies}} &middot; {{.Snapshot.Proc.CPUs}} CPUs</p>
{{with .Snapshot.Proc.CPUTotal}}<p class="meta"><strong>CPU total: {{f1 .BusyPercent}}% busy</strong> (user {{f1 .UserPercent}}% / sys {{f1 .SystemPercent}}% / iowait {{f1 .IOWaitPercent}}% / idle {{f1 .IdlePercent}}%) — idle が大きければ並列度・設定不足、busy 100% 近くなら CPU 飽和</p>{{end}}
{{if .Snapshot.Proc.TopCPU}}<table>
<thead><tr><th>CPU%</th><th>CPU(s)</th><th>RSS(MiB)</th><th>PID</th><th>command</th></tr></thead>
<tbody>{{range .Snapshot.Proc.TopCPU}}<tr>
<td data-v="{{.CPUPercent}}">{{f1 .CPUPercent}}</td><td data-v="{{.CPUSeconds}}">{{f1 .CPUSeconds}}</td>
<td data-v="{{.RSSBytes}}">{{mib .RSSBytes}}</td><td data-v="{{.PID}}">{{.PID}}</td><td class="l">{{.Command}}</td>
</tr>{{end}}</tbody>
</table>{{else}}<p class="empty">no process interval data (POST /reset before the benchmark)</p>{{end}}
{{else}}<p class="empty">process collector unavailable on this platform</p>{{end}}

{{with .Snapshot.Host}}
<h2>Host <span class="meta">(ベンチ区間のホスト資源。procfs / sysfs / cgroup v2 のみ)</span></h2>
<p class="meta">interval {{f1 .Interval.Seconds}}s ({{clock .Interval.BaselineAt}} &rarr; {{clock .Interval.FinalAt}}){{if .CGroup}} &middot; cgroup scope {{.CGroup.Scope}}{{end}}{{if .Partial}} &middot; <span class="warn">partial</span>{{end}}{{if .Codes}} &middot; <span class="warn">{{range $i, $code := .Codes}}{{if $i}}, {{end}}{{$code}}{{end}}</span>{{end}}</p>
<table>
<thead><tr><th>memory</th><th>baseline</th><th>final</th><th>delta</th></tr></thead>
<tbody>
<tr><td class="l">available</td><td data-v="{{.Memory.AvailableBaseline}}">{{size .Memory.AvailableBaseline}}</td><td data-v="{{.Memory.AvailableFinal}}">{{size .Memory.AvailableFinal}}</td><td>{{sizeDelta .Memory.AvailableBaseline .Memory.AvailableFinal}}</td></tr>
<tr><td class="l">cached</td><td data-v="{{.Memory.CachedBaseline}}">{{size .Memory.CachedBaseline}}</td><td data-v="{{.Memory.CachedFinal}}">{{size .Memory.CachedFinal}}</td><td>{{sizeDelta .Memory.CachedBaseline .Memory.CachedFinal}}</td></tr>
<tr><td class="l">dirty</td><td data-v="{{.Memory.DirtyBaseline}}">{{size .Memory.DirtyBaseline}}</td><td data-v="{{.Memory.DirtyFinal}}">{{size .Memory.DirtyFinal}}</td><td>{{sizeDelta .Memory.DirtyBaseline .Memory.DirtyFinal}}</td></tr>
{{if .Memory.SwapTotalBytes}}<tr><td class="l">swap free</td><td data-v="{{.Memory.SwapFreeBaseline}}">{{size .Memory.SwapFreeBaseline}}</td><td data-v="{{.Memory.SwapFreeFinal}}">{{size .Memory.SwapFreeFinal}}</td><td>{{sizeDelta .Memory.SwapFreeBaseline .Memory.SwapFreeFinal}}</td></tr>{{end}}
</tbody>
</table>
<p class="meta">memory total {{size .Memory.TotalBytes}}{{if .Memory.SwapTotalBytes}} &middot; swap total {{size .Memory.SwapTotalBytes}}{{end}} &middot; major page faults {{.Memory.PageMajorFaults}} (区間デルタ。ディスクまで到達した page fault なので、増えていればメモリ不足の実感そのものです)</p>
{{if .Disks}}
<table>
<thead><tr><th>read</th><th>write</th><th>read(MB/s)</th><th>write(MB/s)</th><th>io time(ms)</th><th>util%</th><th>queue avg</th><th>device</th></tr></thead>
<tbody>
{{range .Disks}}<tr>
<td data-v="{{.ReadBytes}}">{{size .ReadBytes}}</td>
<td data-v="{{.WriteBytes}}">{{size .WriteBytes}}</td>
<td{{with .ReadMBPerSec}} data-v="{{.}}"{{end}}>{{pf1 .ReadMBPerSec}}</td>
<td{{with .WriteMBPerSec}} data-v="{{.}}"{{end}}>{{pf1 .WriteMBPerSec}}</td>
<td data-v="{{.IOTimeMillis}}">{{.IOTimeMillis}}</td>
<td{{with .UtilPercent}} data-v="{{.}}"{{end}}>{{pf1 .UtilPercent}}</td>
<td{{with .QueueAvg}} data-v="{{.}}"{{end}}>{{pf1 .QueueAvg}}</td>
<td class="l">{{.Device}}{{if .Appeared}} <span class="warn">appeared mid-run</span>{{end}}{{if .Code}} <span class="warn">{{.Code}}</span>{{end}}</td>
</tr>{{end}}
</tbody>
</table>
<p class="meta">{{diskUtilNote}}</p>
{{end}}
{{with .PSI}}
<table>
<thead><tr><th>pressure</th><th>some avg10</th><th>some avg60</th><th>some stall</th><th>full avg10</th><th>full avg60</th><th>full stall</th></tr></thead>
<tbody>
<tr><td class="l">cpu</td><td>{{f1 .CPU.SomeAvg10}}</td><td>{{f1 .CPU.SomeAvg60}}</td><td>{{pf1 .CPU.SomeStallRatio}}</td><td>{{f1 .CPU.FullAvg10}}</td><td>{{f1 .CPU.FullAvg60}}</td><td>{{pf1 .CPU.FullStallRatio}}</td></tr>
<tr><td class="l">memory</td><td>{{f1 .Memory.SomeAvg10}}</td><td>{{f1 .Memory.SomeAvg60}}</td><td>{{pf1 .Memory.SomeStallRatio}}</td><td>{{f1 .Memory.FullAvg10}}</td><td>{{f1 .Memory.FullAvg60}}</td><td>{{pf1 .Memory.FullStallRatio}}</td></tr>
<tr><td class="l">io</td><td>{{f1 .IO.SomeAvg10}}</td><td>{{f1 .IO.SomeAvg60}}</td><td>{{pf1 .IO.SomeStallRatio}}</td><td>{{f1 .IO.FullAvg10}}</td><td>{{f1 .IO.FullAvg60}}</td><td>{{pf1 .IO.FullStallRatio}}</td></tr>
</tbody>
</table>
<p class="meta">avg10 / avg60 はカーネル側の減衰平均(終端境界での値)、stall はこの run の区間で実際に停止していた割合です。</p>
{{end}}
{{if .Filesystems}}
<table>
<thead><tr><th>total</th><th>avail(baseline)</th><th>avail(final)</th><th>delta</th><th>path</th></tr></thead>
<tbody>
{{range .Filesystems}}<tr>
<td data-v="{{.TotalBytes}}">{{size .TotalBytes}}</td>
<td data-v="{{.AvailBaseline}}">{{size .AvailBaseline}}</td>
<td data-v="{{.AvailFinal}}">{{size .AvailFinal}}</td>
<td>{{sizeDelta .AvailBaseline .AvailFinal}}</td>
<td class="l">{{.Path}}</td>
</tr>{{end}}
</tbody>
</table>
{{end}}
{{with .CGroup}}
<p class="meta">cgroup {{.Scope}} &middot; path {{if .Path}}{{.Path}}{{else}}-{{end}} &middot; cpu.max {{pf1 .CPUMaxCores}} cores &middot; memory.max {{psize .MemoryMaxBytes}} &middot; memory.current {{size .MemoryCurrentBaseline}} &rarr; {{size .MemoryCurrentFinal}} ({{sizeDelta .MemoryCurrentBaseline .MemoryCurrentFinal}}){{if .Code}} &middot; <span class="warn">{{.Code}}</span>{{end}}</p>
<p class="meta">{{cgroupNote}}</p>
{{end}}
<details>
<summary>identity / namespace (この数値をどこから見たか)</summary>
<table>
<tbody>
<tr><td class="l">hostname</td><td class="l">{{.Identity.Hostname}}</td></tr>
<tr><td class="l">machine id hash</td><td class="l">{{.Identity.MachineIDHash}}</td></tr>
<tr><td class="l">boot id hash</td><td class="l">{{.Identity.BootIDHash}}</td></tr>
<tr><td class="l">pid ns</td><td class="l">{{.Identity.PIDNS}}</td></tr>
<tr><td class="l">net ns</td><td class="l">{{.Identity.NetNS}}</td></tr>
<tr><td class="l">mnt ns</td><td class="l">{{.Identity.MntNS}}</td></tr>
<tr><td class="l">cgroup ns</td><td class="l">{{.Identity.CgroupNS}}</td></tr>
{{if .Identity.Role}}<tr><td class="l">role</td><td class="l">{{.Identity.Role}}</td></tr>{{end}}
<tr><td class="l">agent version</td><td class="l">{{.Identity.AgentVersion}}</td></tr>
</tbody>
</table>
<p class="meta">コンテナ内では同じファイルが machine ではなく namespace を説明します。identity と cgroup scope の無い数値は読めません。</p>
</details>
{{end}}

{{with .Snapshot.Network}}
<h2>Network <span class="meta">(TCP ソケット要約は点観測、NIC は区間デルタ)</span></h2>
<p class="meta">TCP in_use {{.TCP.InUse}} &middot; time_wait {{.TCP.TimeWait}} &middot; orphan {{.TCP.Orphan}} &middot; in_use6 {{.TCP.InUse6}}</p>
<p class="meta">time_wait は inbound と outbound を区別せず、ローカルの ephemeral port を消費している socket だけを数えるわけでもありません。したがってこの値だけではポート枯渇の証拠になりません。</p>
{{if .Interfaces}}
<table>
<thead><tr><th>rx(Mbit/s)</th><th>tx(Mbit/s)</th><th>rx bytes</th><th>tx bytes</th><th>rx packets</th><th>tx packets</th><th>rx errors</th><th>tx errors</th><th>rx dropped</th><th>tx dropped</th><th>speed(Mbit/s)</th><th>MTU</th><th>interface</th></tr></thead>
<tbody>
{{range .Interfaces}}<tr>
<td{{with .RxMbitPerSec}} data-v="{{.}}"{{end}}>{{pf1 .RxMbitPerSec}}</td>
<td{{with .TxMbitPerSec}} data-v="{{.}}"{{end}}>{{pf1 .TxMbitPerSec}}</td>
<td data-v="{{.RxBytes}}">{{size .RxBytes}}</td>
<td data-v="{{.TxBytes}}">{{size .TxBytes}}</td>
<td data-v="{{.RxPackets}}">{{.RxPackets}}</td>
<td data-v="{{.TxPackets}}">{{.TxPackets}}</td>
<td data-v="{{.RxErrors}}"{{if .RxErrors}} class="flag"{{end}}>{{.RxErrors}}</td>
<td data-v="{{.TxErrors}}"{{if .TxErrors}} class="flag"{{end}}>{{.TxErrors}}</td>
<td data-v="{{.RxDropped}}"{{if .RxDropped}} class="flag"{{end}}>{{.RxDropped}}</td>
<td data-v="{{.TxDropped}}"{{if .TxDropped}} class="flag"{{end}}>{{.TxDropped}}</td>
<td data-v="{{.SpeedMbit}}">{{if .SpeedMbit}}{{.SpeedMbit}}{{else}}-{{end}}</td>
<td data-v="{{.MTU}}">{{if .MTU}}{{.MTU}}{{else}}-{{end}}</td>
<td class="l">{{.Name}}{{if .Appeared}} <span class="warn">appeared mid-run</span>{{end}}{{if .Code}} <span class="warn">{{.Code}}</span>{{end}}</td>
</tr>{{end}}
</tbody>
</table>
<p class="meta">Mbit/s は NIC の speed とそのまま比べられる単位です。区間平均なので瞬間的な飽和は見えません。MTU は表示のみ(経路全体が一致して初めて意味を持つため、良し悪しは判定しません)。</p>
{{else}}<p class="empty">no interface counters (loopback は既定で除外)</p>{{end}}
{{if .Health}}<p class="meta warn">{{range $i, $note := .Health}}{{if $i}} &middot; {{end}}{{$note.Key}}{{if $note.Detail}}: {{$note.Detail}}{{end}}{{end}}</p>{{end}}
{{end}}

{{if .Sortable}}<script>
document.querySelectorAll("th").forEach(function (th) {
  th.addEventListener("click", function () {
    var table = th.closest("table");
    var tbody = table.tBodies[0];
    var idx = th.cellIndex;
    var dir = th.dataset.dir === "desc" ? "asc" : "desc";
    table.querySelectorAll("th").forEach(function (o) { delete o.dataset.dir; });
    th.dataset.dir = dir;
    var rows = Array.prototype.slice.call(tbody.rows);
    rows.sort(function (a, b) {
      var av = a.cells[idx].dataset.v, bv = b.cells[idx].dataset.v;
      var cmp = av !== undefined && bv !== undefined
        ? Number(av) - Number(bv)
        : a.cells[idx].textContent.localeCompare(b.cells[idx].textContent);
      return dir === "desc" ? -cmp : cmp;
    });
    rows.forEach(function (r) { tbody.appendChild(r); });
  });
});
</script>{{end}}
</body>
</html>
`))
