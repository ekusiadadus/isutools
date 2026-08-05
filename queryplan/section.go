package queryplan

import (
	"sort"
	"strings"
	"time"
)

// Reason IDs for a target that produced no plans. They are stable identifiers:
// the dashboard maps them to labels and the ABBA gate greps for them, so a new
// condition gets a new ID rather than a reworded message.
const (
	// CodePurposeUnregistered means the target has no PurposeExplain
	// credential. It is never treated as a reason to use another one.
	CodePurposeUnregistered = "explain-purpose-unregistered"
	// CodeUnknownTarget means the registry does not know the target ID the
	// interval was published under.
	CodeUnknownTarget = "explain-unknown-target"
	// CodeNoSchema means there is no schema name that can be bound to
	// WHERE SCHEMA_NAME = ? and quoted into USE.
	CodeNoSchema = "explain-no-schema"
	// CodeUnsupported means the server has no QUERY_SAMPLE_TEXT column, i.e.
	// it is older than MySQL 8.0.17 or is MariaDB.
	CodeUnsupported = "explain-unsupported"
	// CodeRolesActive means active roles could neither be neutralised nor
	// enumerated, so the effective privileges are unknown.
	CodeRolesActive = "explain-roles-active"
	// CodeGrantsTooBroad means the effective privileges include something
	// outside the allowlist.
	CodeGrantsTooBroad = "explain-grants-too-broad"
	// CodeGrantsUnverifiable means SHOW GRANTS could not be read or could not
	// be parsed. An unknown grant line is treated as a dangerous one.
	CodeGrantsUnverifiable = "explain-grants-unverifiable"
	// CodeSessionInstrumented means the session could not be proven
	// uninstrumented, so its own statements would land in the interval the
	// next run measures.
	CodeSessionInstrumented = "explain-session-instrumented"
	// CodeBudgetExhausted means the enrich budget ran out before this
	// target's wave could start. Recorded rather than dropped.
	CodeBudgetExhausted = "explain-budget-exhausted"
	// CodeTargetTimeout means the target's session did start but had not
	// returned when the enrich budget expired, so the capture stopped waiting
	// for it. Unlike CodeBudgetExhausted it points at one connection rather
	// than at the fan-out: statements were issued, and something — a driver
	// that ignores its context, a connection stuck in a syscall, a proxy that
	// never answered — did not come back inside the budget.
	CodeTargetTimeout = "explain-target-timeout"
	// CodeQueryError means a statement of the session sequence failed for a
	// reason none of the codes above describes.
	CodeQueryError = "explain-query-error"
	// CodeNoInterval means sqlrows published no usable interval for the
	// target, so there is nothing to rank digests by.
	CodeNoInterval = "explain-no-interval"
	// CodeNoDigests means the interval holds no SELECT digest worth
	// explaining.
	CodeNoDigests = "explain-no-digests"
	// CodeNoDefaultDatabase means the server answered EXPLAIN with 1046 "No
	// database selected", i.e. the USE step did not take effect. That is a
	// fault of this package rather than of the statement, so it is reported in
	// health as well as on the plan.
	CodeNoDefaultDatabase = "explain-no-default-database"
)

// reasons maps a reason ID to the sentence shown to the operator.
//
// Every reason a target can carry comes from this table. That is a leak
// control as much as a style rule: a reason built with fmt from a driver error
// would be exactly the path by which a statement's literals reach a published
// snapshot.
var reasons = map[string]string{
	CodePurposeUnregistered: "EXPLAIN 専用 credential が未登録です — RegisterDBInspector(id, PurposeExplain, ...) で登録してください(app / stats の credential へは fallback しません)",
	CodeUnknownTarget:       "この target ID は registry に登録されていません",
	CodeNoSchema:            "schema 名が空、または識別子として使えない文字を含みます",
	CodeUnsupported:         "QUERY_SAMPLE_TEXT 列がありません — EXPLAIN 自動化は MySQL 8.0.17 以降のみ対応です",
	CodeRolesActive:         "SET ROLE NONE が効かず、有効な role を特定できませんでした",
	CodeGrantsTooBroad:      "EXPLAIN 用ユーザーの実効権限が広すぎます(対象 schema と performance_schema への SELECT 以外を持っています)",
	CodeGrantsUnverifiable:  "SHOW GRANTS を読めない、または解釈できない行があったため権限を検証できませんでした",
	CodeSessionInstrumented: "セッションを非計装にできませんでした(performance_schema.threads の UPDATE 権限か setup_actors の設定が必要です)",
	CodeBudgetExhausted:     "enrich の予算が尽きたため、この target は実行しませんでした",
	CodeTargetTimeout:       "enrich の予算内に EXPLAIN セッションが返らなかったため、この target の待機を打ち切りました(driver が context を無視している可能性があります)",
	CodeQueryError:          "EXPLAIN セッションの文が失敗しました",
	CodeNoInterval:          "区間統計が使えない target のため EXPLAIN を実行しませんでした",
	CodeNoDigests:           "区間内に EXPLAIN 対象の SELECT digest がありません",
	CodeNoDefaultDatabase:   "既定 DB が設定されておらず EXPLAIN が「No database selected」で失敗しました(USE が効いていません)",
}

