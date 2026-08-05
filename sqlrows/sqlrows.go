// Package sqlrows measures per-digest row efficiency — rows examined against
// rows sent — over a benchmark run, by sampling
// performance_schema.events_statements_summary_by_digest at both run
// boundaries and reporting the difference.
//
// The counters in that table are cumulative since the server started, so a
// single reading says nothing about a run. sqlrows takes a full reading at the
// opening boundary, another at the closing boundary, and derives the interval
// afterwards, which is why it implements runctl.BaselineCollector rather than
// accumulating anything of its own.
//
// # Self-contamination
//
// The collector's own statements are recorded by performance_schema just like
// the application's. The connection it samples on therefore deliberately has
// no default database (see sqlstats connection hygiene), which makes MySQL
// attribute every statement issued here to a NULL schema, and the target
// schema is passed as a bound parameter (WHERE SCHEMA_NAME = ?) rather than
// through DATABASE(). A statement of this package can consequently never match
// the application schema's rows. Nothing in this package may use DATABASE().
//
// That hygiene is verified, not assumed. The registry can only remove the
// default database from a DSN it is able to rebuild — a URL-form DSN reaches
// the driver unchanged and keeps the application's schema — so the first probe
// of every target reads the session's default database out of
// performance_schema.threads. A target whose connection has one, or whose
// connection cannot be checked, is skipped with CodeInspectorDefaultDB instead
// of measured: numbers that silently include the measurement's own statements
// are worse than no numbers, and the operator is told which target to
// re-register.
//
// Because statements from a connection without a default database land on
// SCHEMA_NAME IS NULL, that condition alone does not identify the digest
// table's overflow row: the overflow row is the one where SCHEMA_NAME and
// DIGEST are *both* NULL. See TargetSample.Overflow.
package sqlrows

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

const (
	// Name is the collector name and the snapshot section key.
	Name = "sqlrows"

	// DigestTextFetchLimit bounds both how many digest texts are fetched at
	// the closing boundary and how many rows a target reports. The delta is
	// always computed over every digest — truncation happens after the
	// subtraction, never before it, because a digest that was outside the top
	// N at the opening boundary would otherwise contribute its whole
	// historical total to the interval.
	DigestTextFetchLimit = 200

	// digestTextMaxBytes bounds one stored DIGEST_TEXT. The server-side LEFT()
	// already truncates; this is the defensive second cut.
	digestTextMaxBytes = 512

	// EnvFlag disables the collector when set to a false-ish value.
	EnvFlag = "ISUTOOLS_SQLROWS"
)

// Errors returned by the collector. They are sentinels so the run controller
// and tests can distinguish "nothing to measure" from "measurement broke".
var (
	// ErrNoTargetCaptured reports that every registered target failed to be
	// sampled (unreachable, permission denied, or out of budget). It is
	// returned together with a SampleResult whose Committed is false, which is
	// what tells runctl to degrade the run instead of trusting an empty
	// interval. Targets that were merely skipped — performance_schema off, no
	// schema — do not produce this error: having nothing to measure is not a
	// failure.
	ErrNoTargetCaptured = errors.New("sqlrows: no db target could be sampled")

	// ErrSampleType reports a handle that does not carry a *Sample. The
	// collector contract forbids panicking here: measurement must never break
	// the measured application.
	ErrSampleType = errors.New("sqlrows: baseline handle does not carry a sqlrows sample")
)

// InspectFunc is the registry entry point the collector uses to reach a
// target. It matches sqlstats.Inspect and exists as a named type so tests can
// drive the collector without a database.
type InspectFunc func(ctx context.Context, id string, purpose sqlstats.Purpose, fn func(context.Context, sqlstats.Querier) error) error

// Collector samples the digest table at both run boundaries.
//
// It holds no accumulating state: everything a snapshot needs travels inside
// the runctl.BaselineHandle, so Collect can derive an interval from two frozen
// samples without touching the database, the registry, or the fields below.
type Collector struct {
	mu sync.Mutex
	// pending maps a run to the sample its opening boundary took. Only
	// CaptureFinal reads it, and only to decide which digest texts are worth
	// fetching; Collect never does.
	pending map[runKey]*Sample
	// results caches each boundary's outcome so a replayed (runID, epoch)
	// returns byte-identical values, error included.
	results map[resultKey]cachedResult
	// probes caches capability probing per target ID, so the four probe
	// statements are paid once per process rather than once per boundary.
	probes map[string]probeResult
	// latest is the highest epoch seen. Anything older is fenced.
	latest runctl.Epoch

	targets     func() []sqlstats.TargetInfo
	inspect     InspectFunc
	now         func() time.Time
	concurrency int
	perTarget   time.Duration
}

// runKey identifies one run boundary pair.
type runKey struct {
	runID string
	epoch runctl.Epoch
}

// resultKey identifies one boundary of one run.
type resultKey struct {
	run   runKey
	phase runctl.Phase
}

// cachedResult is a boundary outcome kept for idempotent replay. The error is
// cached with the result because runctl treats (result, error) as one value.
type cachedResult struct {
	res runctl.SampleResult
	err error
}

// New returns a collector bound to the process-wide DB target registry.
//
// The fan-out numbers are runctl's, not this package's: runctl is the single
// authority for time budgets, and a collector inventing its own would make
// "why was my target dropped?" unanswerable.
func New() *Collector {
	return &Collector{
		pending:     map[runKey]*Sample{},
		results:     map[resultKey]cachedResult{},
		probes:      map[string]probeResult{},
		targets:     sqlstats.Targets,
		inspect:     sqlstats.Inspect,
		now:         time.Now,
		concurrency: runctl.BaselineConcurrency,
		perTarget:   runctl.PerTargetBudget,
	}
}

// Registration describes how sqlrows participates in a run. It is optional:
// a database without performance_schema must degrade the run to partial, not
// invalidate it.
func Registration() runctl.Registration {
	return runctl.Registration{Name: Name, Required: false}
}

// Enabled reports whether the collector should be wired in. Measurement of the
// measurement is what the flag is for: ISUTOOLS_SQLROWS=off removes every
// statement this package issues, which is how the ABBA overhead gate compares
// runs with and without it.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvFlag))) {
	case "off", "0", "false", "no", "disabled":
		return false
	default:
		return true
	}
}

// Name identifies the snapshot section this collector fills.
func (c *Collector) Name() string { return Name }

// Budget declares the time one boundary of this collector needs, so a
// misconfigured budget table is rejected at registration instead of showing up
// as a truncated measurement.
//
// The number is derived, not invented: sqlstats.MaxTargets (16) targets over
// runctl.BaselineConcurrency (8) is two waves, and each wave is bounded by
// runctl.PerTargetBudget.
func (c *Collector) Budget() time.Duration {
	waves := (sqlstats.MaxTargets + runctl.BaselineConcurrency - 1) / runctl.BaselineConcurrency
	return time.Duration(waves) * runctl.PerTargetBudget
}
