package runctl

import (
	"fmt"
	"time"
)

// Hierarchical time budgets. This package is the single authority for these
// numbers: downstream collectors cite them instead of inventing their own, so
// that "why did my collector get cut off?" always has one answer.
//
// The hierarchy is strict — a run budget bounds a phase budget, which bounds a
// per-collector budget, which bounds a per-target budget. TestBudgetHierarchy
// pins the inequalities so a future tweak cannot quietly invert them.
const (
	// StartRunBudget bounds the whole synchronous part of StartRun.
	StartRunBudget = 6 * time.Second
	// FinishSyncBudget bounds the whole synchronous part of FinishRun
	// (freeze + final sampling). Draining and snapshot building happen after
	// the response, outside this budget.
	FinishSyncBudget = 6 * time.Second

	// PhaseStartBoundaryBudget bounds BeginBoundary across all generation
	// collectors. Generation swaps are pointer swaps, so this is generous.
	PhaseStartBoundaryBudget = 500 * time.Millisecond
	// PhaseStartBaselineBudget bounds CaptureBaseline across all baseline
	// collectors. Baseline sampling does bounded I/O (procfs, DB), hence the
	// order-of-magnitude difference from the boundary phase.
	PhaseStartBaselineBudget = 5 * time.Second
	// PhaseFinishFreezeBudget bounds Freeze across all generation collectors.
	PhaseFinishFreezeBudget = 500 * time.Millisecond
	// PhaseFinishFinalBudget bounds CaptureFinal across all baseline collectors.
	PhaseFinishFinalBudget = 5 * time.Second

	// PerCollectorGenerationBudget bounds one boundary operation of one
	// generation collector.
	PerCollectorGenerationBudget = 100 * time.Millisecond
	// PerCollectorBaselineBudget bounds one sampling operation of one baseline
	// collector, including all of the DB targets it fans out to.
	PerCollectorBaselineBudget = 3500 * time.Millisecond
	// PerTargetBudget bounds one collector's call against one DB target.
	PerTargetBudget = 1 * time.Second

	// DrainBudget bounds draining every handle of one run. Background work.
	DrainBudget = 10 * time.Second
	// DrainCancelGrace is how long a collector's Drain may take to return
	// after its context is done. Collectors must wait on a per-generation done
	// channel rather than sync.Cond, because sync.Cond.Wait cannot be
	// interrupted by a context and would make this contract unimplementable.
	DrainCancelGrace = 1 * time.Second
	// SnapshotBuildBudget bounds Collect plus immutable snapshot construction.
	SnapshotBuildBudget = 5 * time.Second
	// EnrichBudget bounds post-freeze enrichment such as EXPLAIN capture.
	EnrichBudget = 2 * time.Second
	// AbortJoinBudget bounds how long AbortRun waits for the run's worker.
	// Exceeding it is safe but noisy: the worker is detached and fenced.
	AbortJoinBudget = 2 * time.Second
	// PreemptTotalBudget bounds the whole preempt path.
	PreemptTotalBudget = AbortJoinBudget + StartRunBudget
	// DetachedReapBudget bounds how long the reaper watches a detached worker
	// before giving up on reclaiming its handles.
	DetachedReapBudget = 60 * time.Second
	// InitializeGuardBudget bounds how long SerializeInitialize waits for the
	// process-wide initialize guard.
	InitializeGuardBudget = 30 * time.Second
)

// Lease and TTL defaults. All of them are injectable through Budgets so tests
// never wait out a real 30 minute TTL.
const (
	// FinishLease is how long one phase of a finish may live before the
	// watchdog force-aborts the run. It is armed twice: FinishRun arms it over
	// the synchronous freeze (hence FinishSyncBudget < FinishLease), and the
	// background worker re-arms it when it takes over (hence
	// DrainBudget+SnapshotBuildBudget+EnrichBudget < FinishLease). Both halves
	// are therefore bounded by it, and neither is charged for the other's time.
	FinishLease = 20 * time.Second
	// StartedTTL reclaims runs that nobody ever finished.
	StartedTTL = 30 * time.Minute
	// FinishedTTL is how long a finished or acknowledged snapshot is retained.
	FinishedTTL = 10 * time.Minute
	// TombstoneTTL is how long aborted and expired records are retained.
	TombstoneTTL = 10 * time.Minute
	// NonceTTL bounds the StartRun idempotency cache.
	NonceTTL = 10 * time.Minute
	// WatchdogInterval is how often leases and TTLs are examined.
	WatchdogInterval = 1 * time.Second
)

// Structural limits.
const (
	// BaselineConcurrency caps parallel baseline sampling. Parallelism is what
	// keeps the boundary window narrow; the cap keeps the measured application
	// from being hit by a thundering herd of collectors.
	BaselineConcurrency = 8
	// NonceHistoryMax bounds the nonce idempotency cache.
	NonceHistoryMax = 64
	// RetainedRuns is how many run records the Controller keeps, *including*
	// the in-flight one. Two generations back is therefore always a 404.
	RetainedRuns = 2
)

// Boundary spread limits. These are deliberately unrelated to the budgets
// above: a budget is a forced cutoff that protects the application, a spread
// limit is a quality threshold that decides whether the boundary is usable as
// a boundary. "Inside budget but over spread" is a normal, expected verdict.
const (
	// SpreadLimitGeneration caps the measured width of the generation swap
	// window. Expected in practice: well under a millisecond.
	SpreadLimitGeneration = 50 * time.Millisecond
	// SpreadLimitBoundary caps the measured width of the whole boundary
	// window, generation and baseline collectors together. Expected in
	// practice: under 200ms with parallel sampling.
	SpreadLimitBoundary = 1500 * time.Millisecond
)

