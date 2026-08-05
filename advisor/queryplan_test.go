package advisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"
)

// nullRows marks a NULL row estimate in the row builder below.
const nullRows = int64(-1)

// row builds a QueryPlanRow where "" means a NULL column and nullRows means a
// NULL row estimate, so a table entry can state exactly which columns MySQL
// left empty.
func row(table, accessType, key, possibleKeys, extra string, rows int64) QueryPlanRow {
	optional := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	r := QueryPlanRow{
		Table:        optional(table),
		Type:         optional(accessType),
		Key:          optional(key),
		PossibleKeys: optional(possibleKeys),
		Extra:        optional(extra),
	}
	if rows != nullRows {
		r.Rows = &rows
	}
	return r
}

func freshPlan(digest string, rows ...QueryPlanRow) QueryPlan {
	return QueryPlan{
		TargetID:    "db1",
		Digest:      digest,
		Query:       "SELECT * FROM `posts` WHERE `user_id` = ?",
		Freshness:   PlanFreshnessFresh,
		FreshReason: "in_interval",
		Rows:        rows,
	}
}

func withFreshness(p QueryPlan, freshness PlanFreshness, reason string) QueryPlan {
	p.Freshness = freshness
	p.FreshReason = reason
	return p
}

func TestQueryPlanChecks(t *testing.T) {
	fullScan := row("posts", "ALL", "", "", "Using where", 20000)
	smallScan := row("comments", "ALL", "", "idx_post_id", "Using where", 999)
	filesort := row("posts", "ref", "idx_user_id", "idx_user_id", "Using where; Using filesort", 120)
	temporary := row("posts", "ref", "idx_user_id", "idx_user_id", "Using temporary", 120)

	cases := []struct {
		name string
		// plans is the capture result handed to the checks.
		plans []QueryPlan
		// want is the expected status per check ID.
		want map[string]Status
		// detail lists substrings every non-empty entry's Detail must carry.
		detail map[string]string
		// recommendation lists substrings the Recommendation must carry.
		recommendation map[string]string
	}{
		{
			name:  "no plans skips with an enabling hint",
			plans: nil,
			want: map[string]Status{
				"plan-full-scan": StatusSkip, "plan-filesort": StatusSkip, "plan-temporary": StatusSkip,
			},
			detail: map[string]string{"plan-full-scan": "EXPLAIN の取得結果がありません"},
			recommendation: map[string]string{
				"plan-full-scan": "ISUTOOLS_EXPLAIN=1",
				"plan-filesort":  "PurposeExplain",
				"plan-temporary": "QUERY_SAMPLE_TEXT",
			},
		},
		{
			name:  "empty plan slice skips as well",
			plans: []QueryPlan{},
			want: map[string]Status{
				"plan-full-scan": StatusSkip, "plan-filesort": StatusSkip, "plan-temporary": StatusSkip,
			},
			detail: map[string]string{"plan-temporary": "EXPLAIN の取得結果がありません"},
		},
		{
			name:  "full scan above the row threshold warns",
			plans: []QueryPlan{freshPlan("aaaaaaaaaaaabbbbbbbbbbbb", fullScan)},
			want: map[string]Status{
				"plan-full-scan": StatusWarn, "plan-filesort": StatusOK, "plan-temporary": StatusOK,
			},
			detail: map[string]string{
				"plan-full-scan": "db1: posts(digest=aaaaaaaaaaaa…, rows=20000, possible_keys=なし)",
			},
			recommendation: map[string]string{"plan-full-scan": "index"},
		},
		{
			name:  "full scan exactly at the threshold warns",
			plans: []QueryPlan{freshPlan("d1", row("posts", "ALL", "", "idx_user_id", "", planFullScanRows))},
			want:  map[string]Status{"plan-full-scan": StatusWarn},
			detail: map[string]string{
				"plan-full-scan": "rows=1000, possible_keys=idx_user_id",
			},
		},
		{
			name:  "full scan below the threshold is tolerated",
			plans: []QueryPlan{freshPlan("d1", smallScan)},
			want: map[string]Status{
				"plan-full-scan": StatusOK, "plan-filesort": StatusOK, "plan-temporary": StatusOK,
			},
			detail: map[string]string{"plan-full-scan": "小さな表の全走査は許容"},
		},
		{
			name:  "full scan with a NULL row estimate is not judged",
			plans: []QueryPlan{freshPlan("d1", row("posts", "ALL", "", "", "", nullRows))},
			want:  map[string]Status{"plan-full-scan": StatusOK},
			detail: map[string]string{
				"plan-full-scan": "type=ALL は 1 件だが rows は 1000 未満または不明",
			},
		},
		{
			name:  "lowercase access type is still a full scan",
			plans: []QueryPlan{freshPlan("d1", row("posts", "all", "", "", "", 5000))},
			want:  map[string]Status{"plan-full-scan": StatusWarn},
		},
		{
			name:  "index scan is not reported as a full scan",
			plans: []QueryPlan{freshPlan("d1", row("posts", "index", "idx_created_at", "", "", 90000))},
			want:  map[string]Status{"plan-full-scan": StatusOK},
		},
		{
			name:  "filesort warns and names the index lever",
			plans: []QueryPlan{freshPlan("cafecafecafe0001", filesort)},
			want: map[string]Status{
				"plan-full-scan": StatusOK, "plan-filesort": StatusWarn, "plan-temporary": StatusOK,
			},
			detail: map[string]string{
				"plan-filesort": "db1: posts(digest=cafecafecafe…, key=idx_user_id)",
			},
			recommendation: map[string]string{"plan-filesort": "ORDER BY"},
		},
		{
			name:  "temporary table warns",
			plans: []QueryPlan{freshPlan("d1", temporary)},
			want: map[string]Status{
				"plan-full-scan": StatusOK, "plan-filesort": StatusOK, "plan-temporary": StatusWarn,
			},
			detail:         map[string]string{"plan-temporary": "Using temporary が 1 件"},
			recommendation: map[string]string{"plan-temporary": "GROUP BY"},
		},
		{
			name: "one row carrying both flags warns twice",
			plans: []QueryPlan{freshPlan("d1",
				row("posts", "ref", "", "idx_user_id", "Using where; Using temporary; Using filesort", 300))},
			want: map[string]Status{
				"plan-full-scan": StatusOK, "plan-filesort": StatusWarn, "plan-temporary": StatusWarn,
			},
			detail: map[string]string{
				"plan-filesort":  "key=なし",
				"plan-temporary": "key=なし",
			},
		},
		{
			name: "all three findings across several plans",
			plans: []QueryPlan{
				freshPlan("d1", fullScan),
				freshPlan("d2", filesort),
				freshPlan("d3", temporary),
			},
			want: map[string]Status{
				"plan-full-scan": StatusWarn, "plan-filesort": StatusWarn, "plan-temporary": StatusWarn,
			},
		},
		{
			name: "unrelated Extra flags stay ok",
			plans: []QueryPlan{freshPlan("d1",
				row("posts", "ref", "idx_user_id", "idx_user_id", "Using where; Using index condition", 10))},
			want: map[string]Status{
				"plan-full-scan": StatusOK, "plan-filesort": StatusOK, "plan-temporary": StatusOK,
			},
			detail: map[string]string{"plan-filesort": "判定した 1 プランに Using filesort なし"},
		},
		{
			name:  "every column NULL is judged, not crashed",
			plans: []QueryPlan{freshPlan("", QueryPlanRow{})},
			want: map[string]Status{
				"plan-full-scan": StatusOK, "plan-filesort": StatusOK, "plan-temporary": StatusOK,
			},
		},
		{
			name:  "NULL table on an offending row still identifies the digest",
			plans: []QueryPlan{freshPlan("beefbeefbeef0001", row("", "ALL", "", "", "Using filesort", 4000))},
			want: map[string]Status{
				"plan-full-scan": StatusWarn, "plan-filesort": StatusWarn,
			},
			detail: map[string]string{
				"plan-full-scan": "テーブル不明(digest=beefbeefbeef…",
				"plan-filesort":  "テーブル不明(digest=beefbeefbeef…",
			},
		},
		{
			name: "stale plans are excluded from judgement",
			plans: []QueryPlan{
				withFreshness(freshPlan("d1", fullScan, filesort, temporary),
					PlanFreshnessStale, "before_interval"),
			},
			want: map[string]Status{
				"plan-full-scan": StatusInfo, "plan-filesort": StatusInfo, "plan-temporary": StatusInfo,
			},
			detail: map[string]string{
				"plan-full-scan": "鮮度により除外 1 件(計測区間より前のサンプル 1 件)",
			},
			recommendation: map[string]string{"plan-full-scan": "計測区間内に実行されたサンプルだけ"},
		},
		{
			name: "unknown freshness from a clock anomaly is excluded",
			plans: []QueryPlan{
				withFreshness(freshPlan("d1", fullScan), PlanFreshnessUnknown, "db_clock_anomaly"),
			},
			want: map[string]Status{
				"plan-full-scan": StatusInfo, "plan-filesort": StatusInfo, "plan-temporary": StatusInfo,
			},
			detail: map[string]string{"plan-filesort": "DB 時計異常のため判定不能 1 件"},
		},
		{
			name:  "an unset freshness is excluded and reported as unset",
			plans: []QueryPlan{withFreshness(freshPlan("d1", fullScan), "", "")},
			want:  map[string]Status{"plan-full-scan": StatusInfo},
			detail: map[string]string{
				"plan-full-scan": "鮮度未設定 1 件",
			},
		},
		{
			name: "stale plans alongside a fresh one are only counted",
			plans: []QueryPlan{
				freshPlan("d1", row("posts", "ref", "idx_user_id", "idx_user_id", "Using where", 3)),
				withFreshness(freshPlan("d2", fullScan), PlanFreshnessStale, "after_interval"),
				withFreshness(freshPlan("d3", filesort), PlanFreshnessUnknown, "run_partial"),
			},
			want: map[string]Status{
				"plan-full-scan": StatusOK, "plan-filesort": StatusOK, "plan-temporary": StatusOK,
			},
			detail: map[string]string{
				"plan-full-scan": "判定対象外: 鮮度により除外 2 件(計測区間が partial のため判定不能 1 件, 計測区間より後のサンプル 1 件)",
			},
		},
		{
			name: "a fresh plan whose capture failed is reported, not judged",
			plans: []QueryPlan{
				{TargetID: "db1", Digest: "d1", Freshness: PlanFreshnessFresh, ErrClass: "permission_denied"},
			},
			want: map[string]Status{
				"plan-full-scan": StatusInfo, "plan-filesort": StatusInfo, "plan-temporary": StatusInfo,
			},
			detail:         map[string]string{"plan-full-scan": "取得できず 1 件(権限不足 1 件)"},
			recommendation: map[string]string{"plan-full-scan": "GRANT"},
		},
		{
			name: "failed and stale captures are counted separately",
			plans: []QueryPlan{
				{Digest: "d1", Freshness: PlanFreshnessFresh, ErrClass: "budget_exhausted"},
				{Digest: "d2", Freshness: PlanFreshnessFresh},
				withFreshness(freshPlan("d3", fullScan), PlanFreshnessStale, "before_interval"),
			},
			want: map[string]Status{"plan-temporary": StatusInfo},
			detail: map[string]string{
				"plan-temporary": "鮮度により除外 1 件(計測区間より前のサンプル 1 件)、取得できず 2 件(実行計画の行なし 1 件, 時間予算切れ 1 件)",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := byID(queryPlanChecks(tc.plans, ""))
			if len(got) != 3 {
				t.Fatalf("check count = %d, want 3", len(got))
			}
			for id, want := range tc.want {
				c, ok := got[id]
				if !ok {
					t.Fatalf("%s missing", id)
				}
				if c.Status != want {
					t.Errorf("%s status = %q, want %q (detail %q)", id, c.Status, want, c.Detail)
				}
			}
			for id, want := range tc.detail {
				if !strings.Contains(got[id].Detail, want) {
					t.Errorf("%s detail = %q, want it to contain %q", id, got[id].Detail, want)
				}
			}
			for id, want := range tc.recommendation {
				if !strings.Contains(got[id].Recommendation, want) {
					t.Errorf("%s recommendation = %q, want it to contain %q", id, got[id].Recommendation, want)
				}
			}
			for id, c := range got {
				if c.Status == StatusWarn && c.Recommendation == "" {
					t.Errorf("%s warns without a recommendation", id)
				}
			}
		})
	}
}

