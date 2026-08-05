package runctl

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekusiadadus/isutools/internal/health"
)

// HealthRecorder is the sink for the four runctl-* health keys. It is the
// narrow subset of internal/health.Registry this package needs, so a Controller
// can be built in tests without a registry and so measurement never depends on
// health reporting succeeding.
type HealthRecorder interface {
	Set(collector string, status health.Status, message string)
}

// Options configures a Controller. The zero value is usable: every field falls
// back to a package default.
type Options struct {
	// Budgets overrides the timing table. Zero fields keep their constants.
	Budgets Budgets
	// Now overrides the clock. Tests inject a controllable clock so leases and
	// TTLs can be exercised without waiting them out.
	Now func() time.Time
	// Health receives the runctl-* keys. Nil disables health reporting.
	Health HealthRecorder
	// Enrich runs after Collect and before the snapshot is published, bounded
	// by the enrich budget. It is how post-freeze extras such as EXPLAIN
	// output are attached without widening the freeze boundary.
	Enrich func(ctx context.Context, s *Snapshot) error
	// DisableWatchdog stops the Controller from starting its lease/TTL sweeper
	// goroutine. Tests that drive Sweep by hand set this.
	DisableWatchdog bool
}

// runSlot is one run's mutable record. Every field is guarded by
// Controller.mu; workers never write it directly but go through commit, which
// applies the epoch fence first.
type runSlot struct {
	runID    string
	nonce    string
	epoch    Epoch
	state    RunState
	validity Validity
	reason   string
	trigger  string
	ackedBy  string
	detached bool
	since    time.Time

	// bg is the run's detached context: it carries the caller's values but
	// none of its cancellation, so a disconnecting client cannot leave a
	// boundary half-taken. cancel cuts every operation belonging to the run.
	bg     context.Context
	cancel context.CancelFunc

	// lease is the deadline a finishing worker must beat.
	lease time.Time

	// owners counts live goroutines working for this run; done is closed once
	// the slot is sealed and the last of them has left.
	owners   int
	sealed   bool
	done     chan struct{}
	doneOnce sync.Once

	// changed is closed and replaced on every state change so waiters
	// (Await, same-nonce StartRun, concurrent AbortRun) can block without
	// polling.
	changed chan struct{}

	// gen holds the previous generations closed at the opening boundary;
	// genFinal holds the generations frozen at the closing boundary. Only the
	// latter carries the run's data.
	gen      []GenerationHandle
	genFinal []GenerationHandle
	base     []BaselineHandle
	baseF    []BaselineHandle

	start     StartResult
	finish    FinishAccepted
	finishSet bool
	// abort is written together with StateAborted and read back by a later
	// AbortRun, so the record needs no "is it set" flag of its own: the state
	// is the flag. finishSet exists because StateFinishing is published before
	// the acceptance record is, and a second FinishRun has to wait for it.
	abort    AbortResult
	snapshot *Snapshot
}

// closeDoneLocked closes the join channel exactly once.
func (s *runSlot) closeDoneLocked() {
	s.doneOnce.Do(func() { close(s.done) })
}

// nonceEntry is one cached StartResult keyed by nonce.
type nonceEntry struct {
	nonce  string
	result StartResult
	at     time.Time
}

