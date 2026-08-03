package web

import (
	"html/template"
	"strconv"
	"time"
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
<td class="l"><a href="files/{{.File}}">html</a> <a href="files/{{.JSON}}">json</a></td>
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

var reportTmpl = template.Must(template.New("report").Funcs(template.FuncMap{
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
}).Parse(`<!doctype html>
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

<h2>nginx Access Log</h2>
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
{{else}}<p class="empty">not configured (set ISUTOOLS_NGINX_LOG)</p>{{end}}

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