// TestQueryPlanNeverWarnsOnUnjudgeablePlans fixes the rule that decides whether
// this advisor is trustworthy: a sample that may predate the run must not
// produce a warning, however bad the plan looks.
func TestQueryPlanNeverWarnsOnUnjudgeablePlans(t *testing.T) {
	awful := []QueryPlanRow{
		row("posts", "ALL", "", "", "Using where; Using temporary; Using filesort", 5000000),
	}
	for _, tc := range []struct {
		freshness PlanFreshness
		reason    string
	}{
		{PlanFreshnessStale, "before_interval"},
		{PlanFreshnessStale, "after_interval"},
		{PlanFreshnessUnknown, "db_clock_anomaly"},
		{PlanFreshnessUnknown, "db_clock_missing"},
		{PlanFreshnessUnknown, "run_partial"},
		{PlanFreshnessUnknown, "interval_too_short"},
		{"", ""},
	} {
		t.Run(string(tc.freshness)+"/"+tc.reason, func(t *testing.T) {
			plans := []QueryPlan{withFreshness(freshPlan("d1", awful...), tc.freshness, tc.reason)}
			for _, c := range queryPlanChecks(plans, "") {
				if c.Status == StatusWarn || c.Status == StatusMissing {
					t.Errorf("%s = %q, want info at most for %q/%q", c.ID, c.Status, tc.freshness, tc.reason)
				}
				if c.Status != StatusInfo {
					t.Errorf("%s = %q, want info so the excluded plan is still visible", c.ID, c.Status)
				}
			}
		})
	}
}