// Controller owns the run lifecycle for one process.
type Controller struct {
	budgets Budgets
	now     func() time.Time
	healthR HealthRecorder
	enrich  func(context.Context, *Snapshot) error

	mu    sync.Mutex
	epoch Epoch
	slots []*runSlot // oldest first, at most RetainedRuns entries
	gens  []registeredGeneration
	bases []registeredBaseline
	nonce []nonceEntry

	// startMu serializes whole StartRun bodies so that "abort the active run,
	// then start mine" is atomic against other starters without holding mu
	// across a join.
	startMu sync.Mutex

	stale     atomic.Uint64
	published atomic.Uint64

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

// New builds a Controller. It fails only on an inverted budget table, which is
// a configuration bug worth refusing at startup rather than debugging later
// from truncated measurements.
func New(o Options) (*Controller, error) {
	b := o.Budgets.withDefaults()
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("runctl: invalid budgets: %w", err)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	c := &Controller{
		budgets: b,
		now:     now,
		healthR: o.Health,
		enrich:  o.Enrich,
		stop:    make(chan struct{}),
	}
	if !o.DisableWatchdog {
		c.wg.Add(1)
		go c.watchdog()
	}
	return c, nil
}

var (
	defaultOnce sync.Once
	defaultCtrl *Controller
)

// Default returns the process-wide Controller. The lifecycle is a property of
// the process being measured, so there is exactly one of these.
func Default() *Controller {
	defaultOnce.Do(func() {
		c, err := New(Options{})
		if err != nil {
			// The default budget table is a compile-time constant set; it
			// cannot be inverted. Fall back to an unvalidated Controller
			// rather than panicking in a library path.
			c = &Controller{budgets: Budgets{}.withDefaults(), now: time.Now, stop: make(chan struct{})}
		}
		defaultCtrl = c
	})
	return defaultCtrl
}

// Close stops the watchdog. It is idempotent.
func (c *Controller) Close() {
	c.stopOnce.Do(func() { close(c.stop) })
	c.wg.Wait()
}

// RegisterGeneration adds a generation collector.
func (c *Controller) RegisterGeneration(r Registration, g GenerationCollector) error {
	if r.Name == "" || g == nil {
		return fmt.Errorf("%w: generation collector needs a name and an implementation", ErrInvalidRegistration)
	}
	if err := checkBudget(r.Name, g, c.budgets.PerCollectorGeneration, "PerCollectorGeneration"); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.gens {
		if existing.reg.Name == r.Name {
			return fmt.Errorf("%w: %s", ErrCollectorRegistered, r.Name)
		}
	}
	c.gens = append(c.gens, registeredGeneration{reg: r, coll: g})
	return nil
}

// RegisterBaseline adds a baseline collector.
func (c *Controller) RegisterBaseline(r Registration, b BaselineCollector) error {
	if r.Name == "" || b == nil {
		return fmt.Errorf("%w: baseline collector needs a name and an implementation", ErrInvalidRegistration)
	}
	if err := checkBudget(r.Name, b, c.budgets.PerCollectorBaseline, "PerCollectorBaseline"); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.bases {
		if existing.reg.Name == r.Name {
			return fmt.Errorf("%w: %s", ErrCollectorRegistered, r.Name)
		}
	}
	c.bases = append(c.bases, registeredBaseline{reg: r, coll: b})
	return nil
}

// checkBudget rejects a collector asking for more time than its parent allows.
// Budget is a collector call like any other, so it runs behind the panic
// barrier: registration happens inside the measured application's startup, and
// a panicking collector must fail its own registration rather than the
// process.
func checkBudget(name string, coll any, parent time.Duration, parentName string) error {
	aware, ok := coll.(BudgetAware)
	if !ok {
		return nil
	}
	want, err := safeResult(name, "Budget", func() (time.Duration, error) { return aware.Budget(), nil })
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidRegistration, err)
	}
	if want > parent {
		return fmt.Errorf("%w: collector wants %v, %s is %v", ErrBudgetInversion, want, parentName, parent)
	}
	return nil
}

// registrations returns copies of the collector tables so a phase can run
// without holding the mutex.
func (c *Controller) registrations() ([]registeredGeneration, []registeredBaseline) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]registeredGeneration(nil), c.gens...), append([]registeredBaseline(nil), c.bases...)
}

// StaleRejections reports how many commit and publish attempts the epoch fence
// turned away. Tests assert it is non-zero to prove the fence actually ran
// rather than the race merely not happening.
func (c *Controller) StaleRejections() uint64 { return c.stale.Load() }

// PublishedSnapshots reports how many snapshots were accepted.
func (c *Controller) PublishedSnapshots() uint64 { return c.published.Load() }

// commit is the only path by which a worker changes run state. Mutations from
// a run that is no longer current are rejected, which is what makes an aborted
// run's worker harmless even when it could not be joined.
func (c *Controller) commit(ep Epoch, mutate func(*runSlot)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.fencedSlotLocked(ep)
	if s == nil {
		c.stale.Add(1)
		return ErrStaleEpoch
	}
	mutate(s)
	c.notifyLocked(s)
	return nil
}

// publish stores a run's snapshot behind the same fence as commit.
func (c *Controller) publish(ep Epoch, snap *Snapshot) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.fencedSlotLocked(ep)
	if s == nil {
		c.stale.Add(1)
		return ErrStaleEpoch
	}
	s.snapshot = snap
	c.published.Add(1)
	return nil
}

