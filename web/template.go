package web

import (
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/flowviz"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/queryplan"
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
<p class="meta">{{.Snapshot.Meta.Time}} &middot; rev {{.Snapshot.Meta.Revision}} ({{.Snapshot.Meta.BuildSource}}) &middot; gen {{.Snapshot.Meta.Generation}}{{if not .Snapshot.Meta.ProvenanceValid}} &middot; build provenance unverified{{end}}</p>
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
<td class="l"><a href="files/{{.File}}">html</a> <a href="{{.ID}}?view=current">current UI</a> <a href="files/{{.JSON}}">json</a>{{if .PrevID}} <a href="diff?a={{.PrevID}}&b={{.ID}}">diff</a>{{end}}</td>
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

<h2>External analysis <span class="meta">(verified current artifacts; restricted raw inputs are never linked)</span></h2>
{{if .External}}
<table>
<thead><tr><th>run</th><th>kind</th><th>status</th><th>analyzer</th><th>portable output</th></tr></thead>
<tbody>{{range .External}}<tr>
<td class="l">{{.Namespace}}</td><td class="l">{{.Kind}}</td><td class="l">{{.Status}}{{if .Code}} ({{.Code}}){{end}}</td>
<td class="l">{{.Analyzer.Name}} {{.Analyzer.Version}}</td><td class="l">{{range .Outputs}}<a href="files/{{.Name}}">{{.Role}}</a> {{else}}-{{end}}</td>
</tr>{{end}}</tbody>
</table>
<p class="meta"><a href="external-analysis">machine-readable verified index</a></p>
{{else}}<p class="empty">no verified external analysis artifacts yet</p>{{end}}

<h2>Trajectories <span class="meta">(post-benchmark agent / job animation)</span></h2>
{{if .Trajectories}}
<ul class="files">
{{range .Trajectories}}<li><a href="files/{{.}}">{{.}}</a></li>{{end}}
</ul>
{{else}}<p class="empty">no trajectory viewers yet (generate a trajectory_*.html with isutools-trajectory)</p>{{end}}
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

// bottleneckSignal is one evidence-backed triage row shown before the full
// report. It deliberately says where to look next rather than claiming that
// one metric proves causality.
type bottleneckSignal struct {
	Order      string
	Level      string
	Signal     string
	Evidence   string
	NextAction string
}

// bottleneckDiagnosis is the short, decision-oriented answer that precedes
// the evidence tables. It deliberately separates a measured system
// constraint from source-code localization: an expensive endpoint or query
// is a search key, while a file:line is only evidence after symbolized profile
// analysis with matching executable provenance.
type bottleneckDiagnosis struct {
	PrimaryLevel      string
	Primary           string
	PrimaryEvidence   string
	PrimaryAction     string
	PrimaryAnchor     string
	AmplifierLevel    string
	Amplifier         string
	AmplifierEvidence string
	HTTPSearchKey     string
	SQLSearchKey      string
	CodeLevel         string
	CodeTitle         string
	CodeEvidence      string
	CodeAction        string
}

func diagnoseBottleneck(snapshot Snapshot) bottleneckDiagnosis {
	diagnosis := bottleneckDiagnosis{
		PrimaryLevel:    "warn",
		Primary:         "第一修正候補はまだ絞れません",
		PrimaryEvidence: "resource / HTTP / SQL の同一区間データが不足しています。",
		PrimaryAction:   "POST /reset → benchmark → POST /save で一つの計測区間を作ります。",
		PrimaryAnchor:   "bottleneck-overview",
		AmplifierLevel:  "warn",
		Amplifier:       "負荷を増幅している操作は未判定です",
	}

	var waits int64
	var waitDuration time.Duration
	var open, maxOpen int
	for _, entry := range snapshot.DBPool {
		waits += entry.WaitCount
		waitDuration += entry.WaitDuration
		open += entry.Open
		maxOpen += entry.MaxOpen
	}
	if waits > 0 {
		avg := waitDuration / time.Duration(waits)
		diagnosis.PrimaryLevel = "hot"
		diagnosis.Primary = "第一修正候補: DB接続プール待ちを減らす"
		diagnosis.PrimaryEvidence = fmt.Sprintf("wait %s回・累計%s・平均%s、open %d / max %d。requestが接続取得前に待っています。", humanCount(waits), humanDuration(waitDuration), humanDuration(avg), open, maxOpen)
		diagnosis.PrimaryAction = "DB Poolのtarget別waitと、SQL/handler内で接続を保持する区間を確認します。上限を増やすだけでなく、transaction範囲とquery回数を先に縮めます。"
		diagnosis.PrimaryAnchor = "db-pool"
	} else if snapshot.Proc != nil && snapshot.Proc.CPUTotal != nil && procIntervalMatchesRun(snapshot) && snapshot.Proc.CPUTotal.BusyPercent >= 90 {
		diagnosis.PrimaryLevel = "hot"
		diagnosis.Primary = "第一修正候補: CPU hot pathを短くする"
		diagnosis.PrimaryEvidence = fmt.Sprintf("run区間のCPU busy %.1f%%。", snapshot.Proc.CPUTotal.BusyPercent)
		diagnosis.PrimaryAction = "CPU profileのfunctions → linesの順で、累積値が大きい呼び出し元へ降ります。"
		diagnosis.PrimaryAnchor = "profiles"
	} else if len(snapshot.SQL) > 0 {
		top := topSQLDemand(snapshot.SQL)
		diagnosis.PrimaryLevel = "warn"
		diagnosis.Primary = "第一修正候補: 累計DB時間が最大のqueryを減らす"
		diagnosis.PrimaryEvidence = fmt.Sprintf("count %s・累計%s・%s", humanCount(top.Count), humanDuration(top.Total), truncateRunes(top.Key, 100))
		diagnosis.PrimaryAction = "HTTP一回あたりの発行回数、rows examined、Query Planを照合します。"
		diagnosis.PrimaryAnchor = "sql"
	}

	if len(snapshot.HTTP) > 0 {
		top := topHTTPDemand(snapshot.HTTP)
		diagnosis.HTTPSearchKey = httpRouteSearchKey(top.Key)
		diagnosis.AmplifierLevel = "warn"
		diagnosis.Amplifier = "負荷増幅候補: 累計HTTP時間が最大のendpoint"
		if looksLikeLongPoll(diagnosis.HTTPSearchKey) {
			diagnosis.Amplifier = "負荷増幅候補: long-poll候補のendpoint"
		}
		diagnosis.AmplifierEvidence = fmt.Sprintf("%s・count %s・avg %s・p95 %s。待ち時間を含むtotalはhot codeの証明ではありません。", top.Key, humanCount(top.Count), humanDuration(top.Avg), humanDuration(top.P95))
	}
	if len(snapshot.SQL) > 0 {
		diagnosis.SQLSearchKey = truncateRunes(topSQLDemand(snapshot.SQL).Key, 160)
	}

	diagnosis.CodeLevel, diagnosis.CodeTitle, diagnosis.CodeEvidence, diagnosis.CodeAction = codeEvidence(snapshot.Meta.Profiles)
	return diagnosis
}

func topHTTPDemand(entries httpstats.Snapshot) httpstats.Entry {
	top := entries[0]
	for _, entry := range entries[1:] {
		if entry.Total > top.Total {
			top = entry
		}
	}
	return top
}

func topSQLDemand(entries []agg.Entry) agg.Entry {
	top := entries[0]
	for _, entry := range entries[1:] {
		if entry.Total > top.Total {
			top = entry
		}
	}
	return top
}

func httpRouteSearchKey(key string) string {
	fields := strings.Fields(key)
	if len(fields) >= 2 {
		return fields[1]
	}
	return truncateRunes(key, 120)
}

func looksLikeLongPoll(route string) bool {
	route = strings.ToLower(route)
	return strings.Contains(route, "notification") || strings.Contains(route, "poll") ||
		strings.Contains(route, "stream") || strings.Contains(route, "events")
}

func codeEvidence(manifest *ProfileManifest) (level, title, evidence, action string) {
	const requirement = "ソースコードの行番号を断定するには、symbol付きprofile解析とcapture時binaryの一致確認が必要です。"
	if manifest == nil || manifest.CPU == nil {
		availablePairs := ""
		if manifest != nil && len(manifest.Pairs) > 0 {
			availablePairs = " mutex/block/heapの差分解析から待ち・allocationの候補行は探せますが、CPU hot lineの証明にはなりません。"
		}
		return "warn", "コード位置: このrunでは未特定",
			"このrunにはCPU profileがありません。そのため、HTTP/SQLの検索候補は出せても、CPUを実際に消費したソースコードの行番号を断定できません。" + availablePairs,
			"次のrunでrun CPU profileを採取し、isutools-pprofでlines集計を生成します。" + requirement
	}
	cpu := manifest.CPU
	if cpu.Status != "published" || cpu.File == "" {
		code := cpu.Code
		if code == "" {
			code = "unknown"
		}
		if code == "cpu-busy" {
			return "warn", "コード位置: CPU profileなし（採取が競合）",
				fmt.Sprintf("このrunでは、同じGoプロセスのCPU profilerが別のCPU profile採取（前のrun、または手動 /pprof/profile）に使われていたため、新しい採取を開始できませんでした（status=%s / code=%s）。これはCPU使用率が高すぎることが原因ではありません。", cpu.Status, code),
				"別の採取が終わるのを待ち、同時に1本だけにします。次に 1) POST /reset 2) 応答が X-Isutools-CPU-Profile-State: capturing であることを確認 3) ベンチ実行 4) POST /save 5) 保存したrunをisutools-pprofで解析、の順で再計測します。再びcpu-busyなら、手動profileの実行中プロセスか前runの停止待ちをProfilesで確認します。" + requirement
		}
		return "hot", "コード位置: CPU profile採取失敗",
			fmt.Sprintf("CPU profileはstatus=%s / code=%sで、行解析に使えるartifactがありません。", cpu.Status, code),
			"Profilesのcapture状態を直してから再計測します。" + requirement
	}
	return "ok", "コード位置: CPU artifactあり",
		fmt.Sprintf("%s を採取済みです。artifactだけでは行番号は未検証です。", cpu.File),
		"このrunの「行解析結果」にverified analysisがあればfunction / file / lineを確認します。なければisutools-pprofを実行してから開き直します。" + requirement
}

func humanCount(value int64) string {
	s := strconv.FormatInt(value, 10)
	if len(s) <= 3 {
		return s
	}
	start := len(s) % 3
	if start == 0 {
		start = 3
	}
	var out strings.Builder
	out.Grow(len(s) + len(s)/3)
	out.WriteString(s[:start])
	for index := start; index < len(s); index += 3 {
		out.WriteByte(',')
		out.WriteString(s[index : index+3])
	}
	return out.String()
}

// bottleneckOverview reduces the full report to the first checks that answer
// two questions: where request time accumulates, and whether a resource limit
// can explain it. Every row remains traceable to a detailed section below.
func bottleneckOverview(snapshot Snapshot) []bottleneckSignal {
	rows := make([]bottleneckSignal, 0, 8)

	if len(snapshot.HTTP) > 0 {
		top := topHTTPDemand(snapshot.HTTP)
		rows = append(rows, bottleneckSignal{
			Order:      "1",
			Level:      "hot",
			Signal:     "HTTP demand",
			Evidence:   fmt.Sprintf("%s · count %d · total %s · avg %s · p95 %s", top.Key, top.Count, humanDuration(top.Total), humanDuration(top.Avg), humanDuration(top.P95)),
			NextAction: "累計時間が最大の endpoint。polling 回数、handler 内の fan-out、待ちを最初に確認します。total は並行リクエストの合計で、run の経過時間とは別です。",
		})
	}

	if len(snapshot.SQL) > 0 {
		top := topSQLDemand(snapshot.SQL)
		rows = append(rows, bottleneckSignal{
			Order:      "2",
			Level:      "hot",
			Signal:     "SQL demand",
			Evidence:   fmt.Sprintf("count %d · total %s · avg %s · p95 %s · %s", top.Count, humanDuration(top.Total), humanDuration(top.Avg), humanDuration(top.P95), truncateRunes(top.Key, 100)),
			NextAction: "累計DB時間が最大の query。HTTP 1回あたりの実行回数と、SQL 行効率・Query Plans を照合します。",
		})
	}

	if row, ok := requestFailureSignal(snapshot); ok {
		rows = append(rows, row)
	}
	if row, ok := cpuSignal(snapshot); ok {
		rows = append(rows, row)
	}
	if row, ok := dbPoolSignal(snapshot); ok {
		rows = append(rows, row)
	}
	if row, ok := sqlRowsSignal(snapshot); ok {
		rows = append(rows, row)
	}
	if row, ok := hostIOSignal(snapshot); ok {
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		rows = append(rows, bottleneckSignal{
			Order:      "-",
			Level:      "warn",
			Signal:     "measurement unavailable",
			Evidence:   "HTTP / SQL / process / host sections are empty",
			NextAction: "POST /reset → benchmark → POST /save の順で計測区間を作成します。",
		})
	}
	return rows
}

func requestFailureSignal(snapshot Snapshot) (bottleneckSignal, bool) {
	var requests, status5xx, status499 int64
	if snapshot.AccessLog != nil && len(snapshot.AccessLog.Entries) > 0 {
		for _, entry := range snapshot.AccessLog.Entries {
			requests += entry.Count
			status5xx += entry.Status5xx
			status499 += entry.Status499
		}
	} else {
		for _, entry := range snapshot.HTTP {
			requests += entry.Count
			if entry.Status >= 500 {
				status5xx += entry.Count
			}
		}
	}
	if requests == 0 {
		return bottleneckSignal{}, false
	}

	level := "ok"
	next := "観測された応答に 5xx はありません。proxy に到達しなかった request はこの数字だけでは判定できません。"
	if status5xx > 0 {
		level = "hot"
		next = "5xx の URI と upstream timing を確認し、app 未到達・接続枯渇・handler error を切り分けます。"
	} else if status499 > 0 {
		level = "warn"
		next = "client abort (499) があります。長い p95 と同じ URI かを確認します。"
	}
	return bottleneckSignal{
		Order:      "gate",
		Level:      level,
		Signal:     "unserved / failed",
		Evidence:   fmt.Sprintf("observed %d · 5xx %d · 499 %d", requests, status5xx, status499),
		NextAction: next,
	}, true
}

func cpuSignal(snapshot Snapshot) (bottleneckSignal, bool) {
	if snapshot.Proc == nil || snapshot.Proc.CPUTotal == nil {
		return bottleneckSignal{}, false
	}
	cpu := snapshot.Proc.CPUTotal
	evidence := fmt.Sprintf("busy %.1f%% · user %.1f%% · sys %.1f%% · iowait %.1f%% · idle %.1f%%", cpu.BusyPercent, cpu.UserPercent, cpu.SystemPercent, cpu.IOWaitPercent, cpu.IdlePercent)
	if !procIntervalMatchesRun(snapshot) {
		return bottleneckSignal{
			Order:      "capacity",
			Level:      "warn",
			Signal:     "CPU interval",
			Evidence:   fmt.Sprintf("区間不一致: proc %s→%s · run %s→%s · %s", clockTime(snapshot.Proc.StartedAt), clockTime(snapshot.Proc.EndedAt), clockTime(snapshot.Meta.Run.StartedAt), clockTime(snapshot.Meta.Run.FinishedAt), evidence),
			NextAction: "この CPU% では run の飽和を判断できません。proc collector を run boundary で reset/freeze します。",
		}, true
	}

	level := "ok"
	next := "CPU には余力があります。直列待ち、外部I/O、pool、polling/fan-out を優先します。"
	if cpu.BusyPercent >= 90 {
		level = "hot"
		next = "CPU 飽和候補です。Top CPU と pprof の CPU profile で関数へ降ります。"
	} else if cpu.BusyPercent >= 70 {
		level = "warn"
		next = "CPU 使用率は高めです。Top CPU と pressure を確認します。"
	}
	return bottleneckSignal{Order: "capacity", Level: level, Signal: "CPU", Evidence: evidence, NextAction: next}, true
}

func procIntervalMatchesRun(snapshot Snapshot) bool {
	if snapshot.Proc == nil || snapshot.Meta.Run == nil || snapshot.Meta.Run.StartedAt.IsZero() || snapshot.Meta.Run.FinishedAt.IsZero() {
		return true
	}
	const tolerance = 5 * time.Second
	return absDuration(snapshot.Proc.StartedAt.Sub(snapshot.Meta.Run.StartedAt)) <= tolerance &&
		absDuration(snapshot.Proc.EndedAt.Sub(snapshot.Meta.Run.FinishedAt)) <= tolerance
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func dbPoolSignal(snapshot Snapshot) (bottleneckSignal, bool) {
	if len(snapshot.DBPool) == 0 {
		return bottleneckSignal{}, false
	}
	var waits int64
	var waitDuration time.Duration
	var open, maxOpen int
	for _, entry := range snapshot.DBPool {
		waits += entry.WaitCount
		waitDuration += entry.WaitDuration
		open += entry.Open
		maxOpen += entry.MaxOpen
	}
	level := "ok"
	next := "pool wait はありません。DB接続上限は現在の主要因ではありません。"
	if waits > 0 {
		level = "hot"
		next = "pool 上限が request latency を決めています。平均wait、in-use、DB max_connections を確認します。"
	}
	avg := time.Duration(0)
	if waits > 0 {
		avg = waitDuration / time.Duration(waits)
	}
	return bottleneckSignal{
		Order:      "capacity",
		Level:      level,
		Signal:     "DB pool",
		Evidence:   fmt.Sprintf("waits %d · total %s · avg %s · open %d / max %d", waits, humanDuration(waitDuration), humanDuration(avg), open, maxOpen),
		NextAction: next,
	}, true
}

func sqlRowsSignal(snapshot Snapshot) (bottleneckSignal, bool) {
	if snapshot.SQLRows == nil {
		return bottleneckSignal{}, false
	}
	var query string
	var total time.Duration
	var ratio float64
	var examined, sent, noIndex uint64
	for _, target := range snapshot.SQLRows.Targets {
		for _, digest := range target.Digests {
			problem := (digest.HasRatio && digest.ExaminedPerSent > sqlRowsRatioWarn) || digest.NoIndexUsed > 0 || digest.NoGoodIndexUsed > 0 || digest.SortMergePasses > 0 || digest.CreatedTmpDiskTables > 0
			if !problem || (query != "" && digest.TotalTime <= total) {
				continue
			}
			query = digest.Query
			total = digest.TotalTime
			ratio = digest.ExaminedPerSent
			examined = digest.RowsExamined
			sent = digest.RowsSent
			noIndex = digest.NoIndexUsed
		}
	}
	if query == "" {
		return bottleneckSignal{}, false
	}
	return bottleneckSignal{
		Order:      "scan",
		Level:      "hot",
		Signal:     "SQL row efficiency",
		Evidence:   fmt.Sprintf("total %s · examined/sent %.1f · rows %d/%d · no-index %d · %s", humanDuration(total), ratio, examined, sent, noIndex, truncateRunes(query, 32)),
		NextAction: "返す行より読む行が多い query です。filter/order と複合index、pollで0行を繰り返す設計を区別します。",
	}, true
}

func hostIOSignal(snapshot Snapshot) (bottleneckSignal, bool) {
	if snapshot.Host == nil {
		return bottleneckSignal{}, false
	}
	var device string
	var maxUtil float64
	for _, disk := range snapshot.Host.Disks {
		if disk.UtilPercent != nil && (device == "" || *disk.UtilPercent > maxUtil) {
			device, maxUtil = disk.Device, *disk.UtilPercent
		}
	}
	var ioStall float64
	if snapshot.Host.PSI != nil && snapshot.Host.PSI.IO.SomeStallRatio != nil {
		ioStall = *snapshot.Host.PSI.IO.SomeStallRatio
	}
	level := "ok"
	next := "強い I/O pressure は見えていません。disk util は multi-queue では飽和率そのものではありません。"
	if maxUtil >= 80 || ioStall >= 0.10 {
		level = "hot"
		next = "I/O 待ち候補です。device、queue、PSI、SQLの読み書き量を同じ区間で確認します。"
	} else if maxUtil >= 50 || ioStall >= 0.02 || snapshot.Host.Memory.PageMajorFaults > 100 {
		level = "warn"
		next = "I/O またはメモリ pressure の兆候があります。Host 詳細で区間deltaを確認します。"
	}
	if device == "" {
		device = "-"
	}
	return bottleneckSignal{
		Order:      "capacity",
		Level:      level,
		Signal:     "Host I/O",
		Evidence:   fmt.Sprintf("max disk util %.1f%% (%s) · IO PSI stall %.1f%% · major faults %d", maxUtil, device, ioStall*100, snapshot.Host.Memory.PageMajorFaults),
		NextAction: next,
	}, true
}

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

// planNullCell is what a NULL EXPLAIN column renders as. Every column of
// MySQL's classic EXPLAIN output can be NULL, and the one thing it must never
// print as is Go's "<nil>": an em dash reads as "the server reported nothing
// here", which is what a NULL means.
const planNullCell = "—"

// planQueryRunes bounds the statement text in a query-plan cell. The full text
// stays in the cell's title attribute like the SQL 行効率 table's does.
const planQueryRunes = 90

// planTable is one target's query plans, prepared for rendering.
type planTable struct {
	TargetID string
	Schema   string
	Lines    []planLine
}

// planLine is one row of the Query Plans table: one row of one digest's
// EXPLAIN output, or the single line a digest with no plan contributes.
//
// The columns are rendered into strings here rather than in the template
// because the interesting cases — a NULL column, a plan that failed, a sample
// the run cannot vouch for — are decisions, and a decision buried in template
// syntax cannot be tested on its own.
type planLine struct {
	Query  string
	Digest string
	// Freshness is the Japanese label for this sample's verdict, and Fresh is
	// false for anything the advisor is not allowed to judge.
	Freshness string
	Fresh     bool

	SelectType   string
	Table        string
	Type         string
	Key          string
	PossibleKeys string
	Rows         string
	// RowsSort is the sortable numeric value behind Rows, -1 when the server
	// reported none, so a NULL sorts apart from a genuine zero.
	RowsSort int64
	Extra    string

	// The three access-path defects the plan checks warn about, kept as flags
	// so the cell that carries the evidence is the cell that is highlighted.
	FullScan  bool
	Filesort  bool
	Temporary bool

	// Note explains a digest with no EXPLAIN row at all.
	Note string
}

// Hot reports a line worth highlighting as a whole.
func (l planLine) Hot() bool { return l.FullScan || l.Filesort || l.Temporary }

// planTables prepares the captured section for rendering, dropping every
// target that produced no plan.
//
// The dropping is what makes the section disappear entirely on a run where
// EXPLAIN captured nothing: a heading over an empty table would suggest the
// statements had no plan, when in fact none was taken. The reasons are not
// lost — they reach the reader through the Collector Health table, which is
// where the other collectors' skips are read too.
func planTables(section *queryplan.Section) []planTable {
	if section == nil {
		return nil
	}
	tables := make([]planTable, 0, len(section.Targets))
	for _, target := range section.Targets {
		if len(target.Plans) == 0 {
			continue
		}
		tables = append(tables, planTable{
			TargetID: target.TargetID,
			Schema:   target.Schema,
			Lines:    planLines(target.Plans),
		})
	}
	if len(tables) == 0 {
		return nil
	}
	return tables
}

// planLines flattens one target's plans into table rows.
//
// The statement is repeated on every row of a digest rather than written once
// and left blank below, because the table is sortable: a click on a header
// reorders the rows, and a cell whose meaning depends on the row above it
// would then belong to the wrong statement.
func planLines(plans []queryplan.Plan) []planLine {
	lines := make([]planLine, 0, len(plans))
	for _, plan := range plans {
		fresh := plan.Freshness == queryplan.FreshnessFresh
		base := planLine{
			Query:     plan.Query,
			Digest:    plan.Digest,
			Freshness: planFreshnessLabel(plan),
			Fresh:     fresh,
		}
		if len(plan.Rows) == 0 {
			base.Note = planErrorLabel(plan.Err)
			base.RowsSort = -1
			base.SelectType, base.Table = planNullCell, planNullCell
			base.Type, base.Key, base.PossibleKeys = planNullCell, planNullCell, planNullCell
			base.Rows, base.Extra = planNullCell, planNullCell
			lines = append(lines, base)
			continue
		}
		for _, row := range plan.Rows {
			line := base
			line.SelectType = planCell(row.SelectType)
			line.Table = planCell(row.Table)
			line.Type = planCell(row.Type)
			line.Key = planCell(row.Key)
			line.PossibleKeys = planCell(row.PossibleKeys)
			line.Rows, line.RowsSort = planRowsCell(row.Rows)
			line.Extra = planCell(row.Extra)
			// Only a fresh plan is highlighted. A stale or unjudgeable sample
			// may have run with a different literal, and a warning colour on a
			// plan this run never took is an invitation to tune the wrong
			// statement.
			line.FullScan = fresh && planIsFullScan(row.Type)
			line.Filesort = fresh && planHasExtra(row.Extra, "using filesort")
			line.Temporary = fresh && planHasExtra(row.Extra, "using temporary")
			lines = append(lines, line)
		}
	}
	return lines
}

// planCell renders a nullable EXPLAIN column. These carry schema identifiers —
// table names, index names, optimizer notes — never a statement's literals.
func planCell(v *string) string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return planNullCell
	}
	return strings.TrimSpace(*v)
}

