package runctl

import (
	"context"
	"fmt"
)

// startDecision is what planStartLocked concluded about a StartRun request.
type startDecision int

const (
	decisionProceed startDecision = iota
	decisionReplay
	decisionWait
	decisionFail
)

// startPlan is the outcome of evaluating a StartRun against the current state.
type startPlan struct {
	decision startDecision
	replay   StartResult
	wait     chan struct{}
	err      error
	// preempt is the in-flight run to abort before starting.
	preempt *runSlot
	// ackFinished is a finished run to implicitly acknowledge. Its snapshot is
	// kept: a completed measurement is real data, and preemption only targets
	// runs that are still in flight.
	ackFinished *runSlot
}

// planStartLocked evaluates a StartRun request against the current state.
// Callers must hold mu.
func (c *Controller) planStartLocked(o StartRunOptions) startPlan {
	if s := c.slotByNonceLocked(o.Nonce); s != nil {
		switch s.state {
		case StateStarting:
			return startPlan{decision: decisionWait, wait: s.changed}
		case StateStarted, StateFinishing, StateFinished, StateAcknowledged:
			return startPlan{decision: decisionReplay, replay: s.start}
		case StateAborting:
			return startPlan{decision: decisionFail, err: ErrRunTransitioning}
		case StateAborted:
			return startPlan{decision: decisionFail, err: ErrRunAborted}
		case StateExpired:
			return startPlan{decision: decisionFail, err: ErrUnknownRun}
		}
	}
	if r, ok := c.lookupNonceLocked(o.Nonce); ok {
		return startPlan{decision: decisionReplay, replay: r}
	}

	cur := c.currentLocked()
	if cur == nil {
		return startPlan{decision: decisionProceed}
	}
	switch cur.state {
	case StateStarting, StateStarted, StateFinishing:
		if !o.Preempt {
			return startPlan{decision: decisionFail, err: ErrRunActive}
		}
		return startPlan{decision: decisionProceed, preempt: cur}
	case StateAborting:
		if !o.Preempt {
			return startPlan{decision: decisionFail, err: ErrRunActive}
		}
		// Someone else is already tearing this run down; wait for them rather
		// than racing a second abort against the same worker.
		return startPlan{decision: decisionWait, wait: cur.changed}
	case StateFinished:
		if !o.Preempt {
			return startPlan{decision: decisionFail, err: ErrRunActive}
		}
		return startPlan{decision: decisionProceed, ackFinished: cur}
	}
	return startPlan{decision: decisionProceed}
}

// StartRun opens a measurement run: it switches every generation collector to
// a fresh generation and samples every baseline collector, then returns an
// immutable record of that boundary.
//
// A collector failure is not an error return. It downgrades StartResult
// Validity instead, because the boundary itself did happen and the caller
// needs the record. Callers must inspect Validity. An error return means the
// Controller refused (ErrRunActive) or could not act at all.
func (c *Controller) StartRun(ctx context.Context, o StartRunOptions) (StartResult, error) {
	for {
		c.mu.Lock()
		plan := c.planStartLocked(o)
		c.mu.Unlock()

		switch plan.decision {
		case decisionReplay:
			return plan.replay.clone(), nil
		case decisionFail:
			return StartResult{}, plan.err
		case decisionWait:
			select {
			case <-plan.wait:
				continue
			case <-ctx.Done():
				return StartResult{}, fmt.Errorf("runctl: waiting to start a run: %w", ctx.Err())
			}
		}

		// Re-evaluate under startMu so that "abort the active run, then take my
		// boundary" cannot be interleaved with another starter.
		c.startMu.Lock()
		c.mu.Lock()
		plan = c.planStartLocked(o)
		c.mu.Unlock()
		if plan.decision != decisionProceed {
			c.startMu.Unlock()
			continue
		}
		res, err := c.beginRun(ctx, o, plan)
		c.startMu.Unlock()
		return res, err
	}
}

