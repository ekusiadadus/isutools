package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/safefs"
)

// diffRow separates traffic volume from per-operation latency. Total time is
// useful for bottleneck share, but only comparable as an improvement signal
// when request/query counts are equal.
type diffRow struct {
	Key        string
	ACount     int64
	BCount     int64
	AMs        float64
	BMs        float64
	DeltaMs    float64
	AAvgMs     float64
	BAvgMs     float64
	DeltaAvgMs float64
	Comparable bool
}

type diffPage struct {
	A, B              string
	AScore, BScore    string
	ProvenanceWarning string
	SQL, HTTP         []diffRow
	Contradictions    []diffContradiction
}

type diffEvidence struct {
	Signal, Metric string
	A, B           float64
	Formula        string
	Limitation     string
}

type diffContradiction struct {
	Label        string
	Outcome      diffEvidence
	Improvements []diffEvidence
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
		http.Error(w, "a and b must be valid run ids", http.StatusBadRequest)
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
		AScore:            snapA.Meta.Score,
		BScore:            snapB.Meta.Score,
		ProvenanceWarning: provenanceWarning(snapA.Meta, snapB.Meta),
		SQL:               diffEntries(snapA.SQL, snapB.SQL),
		HTTP:              diffEntries(entriesOf(snapA.HTTP), entriesOf(snapB.HTTP)),
		Contradictions:    detectContradictions(*snapA, *snapB),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := diffTmpl.Execute(w, page); err != nil {
		http.Error(w, "isutools: render failed", http.StatusInternalServerError)
	}
}

// detectContradictions reports the exact, intentionally conservative case in
// which at least one measured resource signal improves while a benchmark
// outcome regresses. It labels correlation only; changed load shape can
// explain both sides without either causing the other.
func detectContradictions(a, b Snapshot) []diffContradiction {
	improvements := resourceImprovements(a, b)
	if len(improvements) == 0 {
		return nil
	}
	outcomes := outcomeRegressions(a, b)
	rows := make([]diffContradiction, 0, len(outcomes))
	for _, outcome := range outcomes {
		rows = append(rows, diffContradiction{
			Label:   "resource-improved / outcome-regressed (correlation warning)",
			Outcome: outcome, Improvements: append([]diffEvidence(nil), improvements...),
		})
	}
	return rows
}

func resourceImprovements(a, b Snapshot) []diffEvidence {
	var evidence []diffEvidence
	aWait, aPools := totalPoolWait(a.DBPool)
	bWait, bPools := totalPoolWait(b.DBPool)
	if aPools && bPools && aWait > 0 && bWait < aWait {
		evidence = append(evidence, diffEvidence{
			Signal: "database/sql pool wait", Metric: "dbpool.wait_duration_ns",
			A: float64(aWait), B: float64(bWait), Formula: "B total wait duration < A total wait duration",
			Limitation: "summed goroutine wait time is concurrency-weighted and pool membership or traffic may differ",
		})
	}
	if a.Proc != nil && b.Proc != nil && a.Proc.CPUTotal != nil && b.Proc.CPUTotal != nil &&
		a.Proc.CPUTotal.IOWaitPercent > 0 && b.Proc.CPUTotal.IOWaitPercent < a.Proc.CPUTotal.IOWaitPercent {
		evidence = append(evidence, diffEvidence{
			Signal: "host iowait", Metric: "proc.cpu_total.iowait_percent",
			A: a.Proc.CPUTotal.IOWaitPercent, B: b.Proc.CPUTotal.IOWaitPercent,
			Formula:    "B interval iowait percent < A interval iowait percent",
			Limitation: "host-wide iowait is an interval ratio and does not identify the request or device responsible",
		})
	}
	if a.Proc != nil && b.Proc != nil && a.Proc.CPUTotal != nil && b.Proc.CPUTotal != nil &&
		a.Proc.CPUTotal.BusyPercent > 0 && b.Proc.CPUTotal.BusyPercent < a.Proc.CPUTotal.BusyPercent {
		evidence = append(evidence, diffEvidence{
			Signal: "host CPU busy", Metric: "proc.cpu_total.busy_percent",
			A: a.Proc.CPUTotal.BusyPercent, B: b.Proc.CPUTotal.BusyPercent,
			Formula:    "B interval busy percent < A interval busy percent",
			Limitation: "lower CPU can mean less useful work rather than higher efficiency; traffic shape and run duration may differ",
		})
	}
	evidence = append(evidence, comparableLatencyImprovements("SQL", "sql.avg_latency_ms", diffEntries(a.SQL, b.SQL))...)
	evidence = append(evidence, comparableLatencyImprovements("HTTP endpoint", "http.avg_latency_ms", diffEntries(entriesOf(a.HTTP), entriesOf(b.HTTP)))...)
	return evidence
}