// planRowsCell renders the row estimate together with its sort key.
func planRowsCell(v *int64) (string, int64) {
	if v == nil {
		return planNullCell, -1
	}
	return strconv.FormatInt(*v, 10), *v
}

func planIsFullScan(access *string) bool {
	return access != nil && strings.EqualFold(strings.TrimSpace(*access), "ALL")
}

func planHasExtra(extra *string, flag string) bool {
	return extra != nil && strings.Contains(strings.ToLower(*extra), flag)
}

// planFreshnessLabel says whether the sample belongs to the measured interval,
// and when it does not, why that could not be established.
//
// The reason is a closed enum on the producing side. An unrecognized value is
// reported generically rather than echoed, so a future producer cannot put
// unvetted text on the page through this path.
func planFreshnessLabel(plan queryplan.Plan) string {
	switch plan.FreshReason {
	case queryplan.FreshInInterval:
		if plan.Freshness == queryplan.FreshnessFresh {
			return "計測区間内"
		}
	case queryplan.FreshBeforeInterval:
		return "区間より前"
	case queryplan.FreshAfterInterval:
		return "区間より後"
	case queryplan.FreshClockAnomaly:
		return "DB 時計異常のため判定不能"
	case queryplan.FreshClockMissing:
		return "DB 側時計情報なしのため判定不能"
	case queryplan.FreshRunPartial:
		return "区間が partial のため判定不能"
	case queryplan.FreshIntervalShort:
		return "区間が短すぎて判定不能"
	}
	switch plan.Freshness {
	case queryplan.FreshnessFresh:
		return "計測区間内"
	case queryplan.FreshnessStale:
		return "計測区間外"
	default:
		return "判定不能"
	}
}

