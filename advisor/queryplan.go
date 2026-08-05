package advisor

import (
	"fmt"
	"sort"
	"strings"
)

// Query-plan checks read a captured EXPLAIN (plan 09) and report the three
// access-path defects that decide most ISUCON rounds: a full table scan over a
// table large enough to matter, a sort MySQL could not serve from an index, and
// a materialized temporary table.
//
// Two rules keep the checks honest:
//
//   - Only a plan whose sample ran inside the measured interval
//     (Freshness == PlanFreshnessFresh) is judged. A stale or unjudgeable
//     sample may predate the run, and a different literal can produce a
//     different plan, so those are surfaced as information and never as a
//     warning. Anything not explicitly fresh is excluded, the zero value
//     included: a caller that forgets to set Freshness then under-reports
//     instead of warning about a plan nobody measured.
//   - Nothing derived from the sample SQL is rendered. Detail strings carry
//     schema identifiers (table, key, possible_keys), a digest prefix and fixed
//     labels only. An unrecognized freshness reason or error class maps to a
//     generic label instead of being echoed, so a literal cannot reach the
//     dashboard through this path (plan 09 §エラーの構造化).
//
// Thresholds are provisional, and every Recommendation says so: they are
// calibrated against private-isu measurements before they become defaults.

const (
	// planFullScanRows is the provisional row estimate at or above which a
	// type=ALL access path is reported. Below it, a full scan of a small table
	// is usually cheaper than an index lookup.
	planFullScanRows = 1000
	// planDigestRunes is how much of a digest a Detail string shows. The full
	// MySQL 8 digest is 64 hex characters and unreadable on a dashboard.
	planDigestRunes = 12
	// planColumnRunes bounds one rendered EXPLAIN column (possible_keys can
	// list every index on a table).
	planColumnRunes = 48
)

// PlanFreshness records whether a captured sample can be judged. It mirrors
// plan 09's FreshnessState; only "fresh" is judged, and everything else —
// including the zero value — is excluded from the warnings.
type PlanFreshness string

const (
	// PlanFreshnessFresh means the sample ran inside the measured interval.
	PlanFreshnessFresh PlanFreshness = "fresh"
	// PlanFreshnessStale means the sample ran outside the interval.
	PlanFreshnessStale PlanFreshness = "stale"
	// PlanFreshnessUnknown means freshness could not be decided (database
	// clock anomaly, missing clock, partial run, interval too short).
	PlanFreshnessUnknown PlanFreshness = "unknown"
)

// QueryPlanRow is one EXPLAIN output row. Every column is nullable in MySQL's
// classic EXPLAIN output, so each is a pointer and a nil is rendered as
// "なし" rather than treated as a parse failure.
type QueryPlanRow struct {
	SelectType   *string `json:"select_type,omitempty"`
	Table        *string `json:"table,omitempty"`
	Type         *string `json:"type,omitempty"`
	Key          *string `json:"key,omitempty"`
	PossibleKeys *string `json:"possible_keys,omitempty"`
	Rows         *int64  `json:"rows,omitempty"`
	Extra        *string `json:"extra,omitempty"`
}

// QueryPlan is one digest's captured plan. Query is the normalized DIGEST_TEXT
// supplied by plan 04, never the literal-bearing sample: plan 09 keeps the
// sample inside the capture callback and this type has no field able to hold
// it.
type QueryPlan struct {
	// TargetID names the database target, so a multi-host run can tell two
	// identical plans apart. Empty for a single-target run.
	TargetID string `json:"target_id,omitempty"`
	Digest   string `json:"digest"`
	// Query is the normalized statement text (plan 04's DIGEST_TEXT).
	Query string `json:"query,omitempty"`
	// Freshness decides whether this plan is judged at all.
	Freshness PlanFreshness `json:"freshness"`
	// FreshReason is plan 09's closed FreshReason enum ("in_interval",
	// "before_interval", "after_interval", "db_clock_anomaly",
	// "db_clock_missing", "run_partial", "interval_too_short"). Unrecognized
	// values are never echoed.
	FreshReason string `json:"fresh_reason,omitempty"`
	// Rows is the EXPLAIN output; empty when the capture failed.
	Rows []QueryPlanRow `json:"rows,omitempty"`
	// ErrClass is plan 09's closed PlanErrorClass ("timeout",
	// "budget_exhausted", "permission_denied", "syntax_or_truncated",
	// "object_missing", "sample_unavailable", "sample_possibly_truncated",
	// "connection_error", "other"). Unrecognized values are never echoed.
	ErrClass string `json:"err_class,omitempty"`
}