// Budgets is the injectable form of the constants above. A zero field falls
// back to its package constant, so callers override only what they care about
// and tests can shrink the whole table without waiting out real leases.
type Budgets struct {
	StartRun      time.Duration
	FinishSync    time.Duration
	PhaseBoundary time.Duration // BeginBoundary phase (start side)
	PhaseBaseline time.Duration // CaptureBaseline phase (start side)
	PhaseFreeze   time.Duration // Freeze phase (finish side)
	PhaseFinal    time.Duration // CaptureFinal phase (finish side)

	PerCollectorGeneration time.Duration
	PerCollectorBaseline   time.Duration

	Drain         time.Duration
	SnapshotBuild time.Duration
	Enrich        time.Duration
	AbortJoin     time.Duration
	DetachedReap  time.Duration

	FinishLease  time.Duration
	StartedTTL   time.Duration
	FinishedTTL  time.Duration
	TombstoneTTL time.Duration
	NonceTTL     time.Duration
	Watchdog     time.Duration

	SpreadGeneration time.Duration
	SpreadBoundary   time.Duration
}

// withDefaults returns a copy in which every zero field carries its package
// default. It never mutates the receiver.
func (b Budgets) withDefaults() Budgets {
	out := b
	fill := func(dst *time.Duration, def time.Duration) {
		if *dst <= 0 {
			*dst = def
		}
	}
	fill(&out.StartRun, StartRunBudget)
	fill(&out.FinishSync, FinishSyncBudget)
	fill(&out.PhaseBoundary, PhaseStartBoundaryBudget)
	fill(&out.PhaseBaseline, PhaseStartBaselineBudget)
	fill(&out.PhaseFreeze, PhaseFinishFreezeBudget)
	fill(&out.PhaseFinal, PhaseFinishFinalBudget)
	fill(&out.PerCollectorGeneration, PerCollectorGenerationBudget)
	fill(&out.PerCollectorBaseline, PerCollectorBaselineBudget)
	fill(&out.Drain, DrainBudget)
	fill(&out.SnapshotBuild, SnapshotBuildBudget)
	fill(&out.Enrich, EnrichBudget)
	fill(&out.AbortJoin, AbortJoinBudget)
	fill(&out.DetachedReap, DetachedReapBudget)
	fill(&out.FinishLease, FinishLease)
	fill(&out.StartedTTL, StartedTTL)
	fill(&out.FinishedTTL, FinishedTTL)
	fill(&out.TombstoneTTL, TombstoneTTL)
	fill(&out.NonceTTL, NonceTTL)
	fill(&out.Watchdog, WatchdogInterval)
	fill(&out.SpreadGeneration, SpreadLimitGeneration)
	fill(&out.SpreadBoundary, SpreadLimitBoundary)
	return out
}

// Validate enforces the budget hierarchy on an already-defaulted table.
// Violations are rejected at construction time because an inverted budget
// produces timeouts that are impossible to explain from a snapshot alone.
func (b Budgets) Validate() error {
	type rule struct {
		child, parent time.Duration
		childName     string
		parentName    string
		orEqual       bool
	}
	rules := []rule{
		{b.PhaseBaseline, b.StartRun, "PhaseBaseline", "StartRun", false},
		{b.PerCollectorBaseline, b.PhaseBaseline, "PerCollectorBaseline", "PhaseBaseline", false},
		{b.PhaseBoundary, b.StartRun, "PhaseBoundary", "StartRun", false},
		{b.PerCollectorGeneration, b.PhaseBoundary, "PerCollectorGeneration", "PhaseBoundary", false},
		{b.PhaseFinal, b.FinishSync, "PhaseFinal", "FinishSync", false},
		{b.PerCollectorBaseline, b.PhaseFinal, "PerCollectorBaseline", "PhaseFinal", false},
		{b.PhaseFreeze, b.FinishSync, "PhaseFreeze", "FinishSync", false},
		{b.PerCollectorGeneration, b.PhaseFreeze, "PerCollectorGeneration", "PhaseFreeze", false},
		{b.PhaseBoundary + b.PhaseBaseline, b.StartRun, "PhaseBoundary+PhaseBaseline", "StartRun", true},
		{b.PhaseFreeze + b.PhaseFinal, b.FinishSync, "PhaseFreeze+PhaseFinal", "FinishSync", true},
		// Both halves of a finish run under a lease of their own: the
		// synchronous freeze under the one FinishRun arms, the background work
		// under the one the worker re-arms. Stating the synchronous half here
		// is what stops a table where a freeze inside its budget is aborted as
		// a stuck worker.
		{b.FinishSync, b.FinishLease, "FinishSync", "FinishLease", false},
		{b.Drain + b.SnapshotBuild + b.Enrich, b.FinishLease, "Drain+SnapshotBuild+Enrich", "FinishLease", false},
	}
	for _, r := range rules {
		ok := r.child < r.parent
		if r.orEqual {
			ok = r.child <= r.parent
		}
		if !ok {
			return fmt.Errorf("%w: %s(%v) vs %s(%v)", ErrBudgetInversion, r.childName, r.child, r.parentName, r.parent)
		}
	}
	return nil
}

// BudgetAware is the optional interface a collector implements to declare the
// per-operation budget it needs. Registration rejects a collector asking for
// more than its per-collector budget allows, so the mismatch is reported where
// it can be fixed instead of showing up as a truncated measurement.
type BudgetAware interface {
	Budget() time.Duration
}
