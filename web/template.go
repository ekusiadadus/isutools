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
}).Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>isutools {{.Snapshot.Meta.Revision}}</title>
<style>
body { font-family: ui-monospace, monospace; margin: 1.5rem; background: #fff; color: #111; }
h1 { font-size: 1.2rem; margin: 0 0 .25rem; }
.meta { color: #666; margin: 0 0 1rem; font-size: .85rem; }
table { border-collapse: collapse; width: 100%; font-size: .8rem; }
th, td { border: 1px solid #ddd; padding: .3rem .5rem; text-align: right; white-space: nowrap; }
th { background: #f5f5f5; cursor: pointer; user-select: none; }
td.q { text-align: left; white-space: normal; word-break: break-all; }
tbody tr:nth-child(odd) { background: #fafafa; }
.empty { color: #999; }
</style>
</head>
<body>
<h1>isutools</h1>
<p class="meta">{{.Snapshot.Meta.Time}} &middot; rev {{.Snapshot.Meta.Revision}} &middot; gen {{.Snapshot.Meta.Generation}}</p>
<p class="meta">{{.Snapshot.Meta.Host.Hostname}} &middot; {{.Snapshot.Meta.Host.CPUModel}} &middot; {{.Snapshot.Meta.Host.NumCPU}} cores &middot; {{gb .Snapshot.Meta.Host.MemTotalBytes}} GB &middot; {{.Snapshot.Meta.Host.OS}}</p>
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
<td class="q">{{.Key}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}<p class="empty">no observations yet</p>{{end}}
<p class="meta">* p95 is a log2-bucket upper-bound approximation. SQL time = query dispatch until first response (row iteration excluded).</p>
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
