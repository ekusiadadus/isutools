// Package queryplan runs EXPLAIN against the statements that dominated a
// benchmark run and publishes the resulting plans.
//
// # Where the statement text comes from
//
// The proxy driver never sees an interpolated statement — it is handed the
// query and its arguments separately — so there is no raw SQL to keep. The
// text explained here is MySQL's own QUERY_SAMPLE_TEXT, one recorded example
// per digest, read back at the end of the run. That text carries literals, and
// therefore never leaves the callback that read it: nothing on Plan, on
// Section, or on any error this package builds can hold it. Plan.Query is the
// normalized DIGEST_TEXT sqlrows already published, and a failed EXPLAIN is
// reduced to a closed classification plus the driver's numeric error code (see
// PlanError). A driver message is neither stored nor logged, because MySQL's
// 1064 quotes a fragment of the statement back at the caller.
//
// # Credential
//
// EXPLAIN runs on the least-privilege PurposeExplain credential only. When a
// target has none, the target is skipped and the reason is recorded; the
// application's own credential is never used as a substitute, because
// EXPLAIN SELECT can still have side effects through a stored function and the
// restricted user is what rules that out. The privileges of that user are
// verified on the very connection the EXPLAIN runs on — roles are neutralised
// and expanded, and the effective grants are checked against an allowlist —
// rather than trusted from configuration.
//
// # When it runs
//
// Capture belongs in runctl's Enrich hook, which runs once per run after the
// interval has been collected and before the snapshot is published, inside
// runctl.EnrichBudget. It must not be wired to a GET or to a non-terminal
// flush: those have no interval to rank digests by, and would put EXPLAIN
// statements on the measured database every time someone opens the dashboard.
package queryplan

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/sqlrows"
)

const (
	// Name is the snapshot section key.
	Name = "queryplan"

	// EnvFlag is the master flag, and it is off unless it is explicitly
	// turned on: EXPLAIN issues extra statements against the measured
	// database, so it is opt-in rather than opt-out.
	EnvFlag = "ISUTOOLS_EXPLAIN"

	// EnvTop overrides how many digests of one target are explained.
	EnvTop = "ISUTOOLS_EXPLAIN_TOP"

	// DefaultTop is how many SELECT digests per target are selected when
	// EnvTop is unset. It is a selection ceiling, not a promise: the digest
	// loop stops as soon as the per-target budget can no longer fit another
	// EXPLAIN.
	DefaultTop = 10
)

// MaxTop bounds EnvTop. It is sqlrows' own row limit rather than a new
// number, because a digest outside that limit has no interval row to attach a
// plan to.
const MaxTop = sqlrows.DigestTextFetchLimit

// Budgets carved out of runctl.PerTargetBudget.
//
// runctl owns the hierarchy — this package cites PerTargetBudget (1s) and
// EnrichBudget (2s) and never redefines them — but how one target's second is
// divided is this package's business. The three numbers below add up to 650ms,
// which leaves the digest loop the remainder of a target's second after
// session establishment and the sample read.
const (
	// SessionBudget bounds session establishment: role neutralisation, grant
	// verification, de-instrumentation, capability probing and USE.
	SessionBudget = 300 * time.Millisecond
	// SampleBudget bounds the single statement that reads every sample.
	SampleBudget = 100 * time.Millisecond
	// PerDigestBudget bounds one EXPLAIN. It is a cutoff, not a reservation:
	// a real EXPLAIN takes single-digit milliseconds, and reserving ten of
	// these would exceed the whole enrich budget on the first target.
	PerDigestBudget = 250 * time.Millisecond

	// startupBudget is the smallest slice of the enrich budget a target can
	// usefully start with. A wave that cannot fit it is not started at all:
	// opening a connection that will be cancelled before the sample read
	// costs the measured database a connection for nothing.
	startupBudget = SessionBudget + SampleBudget
)

// defaultMaxSQLTextLength is performance_schema_max_sql_text_length's own
// default, used when the variable cannot be read. Assuming the default is the
// conservative choice: it makes long samples look truncated (and be skipped)
// rather than syntactically complete.
const defaultMaxSQLTextLength = 1024

// Enabled reports whether EXPLAIN capture should be wired in at all.
//
// Capture itself does not consult the environment: the flag is the
// integration point's gate, so that a test can drive Capture without setting
// process-wide state, and so that "the feature is off" means "no statement was
// ever issued" rather than "the statements were issued and discarded".
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvFlag))) {
	case "1", "on", "true", "yes", "enabled":
		return true
	default:
		return false
	}
}

// TopN reports the configured selection ceiling, falling back to DefaultTop
// for an unset, unparseable, non-positive or oversized value. A misspelt
// number degrades to the default instead of disabling the feature, because a
// silently empty section is the harder failure to notice.
func TopN() int {
	raw := strings.TrimSpace(os.Getenv(EnvTop))
	if raw == "" {
		return DefaultTop
	}
	n, err := strconv.Atoi(raw)
	switch {
	case err != nil, n <= 0:
		return DefaultTop
	case n > MaxTop:
		return MaxTop
	default:
		return n
	}
}