// fencedSlotLocked returns the slot owning ep, but only while ep is still the
// Controller's current epoch.
func (c *Controller) fencedSlotLocked(ep Epoch) *runSlot {
	if ep != c.epoch {
		return nil
	}
	for _, s := range c.slots {
		if s.epoch == ep {
			return s
		}
	}
	return nil
}

// notifyLocked wakes everyone waiting on this slot's state.
func (c *Controller) notifyLocked(s *runSlot) {
	close(s.changed)
	s.changed = make(chan struct{})
}

// beginOwnerLocked registers another goroutine working for the run.
func (c *Controller) beginOwnerLocked(s *runSlot) { s.owners++ }

// endOwner retires a worker goroutine and closes the join channel once the
// slot is sealed and nobody is left.
func (c *Controller) endOwner(s *runSlot) {
	c.mu.Lock()
	s.owners--
	if s.owners <= 0 && s.sealed {
		s.closeDoneLocked()
	}
	c.mu.Unlock()
}

// sealLocked marks the slot as accepting no further workers.
func (c *Controller) sealLocked(s *runSlot) {
	s.sealed = true
	if s.owners <= 0 {
		s.closeDoneLocked()
	}
}

// lookupLocked finds a retained run by ID.
func (c *Controller) lookupLocked(runID string) *runSlot {
	for _, s := range c.slots {
		if s.runID == runID {
			return s
		}
	}
	return nil
}

// currentLocked returns the newest retained run, or nil when none is retained.
func (c *Controller) currentLocked() *runSlot {
	if len(c.slots) == 0 {
		return nil
	}
	return c.slots[len(c.slots)-1]
}

// slotByNonceLocked finds a retained run started with this nonce.
func (c *Controller) slotByNonceLocked(nonce string) *runSlot {
	if nonce == "" {
		return nil
	}
	for _, s := range c.slots {
		if s.nonce == nonce {
			return s
		}
	}
	return nil
}

// addSlotLocked appends a run record and enforces RetainedRuns. The retained
// count includes the in-flight run, so exactly one historical record survives
// alongside a live one; anything older is dropped immediately rather than
// lingering until its tombstone TTL, which keeps memory flat under rapid
// start/abort cycles.
func (c *Controller) addSlotLocked(s *runSlot) {
	c.slots = append(c.slots, s)
	for len(c.slots) > RetainedRuns {
		idx := -1
		for i, cand := range c.slots {
			if !cand.state.active() {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		victim := c.slots[idx]
		c.slots = append(c.slots[:idx], c.slots[idx+1:]...)
		victim.snapshot = nil
		if victim.cancel != nil {
			victim.cancel()
		}
	}
}

// rememberNonceLocked caches a successful StartResult for replay. Only
// successful starts are cached: replaying a start that ended in an abort would
// hand a caller a run ID that can never produce data.
func (c *Controller) rememberNonceLocked(nonce string, r StartResult) {
	if nonce == "" || r.State != StateStarted {
		return
	}
	c.nonce = append(c.nonce, nonceEntry{nonce: nonce, result: r, at: c.now()})
	if len(c.nonce) > NonceHistoryMax {
		c.nonce = c.nonce[len(c.nonce)-NonceHistoryMax:]
	}
}

// lookupNonceLocked returns a cached StartResult that has not aged out.
func (c *Controller) lookupNonceLocked(nonce string) (StartResult, bool) {
	if nonce == "" {
		return StartResult{}, false
	}
	cutoff := c.now().Add(-c.budgets.NonceTTL)
	for i := len(c.nonce) - 1; i >= 0; i-- {
		e := c.nonce[i]
		if e.nonce != nonce {
			continue
		}
		if e.at.Before(cutoff) {
			return StartResult{}, false
		}
		return e.result, true
	}
	return StartResult{}, false
}

// recordHealth reports a runctl condition. Health reporting is best effort:
// a missing registry must never change measurement behaviour.
func (c *Controller) recordHealth(key, message string) {
	if c.healthR == nil {
		return
	}
	c.healthR.Set(key, health.StatusDegraded, message)
}

// Status returns a run's current state. The bool is false for runs the
// Controller does not retain.
func (c *Controller) Status(runID string) (RunStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.lookupLocked(runID)
	if s == nil {
		return RunStatus{}, false
	}
	return statusOf(s), true
}

// statusOf projects a slot into its queryable form. Callers must hold mu.
func statusOf(s *runSlot) RunStatus {
	return RunStatus{
		RunID:    s.runID,
		Epoch:    s.epoch,
		State:    s.state,
		Validity: s.validity,
		Reason:   s.reason,
		AckedBy:  s.ackedBy,
		Detached: s.detached,
		Since:    s.since,
	}
}

// SnapshotOf returns a finished run's immutable snapshot.
func (c *Controller) SnapshotOf(runID string) (*Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.lookupLocked(runID)
	if s == nil || s.state == StateExpired {
		return nil, ErrUnknownRun
	}
	if s.state == StateAborted || s.state == StateAborting {
		return nil, ErrRunAborted
	}
	if s.snapshot == nil {
		return nil, ErrRunActive
	}
	return s.snapshot, nil
}

// Await blocks until the run reaches a state that answers the caller's
// question: a starting run until it has started, a finishing run until its
// snapshot exists or the run died.
func (c *Controller) Await(ctx context.Context, runID string) (RunStatus, error) {
	for {
		c.mu.Lock()
		s := c.lookupLocked(runID)
		if s == nil || s.state == StateExpired {
			c.mu.Unlock()
			return RunStatus{}, ErrUnknownRun
		}
		switch s.state {
		case StateStarted, StateFinished, StateAcknowledged, StateAborted:
			st := statusOf(s)
			c.mu.Unlock()
			return st, nil
		}
		changed := s.changed
		c.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return RunStatus{}, fmt.Errorf("runctl: awaiting run %s: %w", runID, ctx.Err())
		}
	}
}

// Ack marks a finished run's snapshot as handed over.
func (c *Controller) Ack(runID string) error { return c.AckBy(runID, AckedByExplicit) }

// AckBy is Ack with an explicit provenance. The provenance matters because a
// self-acknowledgement driven by an expired lease is not the same evidence as
// an operator collecting a result, and multi-host debugging depends on telling
// them apart.
func (c *Controller) AckBy(runID, by string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.lookupLocked(runID)
	if s == nil {
		if cur := c.currentLocked(); cur != nil && cur.state.blocksNewRun() {
			return ErrRunActive
		}
		return ErrUnknownRun
	}
	switch s.state {
	case StateStarting, StateStarted, StateFinishing:
		return ErrRunActive
	case StateAborting, StateAborted:
		return ErrRunAborted
	case StateExpired:
		return ErrUnknownRun
	case StateAcknowledged:
		return nil
	}
	s.state = StateAcknowledged
	s.ackedBy = by
	s.since = c.now()
	c.notifyLocked(s)
	return nil
}

// watchdog enforces leases and TTLs.
func (c *Controller) watchdog() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.budgets.Watchdog)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			c.Sweep()
		}
	}
}