// TestQueryPlanRecommendationsNameTheLever keeps the Japanese guidance
// actionable: each warning must name the concrete fix and say the threshold is
// still provisional.
func TestQueryPlanRecommendationsNameTheLever(t *testing.T) {
	plans := []QueryPlan{
		freshPlan("d1", row("posts", "ALL", "", "", "Using where", 20000)),
		freshPlan("d2", row("posts", "ref", "", "idx_user_id", "Using filesort", 10)),
		freshPlan("d3", row("posts", "ref", "", "idx_user_id", "Using temporary", 10)),
	}
	checks := byID(queryPlanChecks(plans, ""))
	wants := map[string][]string{
		"plan-full-scan": {"index", "possible_keys", "ref/range", "provisional"},
		"plan-filesort":  {"ORDER BY", "複合 index", "DESC", "provisional"},
		"plan-temporary": {"GROUP BY", "DISTINCT", "事前集計", "provisional"},
	}
	for id, substrings := range wants {
		if checks[id].Status != StatusWarn {
			t.Fatalf("%s = %q, want warn", id, checks[id].Status)
		}
		for _, want := range substrings {
			if !strings.Contains(checks[id].Recommendation, want) {
				t.Errorf("%s recommendation %q missing %q", id, checks[id].Recommendation, want)
			}
		}
	}
}

