package queryplan

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// InspectFunc is the registry entry point this package reaches targets
// through. It matches sqlstats.Inspect and exists as a named type so tests can
// drive a capture without a database.
type InspectFunc func(ctx context.Context, id string, purpose sqlstats.Purpose, fn func(context.Context, sqlstats.Querier) error) error

// ErrNoInterval reports that Capture was called without an interval to enrich.
// It is the one error Capture returns: everything else is a reason recorded on
// a target, because enrichment must never fail a run.
var ErrNoInterval = errors.New("queryplan: no sqlrows section to enrich")

// Input is everything Capture needs.
//
// The interval comes in rather than being fetched, because ranking digests and
// judging freshness are the same decisions sqlrows already made: the digests
// to explain are its top rows, and the window a sample must fall into is its
// DBClock. Re-reading either from the database would produce a second opinion
// that could disagree with the numbers shown next to the plans.
type Input struct {
	// Rows is the interval sqlrows published for the run being enriched.
	Rows *sqlrows.Section
	// Top bounds how many SELECT digests of one target are explained. Zero
	// means TopN(), i.e. ISUTOOLS_EXPLAIN_TOP or DefaultTop.
	Top int
	// Inspect defaults to sqlstats.Inspect. It is always called with
	// PurposeExplain; a target without that credential is skipped.
	Inspect InspectFunc
	// Now defaults to time.Now. It is only ever used to measure the remaining
	// budget — never to judge a sample's freshness, which is decided on the
	// database's own clock.
	Now func() time.Time
	// Concurrency defaults to runctl.BaselineConcurrency.
	Concurrency int
}

// Capture explains the statements that dominated the run and returns the
// section to publish.
//
// It is shaped for runctl's Enrich hook: called once per run, after the
// interval exists and before the snapshot is published, with the enrich
// budget's context. The returned error is only ever ErrNoInterval — a target
// that could not be explained comes back as a reason ID on that target, since
// a run must not be degraded by an optional extra. The returned Section is
// always non-nil.
//
//	sec, err := queryplan.Capture(ctx, queryplan.Input{Rows: rowsSection})
//	if err == nil {
//	    snap.Sections[queryplan.Name] = sec
//	}
func Capture(ctx context.Context, in Input) (*Section, error) {
	r := newRunner(in)
	if in.Rows == nil {
		return &Section{Targets: []TargetSection{}, Top: r.top}, ErrNoInterval
	}
	fixed, runnable := r.plan(in.Rows)
	return r.assemble(append(fixed, r.execute(ctx, runnable)...)), nil
}

// runner holds one capture's resolved configuration.
type runner struct {
	inspect     InspectFunc
	now         func() time.Time
	top         int
	concurrency int
}