func comparableLatencyImprovements(signal, metric string, rows []diffRow) []diffEvidence {
	const maxRows = 5
	evidence := make([]diffEvidence, 0, maxRows)
	for _, row := range rows {
		if !row.Comparable || row.AAvgMs <= 0 || row.BAvgMs >= row.AAvgMs {
			continue
		}
		evidence = append(evidence, diffEvidence{
			Signal: signal + ": " + row.Key, Metric: metric,
			A: row.AAvgMs, B: row.BAvgMs,
			Formula:    "A count == B count > 0 and B average latency < A average latency",
			Limitation: "equal counts do not prove equal concurrency, request parameters, cache state, payload size, or downstream work",
		})
		if len(evidence) == maxRows {
			break
		}
	}
	return evidence
}

func totalPoolWait(entries []dbpool.Entry) (int64, bool) {
	if len(entries) == 0 {
		return 0, false
	}
	var total int64
	for _, entry := range entries {
		if entry.Partial || entry.WaitDuration < 0 || int64(entry.WaitDuration) > math.MaxInt64-total {
			return 0, false
		}
		total += int64(entry.WaitDuration)
	}
	return total, true
}

func outcomeRegressions(a, b Snapshot) []diffEvidence {
	var evidence []diffEvidence
	if aScore, errA := strconv.ParseFloat(a.Meta.Score, 64); errA == nil {
		if bScore, errB := strconv.ParseFloat(b.Meta.Score, 64); errB == nil && bScore < aScore {
			evidence = append(evidence, diffEvidence{
				Signal: "benchmark score", Metric: "meta.score", A: aScore, B: bScore,
				Formula:    "numeric B score < numeric A score",
				Limitation: "score semantics and run-to-run variance are benchmark-defined; one pair is not a causal estimate",
			})
		}
	}
	if a.Meta.BenchmarkPass != nil && b.Meta.BenchmarkPass != nil && *a.Meta.BenchmarkPass && !*b.Meta.BenchmarkPass {
		evidence = append(evidence, diffEvidence{
			Signal: "benchmark pass", Metric: "meta.benchmark_pass", A: 1, B: 0,
			Formula:    "A pass=true and B pass=false",
			Limitation: "pass is caller-supplied through /save and its correctness rule belongs to the benchmark",
		})
	}
	if a.Meta.Run != nil && b.Meta.Run != nil {
		aRank, aOK := validityRank(a.Meta.Run.Validity)
		bRank, bOK := validityRank(b.Meta.Run.Validity)
		if aOK && bOK && bRank < aRank {
			evidence = append(evidence, diffEvidence{
				Signal: "measurement validity", Metric: "meta.run.validity", A: float64(aRank), B: float64(bRank),
				Formula:    "B validity rank < A validity rank (valid=2, partial=1, invalid=0)",
				Limitation: "measurement validity describes collection integrity, not benchmark correctness",
			})
		}
	}
	aSuccess, aErrors := httpOutcomeCounts(a.HTTP)
	bSuccess, bErrors := httpOutcomeCounts(b.HTTP)
	if aSuccess > 0 && bSuccess < aSuccess {
		evidence = append(evidence, diffEvidence{
			Signal: "successful HTTP completions", Metric: "http.status_2xx_3xx_count",
			A: float64(aSuccess), B: float64(bSuccess), Formula: "B 2xx/3xx completion count < A count",
			Limitation: "request mix, benchmark pacing, redirects, and run duration may differ",
		})
	}
	if bErrors > aErrors {
		evidence = append(evidence, diffEvidence{
			Signal: "HTTP server errors", Metric: "http.status_5xx_count",
			A: float64(aErrors), B: float64(bErrors), Formula: "B 5xx count > A 5xx count",
			Limitation: "request mix, benchmark pacing, and run duration may differ",
		})
	}
	return evidence
}