// planErrorLabel renders why a digest has no plan. Like the freshness reason
// it is a closed enum, mapped here rather than printed.
func planErrorLabel(err *queryplan.PlanError) string {
	if err == nil {
		return "実行計画の行なし"
	}
	switch err.Class {
	case queryplan.PlanErrTimeout:
		return "タイムアウト"
	case queryplan.PlanErrBudgetExhausted:
		return "時間予算切れ"
	case queryplan.PlanErrPermission:
		return "権限不足"
	case queryplan.PlanErrSyntax:
		return "構文エラーまたはサンプル切り詰め"
	case queryplan.PlanErrObjectMissing:
		return "対象オブジェクトなし"
	case queryplan.PlanErrSampleUnavail:
		return "サンプルなし"
	case queryplan.PlanErrSampleTruncated:
		return "サンプル切り詰めの疑い"
	case queryplan.PlanErrConnection:
		return "接続エラー"
	default:
		return "その他"
	}
}

func barrierWindow(window [2]time.Time) string {
	if window[0].IsZero() || window[1].IsZero() {
		return "-"
	}
	duration := window[1].Sub(window[0])
	if duration < 0 {
		return "invalid"
	}
	return fmt.Sprintf("%s → %s (%s)", window[0].UTC().Format("15:04:05.000000Z"), window[1].UTC().Format("15:04:05.000000Z"), humanDuration(duration))
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
	"flowGraph":    buildFlowGraphView,
	"flowHeatmap":  buildFlowHeatmap,
	"safeFlowViz":  boundedFlowVisualization,
	"pctBP":        percentBasisPoints,
	"flowVizReady": func(value *flowviz.Snapshot) bool { return value != nil && value.Status != flowviz.StatusDisabled },
	"size":         humanBytes,
	"sizeDelta":    humanBytesDelta,
	"dur":          humanDuration,
	// ns renders a nanosecond count the profile records carry as raw integers,
	// so a capture's distance from its boundary reads in the same units as
	// every other duration on the page.
	"ns":           func(nanos int64) string { return humanDuration(time.Duration(nanos)) },
	"barrier":      barrierWindow,
	"pf1":          optFloat,
	"psize":        optBytes,
	"clock":        clockTime,
	"cut":          truncateRunes,
	"byTime":       digestsByTotalTime,
	"ratio":        digestRatio,
	"ratioHot":     digestRatioHot,
	"indexHot":     digestIndexHot,
	"planTables":   planTables,
	"bottlenecks":  bottleneckOverview,
	"diskUtilNote": func() string { return hoststats.DiskUtilNote },
	"cgroupNote":   func() string { return hoststats.CGroupScopeNote },
	"diagnosis":    diagnoseBottleneck,
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
tr.stale > td { color: #999; }
td.flag { color: #b45309; font-weight: bold; }
.signal { display: inline-block; border-radius: .25rem; padding: .1rem .35rem; font-weight: bold; }
.signal.hot { color: #9a3412; background: #ffedd5; }
.signal.warn { color: #92400e; background: #fef3c7; }
.signal.ok { color: #166534; background: #dcfce7; }
.decision-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(18rem, 1fr)); gap: .75rem; margin: .75rem 0; }
.decision { border: 1px solid #d1d5db; border-left-width: .4rem; border-radius: .35rem; padding: .75rem; background: #fafafa; }
.decision.hot { border-left-color: #ea580c; background: #fff7ed; }
.decision.warn { border-left-color: #d97706; background: #fffbeb; }
.decision.ok { border-left-color: #16a34a; background: #f0fdf4; }
.decision h3 { font-size: .9rem; margin: 0 0 .4rem; }
.decision p { font-size: .82rem; line-height: 1.45; margin: .25rem 0; }
.jump { font-size: .82rem; line-height: 1.8; }
.jump-link { color: #0b57d0; font: inherit; text-decoration: underline; cursor: pointer; }
.search-key { display: block; margin-top: .2rem; padding: .25rem .4rem; background: #f3f4f6; border-radius: .2rem; overflow-wrap: anywhere; }
details { font-size: .8rem; margin: .4rem 0; }
summary { cursor: pointer; color: #666; }
ul.files { font-size: .85rem; line-height: 1.7; padding-left: 1.2rem; }
pre.cmd { font-size: .8rem; margin: .2rem 0 .8rem; white-space: pre-wrap; word-break: break-all; }
.flow-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(28rem, 1fr)); gap: 1rem; align-items: start; }
.flow-card { border: 1px solid #d1d5db; border-radius: .35rem; padding: .7rem; overflow-x: auto; }
.flow-card h3 { font-size: .9rem; margin: 0 0 .4rem; }
.funnel-step { display: grid; grid-template-columns: minmax(10rem, 1fr) minmax(12rem, 2fr) auto; gap: .6rem; align-items: center; margin: .35rem 0; }
.funnel-step progress { width: 100%; height: 1.15rem; accent-color: #2563eb; }
.flow-svg { width: 100%; min-width: 36rem; height: auto; background: #fafafa; border: 1px solid #e5e7eb; }
.flow-svg line, .flow-svg path.flow-edge { stroke: #64748b; opacity: .55; }
.flow-svg circle { fill: #dbeafe; stroke: #2563eb; stroke-width: 2; }
.flow-svg text { font-size: 11px; text-anchor: middle; dominant-baseline: middle; }
.heatmap { width: auto; max-width: 100%; }
.heatmap th { writing-mode: vertical-rl; max-height: 10rem; text-align: left; }
.heatmap td { min-width: 2.2rem; text-align: center; }
.heat0 { background: #fff; color: #aaa; } .heat1 { background: #eff6ff; } .heat2 { background: #bfdbfe; }
.heat3 { background: #93c5fd; } .heat4 { background: #3b82f6; color: #fff; } .heat5 { background: #1d4ed8; color: #fff; font-weight: bold; }
</style>
</head>
<body>
<h1>isutools report{{if .Snapshot.Meta.Score}} — score {{.Snapshot.Meta.Score}}{{end}}</h1>
<p class="meta">{{.Snapshot.Meta.Time}} &middot; rev {{.Snapshot.Meta.Revision}} ({{.Snapshot.Meta.BuildSource}}) &middot; gen {{.Snapshot.Meta.Generation}}{{if .Snapshot.Meta.Score}} &middot; score {{.Snapshot.Meta.Score}}{{end}}{{if not .Snapshot.Meta.ProvenanceValid}} &middot; build provenance unverified{{end}}</p>
{{with .Snapshot.Meta.BenchmarkPass}}<p class="meta">benchmark pass: {{.}}</p>{{end}}
<p class="meta">{{.Snapshot.Meta.Host.Hostname}} &middot; {{.Snapshot.Meta.Host.CPUModel}} &middot; {{.Snapshot.Meta.Host.NumCPU}} cores &middot; {{gb .Snapshot.Meta.Host.MemTotalBytes}} GB &middot; {{.Snapshot.Meta.Host.OS}}</p>
<p class="meta">collectors: SQL &middot; DB schema &middot; HTTP &middot; process &middot; nginx access log</p>

{{$diagnosis := diagnosis .Snapshot}}
<span id="diagnosis"></span><h2>結論: 次に修正する場所 <span class="meta">(実測と候補を分離)</span></h2>
<p class="meta">この要約は修正の探索順を示します。候補のendpoint/queryと、profileで検証済みのソース行は同じ意味ではありません。</p>
<div class="decision-grid">
<article class="decision {{$diagnosis.PrimaryLevel}}">
<h3>{{$diagnosis.Primary}}</h3>
<p><strong>根拠:</strong> {{$diagnosis.PrimaryEvidence}}</p>
<p><strong>次:</strong> {{$diagnosis.PrimaryAction}} <a class="jump-link" href="#{{$diagnosis.PrimaryAnchor}}" data-target="{{$diagnosis.PrimaryAnchor}}">詳細を見る</a></p>
</article>
<article class="decision {{$diagnosis.AmplifierLevel}}">
<h3>{{$diagnosis.Amplifier}}</h3>
<p>{{$diagnosis.AmplifierEvidence}}</p>
{{if $diagnosis.HTTPSearchKey}}<p><strong>route検索キー:</strong><code class="search-key">{{$diagnosis.HTTPSearchKey}}</code></p>{{end}}
{{if $diagnosis.SQLSearchKey}}<p><strong>SQL検索キー:</strong><code class="search-key">{{$diagnosis.SQLSearchKey}}</code></p>{{end}}
<p><a class="jump-link" href="#http" data-target="http">HTTP</a> &middot; <a class="jump-link" href="#sql" data-target="sql">SQL</a></p>
</article>
<article class="decision {{$diagnosis.CodeLevel}}">
<h3>{{$diagnosis.CodeTitle}}</h3>
<p>{{$diagnosis.CodeEvidence}}</p>
<p><strong>次:</strong> {{$diagnosis.CodeAction}}</p>
<p><a class="jump-link" href="#profiles" data-target="profiles">Profiles</a> &middot; <a class="jump-link" href="#profiles" data-target="isutools-profile-lines" title="解析が未公開の場合はProfilesへ移動します">行解析結果</a> &middot; <a class="jump-link" href="#profiles" data-target="isutools-profile-analysis" data-expand=".isutools-flame" data-expand-ready="true" title="解析が未公開の場合はProfilesへ移動します">CPU pprofフレームグラフ</a></p>
<p class="meta">行解析またはフレームグラフがまだpublishされていないレポートでは、採取状態を確認できるProfilesへ移動します。</p>
</article>
</div>
<p class="jump">根拠へ移動: <a class="jump-link" href="#bottleneck-overview" data-target="bottleneck-overview">全signal</a> &middot; <a class="jump-link" href="#collector-health" data-target="collector-health">計測の欠損</a> &middot; <a class="jump-link" href="#run-timeline" data-target="run-timeline">時系列</a> &middot; <a class="jump-link" href="#db-pool" data-target="db-pool">DB Pool</a> &middot; <a class="jump-link" href="#sql" data-target="sql">SQL</a> &middot; <a class="jump-link" href="#http" data-target="http">HTTP</a> &middot; <a class="jump-link" href="#profiles" data-target="profiles">Profiles</a></p>

<span id="bottleneck-overview"></span><h2>Bottleneck Overview <span class="meta">(原因の断定ではなく、根拠一覧)</span></h2>
<p class="meta">累計 demand と capacity / failure signal を同じ区間で比較します。各行の根拠は下の詳細セクションに残っています。</p>
<table>
<thead><tr><th>見る順</th><th>signal</th><th>evidence</th><th>次に確認すること</th></tr></thead>
<tbody>{{range bottlenecks .Snapshot}}<tr>
<td>{{.Order}}</td><td class="l"><span class="signal {{.Level}}">{{.Signal}}</span></td><td class="l">{{.Evidence}}</td><td class="l">{{.NextAction}}</td>
</tr>{{end}}</tbody>
</table>

<span id="collector-health"></span><h2>Collector Health</h2>
{{if .Snapshot.Meta.Partial}}<p class="warn">partial snapshot: one or more collectors reported incomplete data</p>{{end}}
{{if .Snapshot.Meta.Health}}
<details><summary>collectorの状態と欠損を表示 ({{len .Snapshot.Meta.Health}}件)</summary>
<table>
<thead><tr><th>collector</th><th>status</th><th>dropped</th><th>message</th></tr></thead>
<tbody>{{range .Snapshot.Meta.Health}}<tr>
<td class="l">{{.Collector}}</td><td>{{.Status}}</td><td data-v="{{.Dropped}}">{{.Dropped}}</td><td class="l">{{.Message}}</td>
</tr>{{end}}</tbody>
</table>
</details>
{{else}}<p class="empty">no core collector warnings</p>{{end}}

{{if .Snapshot.Peers}}
<span id="multi-host"></span><h2>Multi-host Participants <span class="meta">(hostごとの値。合算しません)</span></h2>
<table>
<thead><tr><th>peer / role</th><th>agent</th><th>form</th><th>required</th><th>state</th><th>validity</th><th>start send→ack</th><th>start local spread</th><th>finish send→ack</th><th>finish local spread</th><th>sealed</th><th>budget</th><th>failure</th></tr></thead>
<tbody>{{range .Snapshot.Peers}}<tr>
<td class="l">{{.Name}} / {{.Info.Role}}</td><td class="l">{{.Info.AgentID}}</td><td>{{.Form}}</td><td>{{.Required}}</td>
<td>{{if .Status}}{{.Status.State}}{{else}}-{{end}}</td><td>{{if .Status}}{{.Status.Validity}}{{else}}-{{end}}</td>
<td class="l">{{barrier .StartSendAck}}</td><td>{{if .Start}}{{dur .Start.BoundaryWindow.Spread}}{{else}}-{{end}}</td>
<td class="l">{{barrier .FinishSendAck}}</td><td>{{if .Finish}}{{dur .Finish.BoundaryWindow.Spread}}{{else}}-{{end}}</td><td>{{.Sealed}}</td>
<td>{{if .Local}}{{.Local.Budget.EncodedBytes}} / {{.Local.Budget.MaxBytes}}{{else}}-{{end}}</td>
<td class="l">{{if .Failure}}{{.Failure.Phase}} / {{.Failure.Code}}{{else}}-{{end}}</td>
</tr>{{end}}</tbody>
</table>
<p class="meta">send→ackはhub観測の不確実性区間、local spreadは各peer内のcollector境界幅です。peer間の時計は比較・補正しません。host identity、section issues、local snapshotはJSONにも保持されます。required participantの欠落はinvalid、optionalの欠落はpartialです。</p>
{{end}}

{{with .Snapshot.Timeline}}
<span id="run-timeline"></span><h2>Run Timeline <span class="meta">(時系列相関。原因の断定ではありません)</span></h2>
<p class="meta">{{ns .IntervalNs}} buckets &middot; {{len .Buckets}} / {{.MaxBuckets}} retained{{if .Truncated}} &middot; <span class="warn">truncated</span>{{end}}{{if .OverflowedEvents}} &middot; overflowed events {{.OverflowedEvents}}{{end}}</p>
{{if .Analysis.Available}}
<details><summary>時系列の詳細・phase・相関候補を表示</summary>
{{if .Analysis.Phases}}
<table>
<thead><tr><th>bucket</th><th>phase</th><th>window</th><th>signal</th><th>metric</th><th>value</th><th>formula / limitation</th></tr></thead>
<tbody>{{range .Analysis.Phases}}{{$phase := .}}{{range .Evidence}}<tr>
<td>{{.BucketIndex}}</td><td class="l">{{$phase.Kind}}</td><td>{{.WindowStart}} – {{.WindowEnd}}</td><td class="l">{{.Signal}}</td><td class="l">{{.Metric}}</td><td>{{f1 .Value}}</td><td class="l">{{.Formula}}; limitation: {{.Limitation}}</td>
</tr>{{end}}{{end}}</tbody>
</table>
{{end}}
{{if .Analysis.Suspects}}
<table>
<thead><tr><th>score</th><th>label</th><th>kind</th><th>candidate</th><th>signal</th><th>evidence</th></tr></thead>
<tbody>{{range .Analysis.Suspects}}<tr>
<td data-v="{{.Score}}">{{.Score}}</td><td>{{.Label}}</td><td>{{.Kind}}</td><td class="l">{{.Key}}</td><td class="l">{{.Signal}}</td>
<td class="l">{{range .Evidence}}bucket {{.BucketIndex}} {{.Metric}}={{f1 .Value}}; {{.Formula}}; limitation: {{.Limitation}}{{end}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no correlation suspects met the published rules</p>{{end}}
<details><summary>phase rules</summary>{{range .Analysis.Rules}}<p class="meta"><strong>{{.ID}}</strong>: {{.Formula}}; limitation: {{.Limitation}}</p>{{end}}</details>
</details>
{{else}}<p class="empty">time-aware analysis unavailable: {{.Analysis.Reason}}. Aggregate SQL/HTTP/resource tables remain authoritative.</p>{{end}}
{{else}}
<span id="run-timeline"></span><h2>Run Timeline <span class="meta">(時系列相関。原因の断定ではありません)</span></h2>
<p class="empty">time-aware analysis unavailable: timeline not captured. Aggregate SQL/HTTP/resource tables remain authoritative. Enable ISUTOOLS_TIMELINE=1 before the run to collect bounded buckets.</p>
{{end}}

<h2>Advisor <span class="meta">(ISUCON 定石で未設定のもの)</span></h2>
{{if .Snapshot.Advisor}}
<table>
<thead><tr><th>status</th><th>check</th><th>current</th><th>recommendation</th><th>why</th></tr></thead>
<tbody>
{{range .Snapshot.Advisor}}<tr>
<td class="l">{{if eq (printf "%s" .Status) "missing"}}<strong class="warn">missing</strong>{{else if eq (printf "%s" .Status) "warn"}}<span class="warn">warn</span>{{else}}{{.Status}}{{end}}</td>
<td class="l">{{.Title}}</td>
<td class="l">{{.Detail}}</td>
<td class="l">{{.Recommendation}}</td>
<td class="l"><details><summary>{{.Provenance.RuleVersion}} / {{.Provenance.Category}}</summary>
source: {{.Provenance.Source}} ({{.Provenance.Freshness}}, {{.Provenance.Scope}})<br>
formula: {{.Provenance.Formula}}<br>actual: {{.Provenance.Actual}} {{.Provenance.Unit}}<br>
limitation: {{.Provenance.Limitation}}<br>docs: {{.Provenance.Docs}}</details></td>
</tr>{{end}}
</tbody>
</table>
{{else}}<p class="empty">not captured</p>{{end}}

<h2>DB Schema <span class="meta">(captured at generation start)</span></h2>
<details><summary>Database capability matrix</summary>
<table><thead><tr><th>feature</th><th>MySQL</th><th>MariaDB</th><th>PostgreSQL</th><th>SQLite</th><th>requirement</th></tr></thead><tbody>
{{range .Snapshot.DBCapabilityMatrix}}<tr><td class="l">{{.Feature}}</td><td>{{.MySQL}}</td><td>{{.MariaDB}}</td><td>{{.PostgreSQL}}</td><td>{{.SQLite}}</td><td class="l">{{.Requirement}}</td></tr>{{end}}
</tbody></table>
{{if .Snapshot.DBCapabilities}}<h3>Registered targets</h3>
{{range .Snapshot.DBCapabilities}}<p class="meta"><strong>{{.TargetID}}</strong> dialect={{.Dialect}} flavor={{.Flavor}} version={{.Version}}</p>
<table><thead><tr><th>feature</th><th>state</th><th>reason</th></tr></thead><tbody>{{range .Capabilities}}<tr><td class="l">{{.Feature}}</td><td>{{.State}}</td><td class="l">{{.Reason}}</td></tr>{{end}}</tbody></table>{{end}}
{{else}}<p class="empty">no registered database targets</p>{{end}}
</details>
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

<span id="sql"></span><h2>SQL</h2>
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

{{with .Snapshot.QueryPlan}}{{with planTables .}}
<h2>Query Plans <span class="meta">(ベンチ終了後に上位 digest へ EXPLAIN を 1 回だけ実行した結果。画面を開いても再実行はしません)</span></h2>
{{range .}}
<p class="meta">target {{.TargetID}}{{if .Schema}} &middot; schema {{.Schema}}{{end}}</p>
<table>
<thead><tr>
<th>query</th><th>鮮度</th><th>select_type</th><th>table</th><th>type</th><th>key</th><th>possible_keys</th><th>rows</th><th>Extra</th>
</tr></thead>
<tbody>
{{range .Lines}}<tr{{if .Hot}} class="hot"{{else if not .Fresh}} class="stale"{{end}}>
<td class="l" title="{{.Query}}">{{cut .Query 90}}</td>
<td class="l">{{.Freshness}}</td>
<td class="l">{{.SelectType}}</td>
<td class="l">{{.Table}}</td>
<td class="l{{if .FullScan}} flag{{end}}">{{.Type}}</td>
<td class="l">{{.Key}}</td>
<td class="l">{{.PossibleKeys}}</td>
<td data-v="{{.RowsSort}}">{{.Rows}}</td>
<td class="l{{if or .Filesort .Temporary}} flag{{end}}">{{if .Note}}{{.Note}}{{else}}{{.Extra}}{{end}}</td>
</tr>{{end}}
</tbody>
</table>
{{end}}
<p class="meta">type=ALL(全表走査)・Using filesort(索引で解けないソート)・Using temporary(一時表)の行を網掛けにしています。— は当該列が NULL、つまりサーバが値を返さなかったことを表します。</p>
<p class="meta">灰色の行は計測区間内に実行されたサンプルではありません(区間外・DB 時計異常・partial な区間)。リテラルが違えば実行計画も変わるため、advisor の判定対象からは外しています。鮮度の列にその理由が入ります。</p>
{{end}}{{end}}

<span id="http"></span><h2>HTTP</h2>
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
<span id="db-pool"></span><h2>DB Pool <span class="meta">(database/sql のコネクションプール。点の値は終端境界、カウンタは区間デルタ)</span></h2>
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

<h2>Redis / Key-Value Commands <span class="meta">(command名のみ。key/value/argumentは保存しません)</span></h2>
{{if .Snapshot.Redis}}
<table>
<thead><tr><th>total(ms)</th><th>count</th><th>errors</th><th>avg(ms)</th><th>p95*(ms)</th><th>command</th></tr></thead>
<tbody>{{range .Snapshot.Redis}}<tr>
<td data-v="{{.Total.Nanoseconds}}">{{ms .Total}}</td><td data-v="{{.Count}}">{{.Count}}</td>
<td data-v="{{.ErrorCount}}"{{if .ErrorCount}} class="flag"{{end}}>{{.ErrorCount}}</td>
<td data-v="{{.Avg.Nanoseconds}}">{{ms .Avg}}</td><td data-v="{{.P95.Nanoseconds}}">{{ms .P95}}</td><td class="l">{{.Command}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no Redis observations (isutools.ObserveRedis / MeasureRedis)</p>{{end}}

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

{{$flowViz := safeFlowViz .Snapshot.FlowVisualization}}
{{if flowVizReady $flowViz}}
<span id="journey-visualization"></span><h2>Journey Visualization <span class="meta">(bounded pseudonymous sessions / registered route templates)</span></h2>
<p class="meta">status {{$flowViz.Status}}{{if $flowViz.Partial}} &middot; <span class="warn">partial</span>{{end}} &middot; session dropped {{$flowViz.SessionDropped}} &middot; timing missing {{$flowViz.TimingMissing}} &middot; hidden transition count {{$flowViz.Graph.HiddenCount}}{{if $flowViz.Graph.Truncated}} &middot; graph truncated into (other){{end}}</p>

<h3>Journey Funnel</h3>
{{if $flowViz.Funnels}}
<div class="flow-grid">{{range $flowViz.Funnels}}<section class="flow-card">
<h3>{{$flowViz.Status}} &middot; {{.ID}} <span class="meta">scenario {{.Scenario}} &middot; {{.Mode}}{{if .Within}} within {{.Within}}{{end}}</span></h3>
<p class="meta">entered {{.Entered}} &middot; completed {{.Completed}} &middot; conversion {{pctBP .ConversionBP}}{{if .Expired}} &middot; expired {{.Expired}}{{end}}</p>
<div role="img" aria-label="funnel {{.ID}}">{{range .Steps}}<div class="funnel-step">
<span title="{{.Route}}"><strong>{{.ID}}</strong><br><span class="meta">{{.Route}}</span></span>
<progress max="10000" value="{{.FromStartBP}}">{{pctBP .FromStartBP}}</progress>
<span>{{.Sessions}} sess &middot; {{pctBP .FromStartBP}}<br><span class="meta">prev {{pctBP .FromPreviousBP}} &middot; drop-off {{.DropOff}} &middot; retry {{.Retries}} &middot; 4xx {{.Status4xx}} &middot; 5xx {{.Status5xx}} &middot; p95 {{dur .RequestP95}}</span></span>
</div>{{end}}</div>
</section>{{end}}</div>
{{else}}<p class="empty">no funnel definitions (set ISUTOOLS_FUNNEL_CONFIG; graph-only visualization remains available)</p>{{end}}

<div class="flow-grid">
<section class="flow-card"><h3>User Flow Graph</h3>
{{$graph := flowGraph $flowViz.Graph}}
{{if $graph.Edges}}<svg class="flow-svg" role="img" aria-label="user flow graph" viewBox="0 0 800 500">
<defs><marker id="flow-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="5" markerHeight="5" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="#64748b"/></marker></defs>
{{range $graph.Edges}}{{if .Self}}<path class="flow-edge self-loop" d="{{.Path}}" fill="none" stroke-width="{{.Width}}" marker-end="url(#flow-arrow)"><title>{{.From}} → {{.To}} · {{.Count}}</title></path>{{else}}<line x1="{{.X1}}" y1="{{.Y1}}" x2="{{.X2}}" y2="{{.Y2}}" stroke-width="{{.Width}}" marker-end="url(#flow-arrow)"><title>{{.From}} → {{.To}} · {{.Count}}</title></line>{{end}}{{end}}
{{range $graph.Nodes}}<g><circle cx="{{.X}}" cy="{{.Y}}" r="{{.Radius}}"><title>{{.ID}} · weighted transitions {{.Count}}</title></circle><text x="{{.X}}" y="{{.Y}}">{{.Label}}</text></g>{{end}}
</svg>{{else}}<p class="empty">no visible transitions</p>{{end}}
</section>

<section class="flow-card"><h3>Transition Heatmap</h3>
{{$heatmap := flowHeatmap $flowViz.Graph}}
{{if $heatmap.Nodes}}<table class="heatmap"><thead><tr><th>from \ to</th>{{range $heatmap.Nodes}}<th title="{{.}}">{{.}}</th>{{end}}</tr></thead>
<tbody>{{range $heatmap.Rows}}{{$row := .}}<tr><td class="l">{{.From}}</td>{{range .Cells}}<td class="heat{{.Level}}" title="{{$row.From}} → {{.To}}">{{if .Count}}{{.Count}}{{else}}·{{end}}</td>{{end}}</tr>{{end}}</tbody></table>
{{else}}<p class="empty">no visible transitions</p>{{end}}
</section>
</div>
{{end}}

<h2>Scenario Stories <span class="meta">(明示scenarioラベル別の実測request列。疑似sessが必要)</span></h2>
{{if .Snapshot.ScenarioStories}}
<table>
<thead><tr><th>sessions</th><th>requests</th><th>scenario</th><th>observed journey</th></tr></thead>
<tbody>{{range .Snapshot.ScenarioStories}}<tr>
<td data-v="{{.Sessions}}">{{.Sessions}}</td><td data-v="{{.Requests}}">{{.Requests}}</td><td class="l">{{.Scenario}}</td>
<td class="l">{{range $i, $step := .Journey}}{{if $i}} &rarr; {{end}}{{$step}}{{end}}</td>
</tr>{{end}}</tbody>
</table>
{{else}}<p class="empty">no scenario story data (middlewareに疑似sessionとscenarioを設定)</p>{{end}}

<h2>User Flow <span class="meta">(疑似session毎のroute template遷移 上位20)</span></h2>
{{if .Snapshot.UserFlows}}
<table>
<thead><tr><th>count</th><th>from</th><th></th><th>to</th></tr></thead>
<tbody>{{range .Snapshot.UserFlows}}<tr>
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

{{with .Snapshot.Meta.Profiles}}
<span id="profiles"></span><h2>Profiles <span class="meta">(累積 profile は open/close 差分、goroutine 系は close snapshot、trace は短時間の diagnostic interval)</span></h2>
{{with .Trace}}
<h3>Execution trace</h3>
<p class="meta">status {{.Status}}{{if .Code}} / {{.Code}}{{end}} · capture {{ns .CaptureSpanNs}} · requested {{ns .RequestedSpanNs}} · complete {{.Complete}}</p>
{{if .File}}<p><a href="/files/{{.File}}">restricted raw trace</a> · {{.Bytes}} bytes · <code>{{.SHA256}}</code></p><pre class="cmd">go tool trace {{.File}}</pre>{{else}}<p class="empty">ready な trace artifact はありません。</p>{{end}}
{{end}}
{{if .Pairs}}
{{range .Pairs}}
<p class="meta">{{if .Lagging}}<span class="warn">⚠ 採取遅延</span> &middot; {{end}}{{.Kind}} &middot; {{.ResidualText}}{{if .OpenGate}} &middot; open gate {{.OpenGate}}{{end}}</p>
{{range .Notes}}<p class="meta">{{.}}</p>{{end}}
<pre class="cmd">{{.DiffCommand}}</pre>
{{end}}
{{else}}<p class="empty">差分できる pair はありません(片端しか採れていない種別は pair を作りません)</p>{{end}}
{{if .Captures}}
<table>
<thead><tr><th>kind</th><th>point</th><th>status</th><th>code</th><th>lag</th><th>file</th></tr></thead>
<tbody>{{range .Captures}}<tr>
<td class="l">{{.Kind}}</td><td>{{.Point}}</td><td>{{.Status}}</td>
<td class="l">{{if .Code}}{{.Code}}{{else}}-{{end}}</td>
<td data-v="{{.LagFromRefNs}}">{{ns .LagFromRefNs}}</td>
<td class="l">{{if .File}}{{.File}}{{else}}-{{end}}</td>
</tr>{{end}}</tbody>
</table>
{{end}}
<p class="meta">artifact は ISUTOOLS_DATA_DIR に保存され、ダッシュボードの files/&lt;name&gt; から取得できます(sidecar の .meta.json も同じ場所です)。run 単位のプロファイルは存在しないので、必ず open と close の差分で読んでください。</p>
{{end}}

{{if .Sortable}}<script>
document.querySelectorAll(".jump-link").forEach(function (link) {
  link.addEventListener("click", function (event) {
    var target = document.getElementById(link.dataset.target);
    if (!target) return;
    event.preventDefault();
    var expand = link.dataset.expand;
    if (expand) {
      var details;
      if (link.dataset.expandReady === "true") {
        var graph = target.querySelector(expand + ' svg[aria-label="bounded flame graph"]');
        details = graph ? graph.closest("details") : null;
      } else {
        details = target.matches(expand) ? target : target.querySelector(expand);
      }
      if (details) {
        if (details.tagName === "DETAILS") details.open = true;
        target = details;
      }
    }
    target.scrollIntoView({ behavior: "smooth", block: "start" });
  });
});
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