// isFullScan reports a full table scan (type=ALL). "index", a full index scan,
// is a different access path and is deliberately not reported here.
func (r QueryPlanRow) isFullScan() bool {
	return r.Type != nil && strings.EqualFold(strings.TrimSpace(*r.Type), "ALL")
}

// hasExtra reports whether the Extra column carries flag, which must be given
// lowercase. MySQL writes "Using filesort", but Extra concatenates several
// flags and case is not worth depending on.
func (r QueryPlanRow) hasExtra(flag string) bool {
	if r.Extra == nil {
		return false
	}
	return strings.Contains(strings.ToLower(*r.Extra), flag)
}

const (
	fullScanRecommendation = "WHERE の等値条件列に index を張り type=ALL を ref/range にする" +
		"(possible_keys=なし は候補 index が存在しない状態。possible_keys があるのに key=なし なら、" +
		"暗黙の型変換・列への関数適用・複合 index の先頭列不一致で使えていないことを疑う)。" +
		"閾値 rows>=1000 は provisional(private-isu 実測後に確定)"

	filesortRecommendation = "ORDER BY の列順と昇降順に一致する複合 index を張ってソート自体を消す" +
		"(例: WHERE user_id=? ORDER BY created_at DESC なら (user_id, created_at DESC)。" +
		"MySQL 8.0 の降順 index で ORDER BY ... DESC もそのまま解ける)。ISUCON10 予選の定番の詰まり。" +
		"閾値は provisional(private-isu 実測後に確定)"

	temporaryRecommendation = "GROUP BY / DISTINCT / UNION が一時表を作っている。GROUP BY の列順に一致する index を張るか、" +
		"集計を事前集計テーブル・アプリ側へ寄せる。ディスク一時表に落ちていないかは " +
		"sql-rows-filesort(created_tmp_disk_tables)の数値で確認する。" +
		"閾値は provisional(private-isu 実測後に確定)"

	explainEnableHint = "ISUTOOLS_EXPLAIN=1 と EXPLAIN 専用の最小権限 credential" +
		"(RegisterDBInspector(id, PurposeExplain, ...) もしくは ISUTOOLS_EXPLAIN_DSN)を設定すると、" +
		"ベンチ終了時に上位 digest の EXPLAIN を取得します(MySQL 8.0.17+ の QUERY_SAMPLE_TEXT が必要)"

	staleRecommendation = "計測区間内に実行されたサンプルだけを判定に使います" +
		"(区間外のサンプルはリテラルが違えば実行計画も変わるため)。ベンチを回し直して区間内のサンプルを取り直すか、" +
		"DB 側時計の異常を解消してください"

	failedRecommendation = "EXPLAIN を実行できなかった digest があります。最小権限 credential の GRANT、" +
		"performance_schema_max_sql_text_length によるサンプル切り詰め、時間予算切れを確認してください"
)

// planSplit separates the plans that may be judged from the ones that may not.
type planSplit struct {
	// judged holds fresh plans that produced at least one EXPLAIN row.
	judged []QueryPlan
	// stale holds plans excluded by freshness (stale, unknown, or unset).
	stale []QueryPlan
	// failed holds fresh plans with no EXPLAIN rows.
	failed []QueryPlan
}

func splitPlans(plans []QueryPlan) planSplit {
	split := planSplit{}
	for _, p := range plans {
		switch {
		case p.Freshness != PlanFreshnessFresh:
			split.stale = append(split.stale, p)
		case len(p.Rows) == 0:
			split.failed = append(split.failed, p)
		default:
			split.judged = append(split.judged, p)
		}
	}
	return split
}