// quietCodes are the reason IDs that do not produce a health note. Both are
// already reported by sqlrows (no usable interval) or are the normal shape of
// a run with no interesting SELECT traffic, and repeating them would bury the
// codes an operator has to act on.
var quietCodes = map[string]bool{
	CodeNoInterval: true,
	CodeNoDigests:  true,
}

// Section is the snapshot section this package contributes.
//
// It carries no Validity: EXPLAIN capture is optional enrichment, and the
// run's verdict is sqlrows' to lower. A target that produced nothing says so
// with a reason ID instead of degrading the run.
type Section struct {
	// Targets is ordered by TargetID so two snapshots diff cleanly.
	Targets []TargetSection `json:"targets"`
	// Health carries this section's degradation notes, grouped by reason.
	Health []HealthNote `json:"health,omitempty"`
	// Top is the selection ceiling this run used, recorded so a short plan
	// list can be told from a truncated one.
	Top int `json:"top"`
}

// TargetSection is one target's plans.
type TargetSection struct {
	TargetID string `json:"target_id"`
	Schema   string `json:"schema,omitempty"`
	// Explained reports that a session was established and the plans below
	// were attempted on the database. It is false both for a skipped target
	// and for one whose freshness verdict was decided without connecting.
	Explained bool `json:"explained"`
	// Code and Reason explain a target that produced no plans. Reason is
	// always one of the fixed sentences in reasons.
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Plans keeps the interval's ranking order: highest total time first.
	Plans []Plan `json:"plans,omitempty"`
}

// Plan is one digest's execution plan.
//
// There is deliberately no field a sample's text could be stored in. Query is
// the normalized DIGEST_TEXT sqlrows published, and Err is a classification
// rather than a message.
type Plan struct {
	Digest string `json:"digest"`
	// Query is sqlrows' truncated DIGEST_TEXT — normalized, literal-free.
	Query string `json:"query"`
	// SampleSeen is QUERY_SAMPLE_SEEN, the database's own clock reading of
	// when the explained example ran.
	SampleSeen time.Time `json:"sample_seen"`
	// Freshness says whether that reading falls inside the measured interval.
	Freshness FreshnessState `json:"freshness"`
	// FreshReason is the closed reason enum behind Freshness.
	FreshReason FreshReason `json:"fresh_reason,omitempty"`
	// Rows is the EXPLAIN output, empty when Err says why there is none.
	Rows []PlanRow  `json:"rows,omitempty"`
	Err  *PlanError `json:"err,omitempty"`
}

// PlanRow is one row of EXPLAIN output.
//
// Every column is a pointer because every column of MySQL's EXPLAIN can be
// NULL — an impossible WHERE produces a row that is almost entirely NULL — and
// a missing column has to render as an empty cell rather than as a parse
// failure.
type PlanRow struct {
	SelectType   *string `json:"select_type,omitempty"`
	Table        *string `json:"table,omitempty"`
	Type         *string `json:"type,omitempty"`
	Key          *string `json:"key,omitempty"`
	PossibleKeys *string `json:"possible_keys,omitempty"`
	Rows         *int64  `json:"rows,omitempty"`
	Extra        *string `json:"extra,omitempty"`
}

// HealthNote is one grouped degradation message. Key is the reason ID.
type HealthNote struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// noteSet accumulates health notes so that eight targets skipped for one
// reason produce one line instead of eight. It is filled after the fan-out has
// joined, so it needs no lock of its own.
type noteSet struct {
	order   []string
	targets map[string][]string
}

func newNoteSet() *noteSet { return &noteSet{targets: map[string][]string{}} }

// add records that target degraded under code.
func (n *noteSet) add(code, target string) {
	if _, seen := n.targets[code]; !seen {
		n.order = append(n.order, code)
	}
	n.targets[code] = append(n.targets[code], target)
}

// notes renders the accumulated groups, sorted by reason ID. Target lists keep
// insertion order, which is TargetID order.
func (n *noteSet) notes() []HealthNote {
	if len(n.order) == 0 {
		return nil
	}
	codes := append([]string(nil), n.order...)
	sort.Strings(codes)
	out := make([]HealthNote, 0, len(codes))
	for _, code := range codes {
		out = append(out, HealthNote{
			Key:     code,
			Message: Name + "[" + strings.Join(n.targets[code], ", ") + "]: skip (" + reasons[code] + ")",
		})
	}
	return out
}
