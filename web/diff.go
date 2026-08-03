package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
)

// diffRow is one key's total-time change between two runs.
type diffRow struct {
	Key     string
	AMs     float64
	BMs     float64
	DeltaMs float64
}

type diffPage struct {
	A, B           string
	AScore, BScore string
	SQL, HTTP      []diffRow
}

// diff renders GET /diff?a=<run-id>&b=<run-id>: which queries/paths got
// faster or slower between two stored runs — the "did the change work, or
// did the bottleneck just move?" view.
func (h *handler) diff(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	a, b := r.URL.Query().Get("a"), r.URL.Query().Get("b")
	if !runIDPattern.MatchString(a) || !runIDPattern.MatchString(b) {
		http.Error(w, "a and b must be run ids (YYYYMMDD-HHMMSS)", http.StatusBadRequest)
		return
	}
	snapA, err := h.loadRun(a)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	snapB, err := h.loadRun(b)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	page := diffPage{
		A: a, B: b,
		AScore: snapA.Meta.Score, BScore: snapB.Meta.Score,
		SQL:  diffEntries(snapA.SQL, snapB.SQL),
		HTTP: diffEntries(entriesOf(snapA.HTTP), entriesOf(snapB.HTTP)),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := diffTmpl.Execute(w, page); err != nil {
		http.Error(w, "isutools: render failed", http.StatusInternalServerError)
	}
}

// loadRun reads a stored run's JSON snapshot by its timestamp id.
func (h *handler) loadRun(id string) (*Snapshot, error) {
	if h.p.DataDir == "" {
		return nil, fmt.Errorf("no data dir")
	}
	entries, err := os.ReadDir(h.p.DataDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, id+"_") && strings.HasSuffix(name, ".json") {
			data, err := os.ReadFile(filepath.Join(h.p.DataDir, name))
			if err != nil {
				return nil, err
			}
			snap := &Snapshot{}
			if err := json.Unmarshal(data, snap); err != nil {
				return nil, err
			}
			return snap, nil
		}
	}
	return nil, fmt.Errorf("run %s not found", id)
}

func entriesOf(s httpstats.Snapshot) []agg.Entry {
	out := make([]agg.Entry, 0, len(s))
	for _, e := range s {
		out = append(out, agg.Entry{Key: e.Key, Count: e.Count, Total: e.Total})
	}
	return out
}

func diffEntries(a, b []agg.Entry) []diffRow {
	totals := map[string][2]float64{}
	for _, e := range a {
		v := totals[e.Key]
		v[0] = float64(e.Total.Nanoseconds()) / 1e6
		totals[e.Key] = v
	}
	for _, e := range b {
		v := totals[e.Key]
		v[1] = float64(e.Total.Nanoseconds()) / 1e6
		totals[e.Key] = v
	}
	rows := make([]diffRow, 0, len(totals))
	for key, v := range totals {
		rows = append(rows, diffRow{Key: key, AMs: v[0], BMs: v[1], DeltaMs: v[1] - v[0]})
	}
	sort.Slice(rows, func(i, j int) bool {
		return math.Abs(rows[i].DeltaMs) > math.Abs(rows[j].DeltaMs)
	})
	if len(rows) > 30 {
		rows = rows[:30]
	}
	return rows
}

var diffTmpl = template.Must(template.New("diff").Funcs(template.FuncMap{
	"ms1": func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
}).Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>isutools diff {{.A}} vs {{.B}}</title>
<style>
body { font-family: ui-monospace, monospace; margin: 1.5rem; background: #fff; color: #111; }
h1 { font-size: 1.2rem; margin: 0 0 .25rem; }
h2 { font-size: 1rem; margin: 1.5rem 0 .5rem; border-bottom: 2px solid #333; padding-bottom: .2rem; }
.meta { color: #666; margin: 0 0 .4rem; font-size: .85rem; }
table { border-collapse: collapse; width: 100%; font-size: .8rem; }
th, td { border: 1px solid #ddd; padding: .3rem .5rem; text-align: right; white-space: nowrap; }
th { background: #f5f5f5; }
td.l { text-align: left; white-space: normal; word-break: break-all; }
tbody tr:nth-child(odd) { background: #fafafa; }
.up { color: #b91c1c; } .down { color: #15803d; }
.empty { color: #999; }
a { color: #0b57d0; }
</style>
</head>
<body>
<h1>diff: {{.A}} (score {{.AScore}}) &rarr; {{.B}} (score {{.BScore}})</h1>
<p class="meta">delta = B - A(負 = 改善)。合計時間の変化量順、上位30件。<a href="./">&larr; runs</a></p>

<h2>SQL</h2>
{{if .SQL}}
<table>
<thead><tr><th>delta(ms)</th><th>A(ms)</th><th>B(ms)</th><th>query</th></tr></thead>
<tbody>{{range .SQL}}<tr>
<td data-v="{{.DeltaMs}}">{{if gt .DeltaMs 0.0}}<span class="up">+{{ms1 .DeltaMs}}</span>{{else}}<span class="down">{{ms1 .DeltaMs}}</span>{{end}}</td>
<td>{{ms1 .AMs}}</td><td>{{ms1 .BMs}}</td><td class="l">{{.Key}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no SQL data</p>{{end}}

<h2>HTTP</h2>
{{if .HTTP}}
<table>
<thead><tr><th>delta(ms)</th><th>A(ms)</th><th>B(ms)</th><th>request</th></tr></thead>
<tbody>{{range .HTTP}}<tr>
<td data-v="{{.DeltaMs}}">{{if gt .DeltaMs 0.0}}<span class="up">+{{ms1 .DeltaMs}}</span>{{else}}<span class="down">{{ms1 .DeltaMs}}</span>{{end}}</td>
<td>{{ms1 .AMs}}</td><td>{{ms1 .BMs}}</td><td class="l">{{.Key}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no HTTP data</p>{{end}}
</body>
</html>
`))
