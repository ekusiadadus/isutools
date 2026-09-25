package web

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/internal/agg"
)

// diagnosticCandidate is an evidence-bound experiment suggestion. Its text
// must not turn an aggregate correlation into a causal diagnosis.
type diagnosticCandidate struct {
	Title    string
	Evidence string
	NextStep string
}

// diagnosticCandidates uses only measurements present in this snapshot. The
// ordering puts instrumentation checks before performance experiments.
func diagnosticCandidates(snapshot Snapshot) []diagnosticCandidate {
	var candidates []diagnosticCandidate
	var globalSQL, attributedSQL, trackedHTTP int64
	for _, entry := range snapshot.SQL {
		globalSQL += entry.Count
	}
	for _, entry := range snapshot.HTTP {
		attributedSQL += entry.SQLCount
		trackedHTTP += entry.SQLTrackedRequests
	}

	if globalSQL > 0 && globalSQL > attributedSQL {
		evidence := fmt.Sprintf("SQL集計 %d回、HTTP request contextに紐付いたSQL %d回、tracked requests %d件。集計区間や実行中requestが異なる場合があり、差分を未追跡SQLの正確な件数とは扱えません。", globalSQL, attributedSQL, trackedHTTP)
		next := "同じ計測区間のHTTPとSQLを確認し、代表的なhandlerでQueryContext / ExecContext等へrequest.Context()を渡して再計測します。"
		if trackedHTTP == 0 {
			evidence += " HTTP側のSQL計測情報がない旧snapshot等ではcoverageを判定できません。"
			next = "request context付きの計測を有効にした新しいrunで再確認します。"
		} else if attributedSQL == 0 {
			next = "まず代表的なhandlerのdb.Query / db.Exec、context.Background()への置換を確認し、QueryContext / ExecContext等へrequest.Context()を渡す実験をします。"
		}
		candidates = append(candidates, diagnosticCandidate{
			Title:    "SQLのリクエスト紐付けを確認",
			Evidence: evidence,
			NextStep: next,
		})
	}

	// Heuristic for prioritizing a round-trip experiment, not an N+1 finding.
	// Both cutoffs are intentionally visible in the evidence and can be tuned
	// after workload-specific validation.
	const frequentCalls = 100
	const shortAverage = 5 * time.Millisecond
	var frequent *agg.Entry
	for i := range snapshot.SQL {
		entry := &snapshot.SQL[i]
		if entry.Count < frequentCalls || entry.Avg <= 0 || entry.Avg > shortAverage {
			continue
		}
		if frequent == nil || entry.Count > frequent.Count {
			frequent = entry
		}
	}
	if frequent != nil {
		evidence := fmt.Sprintf("heuristic: 同一SQL形の実行 %d回（閾値 %d回以上）、平均 %s（閾値 %s以下）。短いSQLの反復はN+1や性能律速の証明ではありません。", frequent.Count, frequentCalls, frequent.Avg, shortAverage)
		if frequent.Key != "" {
			evidence += fmt.Sprintf(" SQL検索キー: %q。", boundedDiagnosticSQLKey(frequent.Key))
		}
		next := "SQL per endpointの回数、代表的なhandler、DBの実行計画を照合し、呼び出し回数を減らす変更を1つずつ比較します。score・pass・penalty・params・p95・error rateを同一条件で記録します。"
		for _, check := range snapshot.Advisor {
			if check.ID == "dsn-interpolate-params" && check.Status == advisor.StatusMissing {
				evidence += " MySQL DSN advisorはinterpolateParams未設定と報告しています。"
				next += " 別案としてinterpolateParams設定を単独で試し、効果と副作用を同じ指標で確認します。改善は未測定です。"
				break
			}
		}
		candidates = append(candidates, diagnosticCandidate{
			Title:    "短いSQLの反復を調査",
			Evidence: evidence,
			NextStep: next,
		})
	}

	// A process percent follows top's one-core=100% convention, while host
	// idle is a whole-host percentage. Their scales must stay distinct.
	const processNearOneCore = 90.0
	const processAtMostOneCore = 110.0
	const hostIdle = 50.0
	if proc := snapshot.Proc; proc != nil && proc.CPUTotal != nil && procIntervalMatchesRun(snapshot) && proc.CPUTotal.IdlePercent >= hostIdle {
		for _, process := range proc.TopCPU {
			if process.CPUPercent < processNearOneCore || process.CPUPercent > processAtMostOneCore {
				continue
			}
			candidates = append(candidates, diagnosticCandidate{
				Title:    "プロセス負荷とホスト余力を分けて調査",
				Evidence: fmt.Sprintf("heuristic: PID %d は %.1f%%（1コア=100%%、目安 %.0f–%.0f%%）、ホスト全体は idle %.1f%%（目安 %.0f%%以上）。この値だけではシステム全体の上限や分散先の余力は断定できません。", process.PID, process.CPUPercent, processNearOneCore, processAtMostOneCore, proc.CPUTotal.IdlePercent, hostIdle),
				NextStep: "PIDの役割とCPU profileを確認し、他のapp hostのCPU・traffic・制限値を別途確認します。余力がある場合はtraffic配分を1条件だけ変え、score・pass・penalty・params・p95・error rateを比較します。",
			})
			break
		}
	}
	return candidates
}

// SQL keys are already normalized by sqlstats. Bound the display copy by
// bytes so an unusually long key cannot dominate the candidate table.
func boundedDiagnosticSQLKey(key string) string {
	const maxBytes = 200
	key = strings.ToValidUTF8(key, "�")
	if len(key) <= maxBytes {
		return key
	}
	end := 0
	for index, r := range key {
		size := utf8.RuneLen(r)
		if index+size > maxBytes-len("…") {
			break
		}
		end = index + size
	}
	return key[:end] + "…"
}