// suffix names the plans kept out of the judgement, so "no warning" cannot be
// confused with "nothing was judged".
func (s planSplit) suffix() string {
	parts := make([]string, 0, 2)
	if len(s.stale) > 0 {
		labels := make([]string, 0, len(s.stale))
		for _, p := range s.stale {
			labels = append(labels, freshnessLabel(p))
		}
		parts = append(parts, fmt.Sprintf("鮮度により除外 %d 件(%s)", len(s.stale), countLabels(labels)))
	}
	if len(s.failed) > 0 {
		labels := make([]string, 0, len(s.failed))
		for _, p := range s.failed {
			labels = append(labels, planErrorLabel(p.ErrClass))
		}
		parts = append(parts, fmt.Sprintf("取得できず %d 件(%s)", len(s.failed), countLabels(labels)))
	}
	if len(parts) == 0 {
		return ""
	}
	return " / 判定対象外: " + strings.Join(parts, "、")
}

// recommendation explains the excluded plans when nothing could be judged.
func (s planSplit) recommendation() string {
	parts := make([]string, 0, 2)
	if len(s.stale) > 0 {
		parts = append(parts, staleRecommendation)
	}
	if len(s.failed) > 0 {
		parts = append(parts, failedRecommendation)
	}
	return strings.Join(parts, " / ")
}

// freshnessLabel renders why a plan was excluded. The reason is a closed enum
// on the producing side; an unrecognized value is reported generically so no
// unvetted string reaches the dashboard.
func freshnessLabel(p QueryPlan) string {
	switch p.FreshReason {
	case "before_interval":
		return "計測区間より前のサンプル"
	case "after_interval":
		return "計測区間より後のサンプル"
	case "db_clock_anomaly":
		return "DB 時計異常のため判定不能"
	case "db_clock_missing":
		return "DB 側時計情報なしのため判定不能"
	case "run_partial":
		return "計測区間が partial のため判定不能"
	case "interval_too_short":
		return "計測区間が短すぎて判定不能"
	}
	if p.Freshness == "" {
		return "鮮度未設定"
	}
	if p.Freshness == PlanFreshnessStale {
		return "計測区間外のサンプル"
	}
	return "判定不能"
}

// planErrorLabel renders plan 09's PlanErrorClass. Unrecognized classes are
// reported generically for the same reason as freshnessLabel.
func planErrorLabel(class string) string {
	switch class {
	case "":
		return "実行計画の行なし"
	case "timeout":
		return "タイムアウト"
	case "budget_exhausted":
		return "時間予算切れ"
	case "permission_denied":
		return "権限不足"
	case "syntax_or_truncated":
		return "構文エラーまたはサンプル切り詰め"
	case "object_missing":
		return "対象オブジェクトなし"
	case "sample_unavailable":
		return "サンプルなし"
	case "sample_possibly_truncated":
		return "サンプル切り詰めの疑い"
	case "connection_error":
		return "接続エラー"
	default:
		return "その他"
	}
}

// countLabels aggregates labels into a deterministic "ラベル N 件" list.
func countLabels(labels []string) string {
	counts := map[string]int{}
	for _, label := range labels {
		counts[label]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d 件", key, counts[key]))
	}
	return joinLimited(parts, maxDetailItems)
}

// planRowLabel identifies one offending EXPLAIN row for a Detail string.
func planRowLabel(p QueryPlan, r QueryPlanRow, extra ...string) string {
	fields := append([]string{"digest=" + shortDigest(p.Digest)}, extra...)
	prefix := ""
	if p.TargetID != "" {
		prefix = truncateRunes(p.TargetID, planColumnRunes) + ": "
	}
	return prefix + tableLabel(r) + "(" + strings.Join(fields, ", ") + ")"
}

func tableLabel(r QueryPlanRow) string {
	if r.Table == nil || strings.TrimSpace(*r.Table) == "" {
		return "テーブル不明"
	}
	return truncateRunes(strings.TrimSpace(*r.Table), planColumnRunes)
}

// optionalColumn renders a nullable EXPLAIN column. These carry schema
// identifiers (index names, table names), never sample literals.
func optionalColumn(v *string) string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return "なし"
	}
	return truncateRunes(strings.TrimSpace(*v), planColumnRunes)
}

func shortDigest(digest string) string {
	if strings.TrimSpace(digest) == "" {
		return "不明"
	}
	return truncateRunes(digest, planDigestRunes)
}