func newRunner(in Input) *runner {
	r := &runner{inspect: in.Inspect, now: in.Now, top: in.Top, concurrency: in.Concurrency}
	if r.inspect == nil {
		r.inspect = sqlstats.Inspect
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.top <= 0 {
		r.top = TopN()
	}
	if r.top > MaxTop {
		r.top = MaxTop
	}
	if r.concurrency < 1 {
		// The fan-out width is runctl's number, not this package's: a
		// collector inventing its own would make "why was my target dropped?"
		// unanswerable.
		r.concurrency = runctl.BaselineConcurrency
	}
	return r
}

// candidate is a target that will be connected to.
type candidate struct {
	targetID string
	schema   string
	// digests are the selected statements in the interval's ranking order.
	digests []sqlrows.DigestStat
	// total is the target's whole interval time, used to order the waves so
	// that a budget shortfall drops the least busy targets.
	total uint64
	// window is the freshness window, already known to be usable.
	window freshWindow
}

// plan splits the interval's targets into those that are already decided and
// those worth connecting to.
//
// Everything that can be decided without the database is decided here: an
// unusable interval, an unusable schema, no SELECT digests, and every
// "unknown" freshness verdict. That last one matters for the budget as much as
// for correctness — a target whose clock sqlrows called anomalous can produce
// no fresh plan, so connecting to it would spend a fifth of the enrich budget
// on statements whose results could not be used.
func (r *runner) plan(rows *sqlrows.Section) ([]TargetSection, []candidate) {
	fixed := make([]TargetSection, 0, len(rows.Targets))
	runnable := make([]candidate, 0, len(rows.Targets))
	for _, target := range rows.Targets {
		switch {
		case !target.Usable:
			fixed = append(fixed, skipSection(target.TargetID, target.Schema, CodeNoInterval))
			continue
		case !validSchema(target.Schema):
			fixed = append(fixed, skipSection(target.TargetID, target.Schema, CodeNoSchema))
			continue
		}
		digests := selectDigests(target.Digests, r.top)
		if len(digests) == 0 {
			fixed = append(fixed, skipSection(target.TargetID, target.Schema, CodeNoDigests))
			continue
		}
		window, reason, ok := windowFor(target.DBClock, targetValidity(target))
		if !ok {
			fixed = append(fixed, TargetSection{
				TargetID: target.TargetID,
				Schema:   target.Schema,
				Plans:    undatedPlans(digests, reason),
			})
			continue
		}
		runnable = append(runnable, candidate{
			targetID: target.TargetID,
			schema:   target.Schema,
			digests:  digests,
			total:    intervalTotal(target.Digests),
			window:   window,
		})
	}
	sort.Slice(runnable, func(i, j int) bool {
		if runnable[i].total != runnable[j].total {
			return runnable[i].total > runnable[j].total
		}
		return runnable[i].targetID < runnable[j].targetID
	})
	return fixed, runnable
}

// waveResult is one worker's finished target, addressed by its position in the
// candidate slice.
//
// Results travel by channel rather than being written into the output slice
// directly, because the wait below can end before a worker does. A worker that
// outlives the wait must have nowhere to write: the slice it would have written
// to is by then part of a published section.
type waveResult struct {
	index   int
	section TargetSection
}

// execute runs the candidates in waves of at most concurrency, busiest target
// first.
//
// A wave that cannot fit session establishment and the sample read is not
// started at all, and its targets are recorded as budget-exhausted. Both
// halves of that matter: starting a doomed wave would open connections to the
// measured database for nothing, and dropping the targets silently would make
// an out-of-budget run look like a run with nothing to report.
func (r *runner) execute(ctx context.Context, candidates []candidate) []TargetSection {
	out := make([]TargetSection, len(candidates))
	for start := 0; start < len(candidates); {
		if !r.waveFits(ctx) {
			for i := start; i < len(candidates); i++ {
				out[i] = skipSection(candidates[i].targetID, candidates[i].schema, CodeBudgetExhausted)
			}
			break
		}
		end := min(start+r.concurrency, len(candidates))
		// One slot per worker, so a worker whose result nobody is waiting for
		// any more still completes its send and exits instead of leaking.
		results := make(chan waveResult, end-start)
		for i := start; i < end; i++ {
			go func(i int) {
				// Fail-open: a panic inside enrichment must cost one target's
				// plans, never the measured application.
				defer func() {
					if recovered := recover(); recovered != nil {
						results <- waveResult{index: i,
							section: skipSection(candidates[i].targetID, candidates[i].schema, CodeQueryError)}
					}
				}()
				results <- waveResult{index: i, section: r.runTarget(ctx, candidates[i])}
			}(i)
		}
		collectWave(ctx, results, candidates, out, start, end)
		start = end
	}
	return out
}

// collectWave gathers one wave's results and, crucially, bounds the wait.
//
// sync.WaitGroup.Wait has no bound of its own, and the goroutine calling this
// hook is the run's finish worker. A driver that ignores its context — a
// connection stuck in a syscall, a proxy that accepts and never answers — would
// therefore hold that worker for as long as it liked: past the enrich budget,
// past the finish lease, until the run's own watchdog aborted a run that was
// otherwise healthy. Bounding the wait converts that from a lost run into one
// target's lost plans, which is the trade this whole package is built on.
//
// Everything a worker managed to deliver is kept; every target still
// outstanding is recorded as timed out rather than dropped, because a target
// missing from the section is indistinguishable from a target with no traffic.
func collectWave(ctx context.Context, results <-chan waveResult, candidates []candidate, out []TargetSection, start, end int) {
	bounded, cancel := waveBound(ctx)
	defer cancel()

	arrived := make([]bool, end-start)
	for pending := end - start; pending > 0; pending-- {
		select {
		case result := <-results:
			out[result.index] = result.section
			arrived[result.index-start] = true
		case <-bounded.Done():
			// A worker may have finished in the same instant the deadline
			// fired; what it sent is a real result and is kept.
			drainResults(results, out, arrived, start)
			for i := start; i < end; i++ {
				if !arrived[i-start] {
					out[i] = skipSection(candidates[i].targetID, candidates[i].schema, CodeTargetTimeout)
				}
			}
			return
		}
	}
}

// drainResults takes every result already delivered, without waiting for more.
func drainResults(results <-chan waveResult, out []TargetSection, arrived []bool, start int) {
	for {
		select {
		case result := <-results:
			out[result.index] = result.section
			arrived[result.index-start] = true
		default:
			return
		}
	}
}

// waveBound is the deadline the wait for one wave ends on.
//
// Normally it is the enrich deadline itself, since runctl calls the hook with
// EnrichBudget. A caller that gave no deadline still gets a bound: "wait
// forever" is not an outcome an optional extra may impose on the process it is
// measuring, and one wave is one PerTargetBudget of work by construction.
func waveBound(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, runctl.PerTargetBudget)
}

// waveFits reports whether a whole wave still fits in the enrich budget.
func (r *runner) waveFits(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return deadline.Sub(r.now()) >= startupBudget
}

