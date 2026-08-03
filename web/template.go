package web

import (
	"html/template"
	"strconv"
	"time"
)

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
<h1>isutools{{if .Dashboard}} dashboard{{end}}</h1>
<p class="meta">{{.Snapshot.Meta.Time}} &middot; rev {{.Snapshot.Meta.Revision}} &middot; gen {{.Snapshot.Meta.Generation}}</p>
<p class="meta">{{.Snapshot.Meta.Host.Hostname}} &middot; {{.Snapshot.Meta.Host.CPUModel}} &middot; {{.Snapshot.Meta.Host.NumCPU}} cores &middot; {{gb .Snapshot.Meta.Host.MemTotalBytes}} GB &middot; {{.Snapshot.Meta.Host.OS}}</p>
<p class="meta">collectors: SQL &#10003; &middot; DB schema &#10003; &middot; HTTP (M2) &middot; process (M2) &middot; access log (M3)</p>

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
<th>total(ms)</th><th>count</th><th>avg(ms)</th><th>p95*(ms)</th><th>max(ms)</th><th>query</th>
</tr></thead>
<tbody>
{{range .Snapshot.SQL}}<tr>
<td data-v="{{.Total.Nanoseconds}}">{{ms .Total}}</td>
<td data-v="{{.Count}}">{{.Count}}</td>
<td data-v="{{.Avg.Nanoseconds}}">{{ms .Avg}}</td>
<td data-v="{{.P95.Nanoseconds}}">{{ms .P95}}</td>
<td data-v="{{.Max.Nanoseconds}}">{{ms .Max}}</td>
<td class="l">{{.Key}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}<p class="empty">no observations yet</p>{{end}}
<p class="meta">* p95 is a log2-bucket upper-bound approximation. SQL time = query dispatch until first response (row iteration excluded).</p>

{{if .Dashboard}}
<h2>Snapshots <span class="meta">(past results)</span></h2>
{{if .Files}}
<ul class="files">
{{range .Files}}<li><a href="files/{{.}}">{{.}}</a></li>{{end}}
</ul>
{{else}}<p class="empty">no saved snapshots yet (POST /save persists the current generation; set ISUTOOLS_DATA_DIR)</p>{{end}}
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