// beginRun performs the opening boundary. startMu is held by the caller.
func (c *Controller) beginRun(ctx context.Context, o StartRunOptions, plan startPlan) (StartResult, error) {
	runID := newID("run")
	nonce := o.Nonce
	if nonce == "" {
		nonce = newID("nonce")
	}

	preemptedID := ""
	if plan.preempt != nil {
		preemptedID = plan.preempt.runID
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PreemptTotalBudget)
		_, err := c.AbortRun(abortCtx, preemptedID, ReasonPreemptedBy+runID)
		cancel()
		if err != nil {
			return StartResult{}, fmt.Errorf("runctl: preempting run %s: %w", preemptedID, err)
		}
	}
	if plan.ackFinished != nil {
		// A finished snapshot is valid data; record who displaced it instead of
		// discarding it.
		_ = c.AckBy(plan.ackFinished.runID, AckedByPreempt)
	}

	// The caller's cancellation is deliberately dropped: a client hanging up
	// mid-reset must not leave half the collectors switched.
	bg, cancel := context.WithCancel(context.WithoutCancel(ctx))

	c.mu.Lock()
	c.epoch++
	ep := c.epoch
	slot := &runSlot{
		runID:    runID,
		nonce:    nonce,
		epoch:    ep,
		state:    StateStarting,
		validity: ValidityValid,
		trigger:  o.Trigger,
		since:    c.now(),
		bg:       bg,
		cancel:   cancel,
		owners:   1, // this goroutine
		done:     make(chan struct{}),
		changed:  make(chan struct{}),
	}
	c.addSlotLocked(slot)
	c.mu.Unlock()

	gens, bases := c.registrations()
	syncCtx, syncCancel := context.WithTimeout(bg, c.budgets.StartRun)
	defer syncCancel()

	genOut := c.runGenerationPhase(syncCtx, PhaseStartBoundary, runID, ep, gens, c.budgets.PhaseBoundary)
	baseOut := c.runBaselinePhase(syncCtx, PhaseStartBaseline, runID, ep, bases, c.budgets.PhaseBaseline)

	boundaries := make([]CollectorBoundary, 0, len(genOut.boundaries)+len(baseOut.boundaries))
	boundaries = append(boundaries, genOut.boundaries...)
	boundaries = append(boundaries, baseOut.boundaries...)

	validity := worse(genOut.validity, baseOut.validity)
	validity = worse(validity, c.applySpread(runID, boundaries, "start"))

	res := StartResult{
		RunID:            runID,
		Nonce:            nonce,
		Epoch:            ep,
		State:            StateStarted,
		Validity:         validity,
		Collectors:       boundaries,
		GenerationWindow: computeWindow(boundaries, keepGeneration),
		BoundaryWindow:   computeWindow(boundaries, nil),
		PreemptedRunID:   preemptedID,
		StartedAt:        c.now(),
	}
	// A required failure at the opening boundary means the run never had a
	// trustworthy beginning, so it is recorded and abandoned rather than
	// measured. The closing boundary is treated differently: there the data is
	// already real and worth keeping even when invalid.
	if validity == ValidityInvalid {
		res.State = StateAborted
	}

	err := c.commit(ep, func(s *runSlot) {
		s.start = res
		s.validity = validity
		s.gen = genOut.handles
		s.base = baseOut.handles
		s.since = c.now()
		s.state = res.State
		if res.State == StateAborted {
			s.reason = ReasonRequiredFailed
			s.abort = AbortResult{
				RunID:     runID,
				Epoch:     ep,
				Reason:    ReasonRequiredFailed,
				AbortedAt: s.since,
				Partial:   committedNames(boundaries),
			}
			c.beginOwnerLocked(s)
			go c.discardWorker(s, ep, bg, genOut.handles, baseOut.handles)
			c.sealLocked(s)
			return
		}
		c.rememberNonceLocked(nonce, res)
		if len(genOut.handles) > 0 {
			c.beginOwnerLocked(s)
			go c.startDrainWorker(s, ep, bg, genOut.handles)
		}
	})
	if err != nil {
		// The run was fenced while its boundary was being taken. Nothing was
		// committed, so nothing else will release these handles. Release
		// before retiring ownership, so whoever is joining this run observes a
		// fully cleaned-up slot.
		c.releaseGeneration(genOut.handles)
		c.releaseBaseline(baseOut.handles)
		c.endOwner(slot)
		return StartResult{}, fmt.Errorf("runctl: run %s was preempted while starting: %w", runID, ErrRunAborted)
	}
	c.endOwner(slot)
	return res.clone(), nil
}