// runTarget explains one target inside one Inspect call.
//
// One call, not one per digest: the session state established below — the
// neutralised roles, the instrumentation flag, the default database — belongs
// to the pinned connection, and a second Inspect would get a different one
// with none of it.
func (r *runner) runTarget(ctx context.Context, c candidate) TargetSection {
	budget := runctl.PerTargetBudget
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := deadline.Sub(r.now()); remaining < budget {
			budget = remaining
		}
	}
	if budget <= 0 {
		return skipSection(c.targetID, c.schema, CodeBudgetExhausted)
	}
	targetCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	section := skipSection(c.targetID, c.schema, CodeQueryError)
	// PurposeExplain only. There is no fallback to the application or stats
	// credential, by design: an implicit downgrade to a credential holding DML
	// is exactly the thing the dedicated user exists to prevent.
	err := r.inspect(targetCtx, c.targetID, sqlstats.PurposeExplain, func(ctx context.Context, q sqlstats.Querier) error {
		section = r.explainTarget(ctx, q, c)
		return nil
	})
	if err != nil {
		return skipSection(c.targetID, c.schema, inspectCode(err))
	}
	return section
}

// explainTarget runs the whole sequence on the pinned connection.
func (r *runner) explainTarget(ctx context.Context, q sqlstats.Querier, c candidate) TargetSection {
	sessionCtx, cancel := context.WithTimeout(ctx, SessionBudget)
	sess := establish(sessionCtx, q, c.schema)
	cancel()
	if sess.code != "" {
		return skipSection(c.targetID, c.schema, sess.code)
	}

	sampleCtx, cancel := context.WithTimeout(ctx, SampleBudget)
	samples, err := fetchSamples(sampleCtx, q, c.schema, digestKeys(c.digests))
	cancel()
	if err != nil {
		return skipSection(c.targetID, c.schema, CodeQueryError)
	}

	return TargetSection{
		TargetID:  c.targetID,
		Schema:    c.schema,
		Explained: true,
		Plans:     r.explainDigests(ctx, q, c, sess, samples),
	}
}

// inspectCode maps an Inspect failure onto a reason ID. The error itself is
// dropped here: registry errors carry a driver message, and a health note is a
// published string.
func inspectCode(err error) string {
	switch {
	case errors.Is(err, sqlstats.ErrPurposeNotRegistered):
		return CodePurposeUnregistered
	case errors.Is(err, sqlstats.ErrUnknownTarget):
		return CodeUnknownTarget
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return CodeBudgetExhausted
	default:
		return CodeQueryError
	}
}

// assemble orders the targets and derives the health notes from their reason
// IDs.
func (r *runner) assemble(sections []TargetSection) *Section {
	sort.Slice(sections, func(i, j int) bool { return sections[i].TargetID < sections[j].TargetID })
	notes := newNoteSet()
	for _, section := range sections {
		switch {
		case section.Code != "":
			if !quietCodes[section.Code] {
				notes.add(section.Code, section.TargetID)
			}
		case lostDefaultDatabase(section.Plans):
			// Every plan of this target failed for a reason that is this
			// package's own fault, so it belongs in health next to the
			// configuration problems rather than only on the rows.
			notes.add(CodeNoDefaultDatabase, section.TargetID)
		}
	}
	return &Section{Targets: sections, Health: notes.notes(), Top: r.top}
}

// lostDefaultDatabase reports whether the server answered with 1046, which
// means the USE step did not take effect on the connection the EXPLAIN ran on.
func lostDefaultDatabase(plans []Plan) bool {
	for _, plan := range plans {
		if plan.Err != nil && plan.Err.Errno == errnoNoDatabase {
			return true
		}
	}
	return false
}

// skipSection builds a target that produced no plans. Reason always comes from
// the fixed table, so no driver text can reach it.
func skipSection(targetID, schema, code string) TargetSection {
	return TargetSection{TargetID: targetID, Schema: schema, Code: code, Reason: reasons[code]}
}

// undatedPlans records the selected digests of a target whose freshness could
// not be judged. They are kept rather than dropped so the dashboard can show
// why a statement has no plan, and they carry no sample time because none was
// read: with an untrustworthy window there is nothing to compare one against,
// and EXPLAIN is not run at all.
func undatedPlans(stats []sqlrows.DigestStat, reason FreshReason) []Plan {
	out := make([]Plan, 0, len(stats))
	for _, stat := range stats {
		out = append(out, Plan{
			Digest:      stat.Digest,
			Query:       stat.Query,
			Freshness:   FreshnessUnknown,
			FreshReason: reason,
		})
	}
	return out
}

// intervalTotal sums a target's interval time, which is how targets are
// ordered when the budget cannot hold all of them.
func intervalTotal(stats []sqlrows.DigestStat) uint64 {
	var total uint64
	for _, stat := range stats {
		total += stat.TimerWaitPicos
	}
	return total
}