func validityRank(value string) (int, bool) {
	switch value {
	case "valid":
		return 2, true
	case "partial":
		return 1, true
	case "invalid":
		return 0, true
	default:
		return 0, false
	}
}

func httpOutcomeCounts(entries httpstats.Snapshot) (successes, errors int64) {
	for _, entry := range entries {
		if entry.Count <= 0 {
			continue
		}
		switch {
		case entry.Status >= 200 && entry.Status < 400:
			successes = saturatingCountAdd(successes, entry.Count)
		case entry.Status >= 500:
			errors = saturatingCountAdd(errors, entry.Count)
		}
	}
	return successes, errors
}

func saturatingCountAdd(current, value int64) int64 {
	if value <= 0 {
		return current
	}
	if current > math.MaxInt64-value {
		return math.MaxInt64
	}
	return current + value
}

func provenanceWarning(a, b Meta) string {
	if a.ProvenanceValid && b.ProvenanceValid {
		return ""
	}
	return "build provenance unverified: revision が unknown または dirty の run を含むため、差分を再現可能なビルド間比較とは断定できません。"
}

// loadRun reads a stored run's JSON snapshot by its timestamp id.
func (h *handler) loadRun(id string) (*Snapshot, error) {
	if h.p.DataDir == "" {
		return nil, fmt.Errorf("no data dir")
	}
	matches := make([]string, 0, 1)
	for _, entry := range h.dataEntries() {
		name := entry.name
		if strings.HasPrefix(name, id+"_") && strings.HasSuffix(name, ".json") {
			matches = append(matches, name)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("run %s not found", id)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("run %s is ambiguous (%d snapshots)", id, len(matches))
	}
	file, err := h.openDataRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := file.ReadFile(matches[0], maxSnapshotBytes)
	if err != nil {
		if errors.Is(err, safefs.ErrTooLarge) {
			return nil, errSnapshotTooLarge
		}
		return nil, err
	}
	snap := &Snapshot{}
	if err := json.Unmarshal(data, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func entriesOf(s httpstats.Snapshot) []agg.Entry {
	out := make([]agg.Entry, 0, len(s))
	for _, e := range s {
		out = append(out, agg.Entry{Key: e.Key, Count: e.Count, Total: e.Total})
	}
	return out
}

func diffEntries(a, b []agg.Entry) []diffRow {
	type values struct {
		aCount, bCount int64
		aTotal, bTotal float64
	}
	totals := map[string]values{}
	for _, e := range a {
		v := totals[e.Key]
		v.aCount = e.Count
		v.aTotal = float64(e.Total.Nanoseconds()) / 1e6
		totals[e.Key] = v
	}
	for _, e := range b {
		v := totals[e.Key]
		v.bCount = e.Count
		v.bTotal = float64(e.Total.Nanoseconds()) / 1e6
		totals[e.Key] = v
	}
	rows := make([]diffRow, 0, len(totals))
	for key, v := range totals {
		row := diffRow{
			Key: key, ACount: v.aCount, BCount: v.bCount,
			AMs: v.aTotal, BMs: v.bTotal, DeltaMs: v.bTotal - v.aTotal,
			Comparable: v.aCount > 0 && v.aCount == v.bCount,
		}
		if v.aCount > 0 {
			row.AAvgMs = v.aTotal / float64(v.aCount)
		}
		if v.bCount > 0 {
			row.BAvgMs = v.bTotal / float64(v.bCount)
		}
		row.DeltaAvgMs = row.BAvgMs - row.AAvgMs
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		iDelta, jDelta := math.Abs(rows[i].DeltaMs), math.Abs(rows[j].DeltaMs)
		if iDelta != jDelta {
			return iDelta > jDelta
		}
		return rows[i].Key < rows[j].Key
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
.warn { color: #b45309; font-weight: bold; }
.contradiction { border: 2px solid #b45309; padding: .6rem; margin: .8rem 0; }
.empty { color: #999; }
a { color: #0b57d0; }
</style>
</head>
<body>
<h1>diff: {{.A}} (score {{.AScore}}) &rarr; {{.B}} (score {{.BScore}})</h1>
<p class="meta">delta = B - A。合計時間の変化量順、上位30件。件数が異なる行では合計deltaを改善・悪化とは判定できません。avgと負荷条件を確認してください。<a href="./">&larr; runs</a></p>
{{if .ProvenanceWarning}}<p class="warn">{{.ProvenanceWarning}}</p>{{end}}
{{range .Contradictions}}<div class="contradiction">
<p class="warn">{{.Label}}</p>
<p class="meta">outcome: {{.Outcome.Signal}} ({{.Outcome.Metric}}) A={{ms1 .Outcome.A}} B={{ms1 .Outcome.B}}; {{.Outcome.Formula}}; limitation: {{.Outcome.Limitation}}</p>
{{range .Improvements}}<p class="meta">improved resource: {{.Signal}} ({{.Metric}}) A={{ms1 .A}} B={{ms1 .B}}; {{.Formula}}; limitation: {{.Limitation}}</p>{{end}}
</div>{{end}}

<h2>SQL</h2>
{{if .SQL}}
<table>
<thead><tr><th>total delta(ms)</th><th>A total(ms)</th><th>B total(ms)</th><th>A count</th><th>B count</th><th>A avg(ms)</th><th>B avg(ms)</th><th>avg delta(ms)</th><th>query</th></tr></thead>
<tbody>{{range .SQL}}<tr>
<td data-v="{{.DeltaMs}}">{{if .Comparable}}{{if gt .DeltaMs 0.0}}<span class="up">+{{ms1 .DeltaMs}}</span>{{else}}<span class="down">{{ms1 .DeltaMs}}</span>{{end}}{{else}}{{ms1 .DeltaMs}}*{{end}}</td>
<td>{{ms1 .AMs}}</td><td>{{ms1 .BMs}}</td><td>{{.ACount}}</td><td>{{.BCount}}</td><td>{{ms1 .AAvgMs}}</td><td>{{ms1 .BAvgMs}}</td><td>{{ms1 .DeltaAvgMs}}</td><td class="l">{{.Key}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no SQL data</p>{{end}}

<h2>HTTP</h2>
{{if .HTTP}}
<table>
<thead><tr><th>total delta(ms)</th><th>A total(ms)</th><th>B total(ms)</th><th>A count</th><th>B count</th><th>A avg(ms)</th><th>B avg(ms)</th><th>avg delta(ms)</th><th>request</th></tr></thead>
<tbody>{{range .HTTP}}<tr>
<td data-v="{{.DeltaMs}}">{{if .Comparable}}{{if gt .DeltaMs 0.0}}<span class="up">+{{ms1 .DeltaMs}}</span>{{else}}<span class="down">{{ms1 .DeltaMs}}</span>{{end}}{{else}}{{ms1 .DeltaMs}}*{{end}}</td>
<td>{{ms1 .AMs}}</td><td>{{ms1 .BMs}}</td><td>{{.ACount}}</td><td>{{.BCount}}</td><td>{{ms1 .AAvgMs}}</td><td>{{ms1 .BAvgMs}}</td><td>{{ms1 .DeltaAvgMs}}</td><td class="l">{{.Key}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no HTTP data</p>{{end}}
</body>
</html>
`))