// committedNames lists the collectors that had already switched when a run
// died, so an operator can tell how far the boundary got.
func committedNames(boundaries []CollectorBoundary) []string {
	var names []string
	for _, b := range boundaries {
		if b.Committed {
			names = append(names, b.Name)
		}
	}
	return names
}

// startDrainWorker settles and releases the generations that the opening
// boundary closed. Their data belongs to whatever came before this run, so it
// is discarded; draining is still required so no goroutine keeps writing to a
// generation the collector has already handed over.
func (c *Controller) startDrainWorker(s *runSlot, ep Epoch, bg context.Context, handles []GenerationHandle) {
	defer c.endOwner(s)

	ctx, cancel := context.WithTimeout(bg, c.budgets.Drain)
	defer cancel()
	c.drainAndRelease(ctx, handles)

	// Ignore the fence result: if the run was aborted meanwhile, the abort has
	// already taken ownership of the bookkeeping.
	_ = c.commit(ep, func(slot *runSlot) { slot.gen = nil })
}

// discardWorker drains and releases everything a failed opening boundary
// switched, then retires the run's context.
func (c *Controller) discardWorker(s *runSlot, ep Epoch, bg context.Context, gen []GenerationHandle, base []BaselineHandle) {
	defer c.endOwner(s)
	defer func() {
		if s.cancel != nil {
			s.cancel()
		}
	}()

	ctx, cancel := context.WithTimeout(bg, c.budgets.Drain)
	defer cancel()
	c.drainAndRelease(ctx, gen)
	c.releaseBaseline(base)

	// The handles are gone, so the tombstone must stop referring to them: an
	// abort arriving later would otherwise release them a second time and the
	// record would pin them until its TTL. Ignore the fence result — a run
	// aborted meanwhile has already taken over this bookkeeping.
	_ = c.commit(ep, func(slot *runSlot) { slot.gen, slot.base = nil, nil })
}

// drainAndRelease settles then frees generation handles.
func (c *Controller) drainAndRelease(ctx context.Context, handles []GenerationHandle) {
	for _, h := range handles {
		rc, ok := c.generationByName(h.Collector)
		if !ok {
			continue
		}
		// A drain failure here only means in-flight work outlived the budget;
		// the handle still has to be released.
		c.safeInvoke(rc.reg.Name, "Drain", func() error { return rc.coll.Drain(ctx, h) })
		c.safeInvoke(rc.reg.Name, "Release", func() error { rc.coll.Release(h); return nil })
	}
}

// generationByName resolves a handle's collector.
func (c *Controller) generationByName(name string) (registeredGeneration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rc := range c.gens {
		if rc.reg.Name == name {
			return rc, true
		}
	}
	return registeredGeneration{}, false
}

// baselineByName resolves a handle's collector.
func (c *Controller) baselineByName(name string) (registeredBaseline, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rc := range c.bases {
		if rc.reg.Name == name {
			return rc, true
		}
	}
	return registeredBaseline{}, false
}

// releaseGeneration frees generation handles. Release is idempotent by
// contract, so overlapping owners are harmless. A collector that panics on
// release is recorded and skipped rather than allowed to strand the handles of
// every collector after it.
func (c *Controller) releaseGeneration(handles []GenerationHandle) {
	for _, h := range handles {
		if rc, ok := c.generationByName(h.Collector); ok {
			c.safeInvoke(rc.reg.Name, "Release", func() error { rc.coll.Release(h); return nil })
		}
	}
}

// releaseBaseline frees baseline handles.
func (c *Controller) releaseBaseline(handles []BaselineHandle) {
	for _, h := range handles {
		if rc, ok := c.baselineByName(h.Collector); ok {
			c.safeInvoke(rc.reg.Name, "Release", func() error { rc.coll.Release(h); return nil })
		}
	}
}