func TestWithQueryPlansReplacesBaselineChecks(t *testing.T) {
	baseline := Collect(context.Background(), Options{
		FS: fakeOS("4096", "32768\t60999", goodLimits, "max 100000"),
	})
	for _, id := range []string{"plan-full-scan", "plan-filesort", "plan-temporary"} {
		c, ok := byID(baseline)[id]
		if !ok {
			t.Fatalf("Collect did not emit %s", id)
		}
		if c.Status != StatusSkip {
			t.Errorf("%s = %q before capture, want skip", id, c.Status)
		}
	}

	replaced := WithQueryPlans(baseline, []QueryPlan{
		freshPlan("d1", row("posts", "ALL", "", "", "Using filesort", 20000)),
	}, nil)
	if len(replaced) != len(baseline) {
		t.Fatalf("check count changed: %d -> %d", len(baseline), len(replaced))
	}
	seen := map[string]int{}
	for _, c := range replaced {
		seen[c.ID]++
	}
	for _, id := range []string{"plan-full-scan", "plan-filesort", "plan-temporary"} {
		if seen[id] != 1 {
			t.Errorf("%s appears %d times, want exactly once", id, seen[id])
		}
	}
	m := byID(replaced)
	if m["plan-full-scan"].Status != StatusWarn || m["plan-filesort"].Status != StatusWarn {
		t.Errorf("statuses = %q/%q, want warn/warn", m["plan-full-scan"].Status, m["plan-filesort"].Status)
	}
	if m["plan-temporary"].Status != StatusOK {
		t.Errorf("plan-temporary = %q, want ok", m["plan-temporary"].Status)
	}
	for i := 1; i < len(replaced); i++ {
		if statusRank[replaced[i-1].Status] > statusRank[replaced[i].Status] {
			t.Fatalf("checks are not sorted by severity at index %d", i)
		}
	}
}