// truncateRunes cuts on a rune boundary so a multi-byte identifier cannot be
// split into invalid UTF-8.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// queryPlanChecks builds the three plan findings. captureError reports why
// capture could not run at all; it must not carry driver text (plan 09
// §エラーの構造化).
func queryPlanChecks(plans []QueryPlan, captureError string) []Check {
	checks := []Check{
		{ID: "plan-full-scan", Title: "実行計画: type=ALL の全表走査"},
		{ID: "plan-filesort", Title: "実行計画: Using filesort(索引で解けないソート)"},
		{ID: "plan-temporary", Title: "実行計画: Using temporary(一時表の作成)"},
	}
	if captureError != "" {
		return skipQueryPlanChecks(checks, "EXPLAIN を取得できませんでした: "+captureError)
	}
	if len(plans) == 0 {
		return skipQueryPlanChecks(checks, "EXPLAIN の取得結果がありません")
	}

	split := splitPlans(plans)
	if len(split.judged) == 0 {
		for i := range checks {
			checks[i].Status = StatusInfo
			checks[i].Detail = "判定できる実行計画がありません" + split.suffix()
			checks[i].Recommendation = split.recommendation()
		}
		return checks
	}
	checks[0] = fullScanCheck(checks[0], split)
	checks[1] = extraCheck(checks[1], split, "using filesort", "Using filesort", filesortRecommendation)
	checks[2] = extraCheck(checks[2], split, "using temporary", "Using temporary", temporaryRecommendation)
	return checks
}

func skipQueryPlanChecks(checks []Check, detail string) []Check {
	for i := range checks {
		checks[i].Status = StatusSkip
		checks[i].Detail = detail
		checks[i].Recommendation = explainEnableHint
	}
	return checks
}

// fullScanCheck reports type=ALL rows whose row estimate reaches the
// provisional threshold. A NULL row estimate is not judged: a missing number
// is not evidence of a large table.
func fullScanCheck(c Check, split planSplit) Check {
	hits := make([]string, 0, len(split.judged))
	tolerated := 0
	for _, p := range split.judged {
		for _, r := range p.Rows {
			if !r.isFullScan() {
				continue
			}
			if r.Rows == nil || *r.Rows < planFullScanRows {
				tolerated++
				continue
			}
			hits = append(hits, planRowLabel(p, r,
				fmt.Sprintf("rows=%d", *r.Rows),
				"possible_keys="+optionalColumn(r.PossibleKeys)))
		}
	}
	switch {
	case len(hits) > 0:
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("rows>=%d の type=ALL が %d 件: %s",
			planFullScanRows, len(hits), joinLimited(hits, maxDetailItems))
		c.Recommendation = fullScanRecommendation
	case tolerated > 0:
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("type=ALL は %d 件だが rows は %d 未満または不明(小さな表の全走査は許容)",
			tolerated, planFullScanRows)
	default:
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("判定した %d プランに type=ALL なし", len(split.judged))
	}
	c.Detail += split.suffix()
	return c
}

// extraCheck reports rows whose Extra column carries flag.
func extraCheck(c Check, split planSplit, flag, label, recommendation string) Check {
	hits := make([]string, 0, len(split.judged))
	for _, p := range split.judged {
		for _, r := range p.Rows {
			if !r.hasExtra(flag) {
				continue
			}
			hits = append(hits, planRowLabel(p, r, "key="+optionalColumn(r.Key)))
		}
	}
	if len(hits) > 0 {
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("%s が %d 件: %s", label, len(hits), joinLimited(hits, maxDetailItems))
		c.Recommendation = recommendation
	} else {
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("判定した %d プランに %s なし", len(split.judged), label)
	}
	c.Detail += split.suffix()
	return c
}

func isQueryPlanCheckID(id string) bool {
	switch id {
	case "plan-full-scan", "plan-filesort", "plan-temporary":
		return true
	default:
		return false
	}
}

// WithQueryPlans replaces the query-plan checks at snapshot time, once plan
// 09's capture has run in the post-FinishRun enrich phase. err reports why
// capture could not run; like the other hooks it must carry a summary, never a
// driver error, because a driver message can embed a fragment of the sample
// SQL.
func WithQueryPlans(checks []Check, plans []QueryPlan, err error) []Check {
	result := make([]Check, 0, len(checks)+3)
	for _, check := range checks {
		if !isQueryPlanCheckID(check.ID) {
			result = append(result, check)
		}
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	result = append(result, queryPlanChecks(plans, errText)...)
	return sortChecks(result)
}