// Sweep applies lease and TTL expiry once. It is exported so tests can drive
// expiry from an injected clock instead of waiting out a twenty second lease.
func (c *Controller) Sweep() {
	now := c.now()

	type abortReq struct {
		runID  string
		reason string
		health string
	}
	var aborts []abortReq

	c.mu.Lock()
	kept := c.slots[:0]
	for _, s := range c.slots {
		switch s.state {
		case StateFinishing:
			if !s.lease.IsZero() && now.After(s.lease) {
				aborts = append(aborts, abortReq{s.runID, ReasonFinishLeaseExpired, HealthLeaseExpired})
			}
		case StateStarted:
			if now.Sub(s.since) > c.budgets.StartedTTL {
				aborts = append(aborts, abortReq{s.runID, ReasonStartedTTL, ""})
			}
		case StateFinished, StateAcknowledged:
			if now.Sub(s.since) > c.budgets.FinishedTTL {
				s.state = StateExpired
				s.snapshot = nil
				s.since = now
				c.notifyLocked(s)
			}
		case StateAborted, StateExpired:
			if now.Sub(s.since) > c.budgets.TombstoneTTL {
				if s.cancel != nil {
					s.cancel()
				}
				continue
			}
		}
		kept = append(kept, s)
	}
	for i := len(kept); i < len(c.slots); i++ {
		c.slots[i] = nil
	}
	c.slots = kept
	c.mu.Unlock()

	// Aborting takes a join, so it must happen with the mutex released.
	for _, a := range aborts {
		if a.health != "" {
			c.recordHealth(a.health, fmt.Sprintf("run %s exceeded its finish lease and was aborted", a.runID))
		}
		//nolint:errcheck // AbortRun is idempotent and never fails for a known run.
		_, _ = c.AbortRun(context.Background(), a.runID, a.reason)
	}
}