func TestWithQueryPlansCaptureError(t *testing.T) {
	checks := WithQueryPlans(nil, []QueryPlan{freshPlan("d1", row("posts", "ALL", "", "", "", 99999))},
		errors.New("explain credential not registered"))
	for _, c := range checks {
		if c.Status != StatusSkip {
			t.Errorf("%s = %q, want skip when capture failed", c.ID, c.Status)
		}
		if !strings.Contains(c.Detail, "explain credential not registered") {
			t.Errorf("%s detail = %q, want the capture error", c.ID, c.Detail)
		}
		if !strings.Contains(c.Recommendation, "ISUTOOLS_EXPLAIN=1") {
			t.Errorf("%s recommendation = %q, want the enabling hint", c.ID, c.Recommendation)
		}
	}
}

// TestQueryPlanDoesNotEchoUnvettedStrings fixes plan 09's leak rule at the
// advisor boundary: freshness reasons and error classes are closed enums on the
// producing side, and the normalized query text is not rendered at all, so no
// caller-supplied string can reach a Detail.
func TestQueryPlanDoesNotEchoUnvettedStrings(t *testing.T) {
	const marker = "ZZ_ISUTOOLS_MARKER_ZZ"
	plans := []QueryPlan{
		{
			Digest:      "d1",
			Query:       "SELECT * FROM posts WHERE title = '" + marker + "'",
			Freshness:   PlanFreshnessUnknown,
			FreshReason: marker,
		},
		{
			Digest:    "d2",
			Query:     "SELECT " + marker,
			Freshness: PlanFreshnessFresh,
			ErrClass:  marker,
		},
		{
			Digest:    "d3",
			Freshness: PlanFreshnessFresh,
			Query:     marker,
			Rows: []QueryPlanRow{
				row("posts", "ALL", "", "", "Using filesort", 90000),
			},
		},
	}
	for _, c := range queryPlanChecks(plans, "") {
		for _, field := range []string{c.ID, c.Title, c.Detail, c.Recommendation} {
			if strings.Contains(field, marker) {
				t.Errorf("%s leaked the marker: %q", c.ID, field)
			}
		}
	}
	got := byID(queryPlanChecks(plans, ""))
	if !strings.Contains(got["plan-full-scan"].Detail, "判定不能 1 件") {
		t.Errorf("unknown freshness reason = %q, want a generic label", got["plan-full-scan"].Detail)
	}
	if !strings.Contains(got["plan-full-scan"].Detail, "その他 1 件") {
		t.Errorf("unknown error class = %q, want a generic label", got["plan-full-scan"].Detail)
	}
}

func TestQueryPlanDetailTruncation(t *testing.T) {
	long := strings.Repeat("あ", 200)
	plans := []QueryPlan{{
		TargetID:  long,
		Digest:    strings.Repeat("0123456789abcdef", 4),
		Freshness: PlanFreshnessFresh,
		Rows: []QueryPlanRow{
			row(long, "ALL", "", long, "", 5000),
		},
	}}
	detail := byID(queryPlanChecks(plans, ""))["plan-full-scan"].Detail
	if !utf8.ValidString(detail) {
		t.Fatal("detail is not valid UTF-8 after truncation")
	}
	if strings.Contains(detail, long) {
		t.Error("detail carries an untruncated 200-rune identifier")
	}
	if !strings.Contains(detail, "digest=0123456789ab…") {
		t.Errorf("detail = %q, want a 12-rune digest prefix", detail)
	}
	if want := strings.Repeat("あ", planColumnRunes) + "…"; !strings.Contains(detail, want) {
		t.Errorf("detail = %q, want columns truncated to %d runes", detail, planColumnRunes)
	}
}

func TestQueryPlanDetailLimitsEnumeration(t *testing.T) {
	plans := make([]QueryPlan, 0, maxDetailItems+3)
	for i := 0; i < maxDetailItems+3; i++ {
		plans = append(plans, freshPlan("d"+string(rune('a'+i)),
			row("posts", "ALL", "", "", "", 5000)))
	}
	detail := byID(queryPlanChecks(plans, ""))["plan-full-scan"].Detail
	if !strings.Contains(detail, "他 3 件") {
		t.Errorf("detail = %q, want the enumeration limited to %d items", detail, maxDetailItems)
	}
	if !strings.Contains(detail, "type=ALL が 9 件") {
		t.Errorf("detail = %q, want the full count reported", detail)
	}
}

func TestQueryPlanMultiTargetLabels(t *testing.T) {
	db2 := freshPlan("d2", row("comments", "ALL", "", "", "", 8000))
	db2.TargetID = "db2"
	single := freshPlan("d3", row("users", "ALL", "", "", "", 8000))
	single.TargetID = ""
	detail := byID(queryPlanChecks([]QueryPlan{db2, single}, ""))["plan-full-scan"].Detail
	if !strings.Contains(detail, "db2: comments(") {
		t.Errorf("detail = %q, want the target id to prefix a multi-host finding", detail)
	}
	if !strings.Contains(detail, "users(digest=") || strings.Contains(detail, ": users(") {
		t.Errorf("detail = %q, want no prefix when the target id is empty", detail)
	}
}

func TestFreshnessLabels(t *testing.T) {
	cases := []struct {
		plan QueryPlan
		want string
	}{
		{QueryPlan{Freshness: PlanFreshnessStale, FreshReason: "before_interval"}, "計測区間より前のサンプル"},
		{QueryPlan{Freshness: PlanFreshnessStale, FreshReason: "after_interval"}, "計測区間より後のサンプル"},
		{QueryPlan{Freshness: PlanFreshnessUnknown, FreshReason: "db_clock_anomaly"}, "DB 時計異常のため判定不能"},
		{QueryPlan{Freshness: PlanFreshnessUnknown, FreshReason: "db_clock_missing"}, "DB 側時計情報なしのため判定不能"},
		{QueryPlan{Freshness: PlanFreshnessUnknown, FreshReason: "run_partial"}, "計測区間が partial のため判定不能"},
		{QueryPlan{Freshness: PlanFreshnessUnknown, FreshReason: "interval_too_short"}, "計測区間が短すぎて判定不能"},
		{QueryPlan{Freshness: PlanFreshnessFresh, FreshReason: "in_interval"}, "判定不能"},
		{QueryPlan{Freshness: PlanFreshnessStale}, "計測区間外のサンプル"},
		{QueryPlan{Freshness: PlanFreshnessUnknown}, "判定不能"},
		{QueryPlan{}, "鮮度未設定"},
		{QueryPlan{Freshness: PlanFreshnessUnknown, FreshReason: "invented_by_a_future_version"}, "判定不能"},
	}
	for _, tc := range cases {
		if got := freshnessLabel(tc.plan); got != tc.want {
			t.Errorf("freshnessLabel(%q/%q) = %q, want %q",
				tc.plan.Freshness, tc.plan.FreshReason, got, tc.want)
		}
	}
}

func TestPlanErrorLabels(t *testing.T) {
	cases := map[string]string{
		"":                             "実行計画の行なし",
		"timeout":                      "タイムアウト",
		"budget_exhausted":             "時間予算切れ",
		"permission_denied":            "権限不足",
		"syntax_or_truncated":          "構文エラーまたはサンプル切り詰め",
		"object_missing":               "対象オブジェクトなし",
		"sample_unavailable":           "サンプルなし",
		"sample_possibly_truncated":    "サンプル切り詰めの疑い",
		"connection_error":             "接続エラー",
		"other":                        "その他",
		"invented_by_a_future_version": "その他",
	}
	for class, want := range cases {
		if got := planErrorLabel(class); got != want {
			t.Errorf("planErrorLabel(%q) = %q, want %q", class, got, want)
		}
	}
}

func TestShortDigestFallsBackWhenEmpty(t *testing.T) {
	detail := byID(queryPlanChecks([]QueryPlan{{
		Freshness: PlanFreshnessFresh,
		Rows:      []QueryPlanRow{row("posts", "ALL", "", "", "", 4000)},
	}}, ""))["plan-full-scan"].Detail
	if !strings.Contains(detail, "digest=不明") {
		t.Errorf("detail = %q, want a placeholder for a missing digest", detail)
	}
}

func TestCollectEmitsQueryPlanSkips(t *testing.T) {
	checks := byID(Collect(context.Background(), Options{FS: fstest.MapFS{}}))
	for _, id := range []string{"plan-full-scan", "plan-filesort", "plan-temporary"} {
		if checks[id].Status != StatusSkip {
			t.Errorf("%s = %q, want skip when no capture ran", id, checks[id].Status)
		}
		if !strings.Contains(checks[id].Recommendation, "RegisterDBInspector") {
			t.Errorf("%s recommendation = %q, want the capture setup hint", id, checks[id].Recommendation)
		}
	}
}
