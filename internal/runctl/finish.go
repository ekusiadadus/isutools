package runctl

import (
	"context"
	"fmt"
	"time"
)

// finishWork is everything the background worker needs. It is passed by value
// so the worker never reads the mutable slot without the fence.
type finishWork struct {
	runID      string
	trigger    string
	startedAt  time.Time
	validity   Validity
	boundaries []CollectorBoundary
	genFinal   []GenerationHandle
	baseStart  []BaselineHandle
	baseFinal  []BaselineHandle
	genStart   []GenerationHandle
}

// FinishRun fixes a run's closing boundary and returns as soon as that
// boundary exists. Draining, collecting and snapshot building continue in the
// background, because the caller is usually a benchmark driver that must be
// released the instant measurement has stopped — making it wait for the
// snapshot would put snapshot-building time inside the measured window of
// whatever runs next.
func (c *Controller) FinishRun(ctx context.Context, runID string) (FinishAccepted, error) {
	for {
		c.mu.Lock()
		s := c.lookupLocked(runID)
		if s == nil {
			cur := c.currentLocked()
			blocked := cur != nil && cur.state.blocksNewRun()
			c.mu.Unlock()
			if blocked {
				return FinishAccepted{}, ErrRunActive
			}
			return FinishAccepted{}, ErrUnknownRun
		}

		switch s.state {
		case StateStarting:
			c.mu.Unlock()
			return FinishAccepted{}, ErrRunTransitioning
		case StateAborting, StateAborted:
			c.mu.Unlock()
			return FinishAccepted{}, ErrRunAborted
		case StateExpired:
			c.mu.Unlock()
			return FinishAccepted{}, ErrUnknownRun
		case StateFinishing, StateFinished, StateAcknowledged:
			if s.finishSet {
				accepted := s.finish
				c.mu.Unlock()
				return accepted.clone(), nil
			}
			// Another caller marked the run finishing but has not published the
			// acceptance record yet; wait for it so retries are idempotent.
			changed := s.changed
			c.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return FinishAccepted{}, fmt.Errorf("runctl: waiting for the finish of run %s: %w", runID, ctx.Err())
			}
		}

		now := c.now()
		s.state = StateFinishing
		s.since = now
		s.lease = now.Add(c.budgets.FinishLease)
		ep, bg, slot := s.epoch, s.bg, s
		// The freeze phases run on this goroutine, outside the mutex. Claiming
		// ownership before releasing the mutex is what stops a concurrent abort
		// from finding an ownerless slot and reporting a join it never
		// performed while a collector is still inside Freeze or CaptureFinal.
		c.beginOwnerLocked(s)
		c.notifyLocked(s)
		c.mu.Unlock()

		return c.freeze(slot, ep, bg)
	}
}

// freeze runs the closing boundary phases and hands the rest to a worker.
//
// FinishRun registered this goroutine as an owner of the slot before it
// released the mutex; that registration is retired here. The background worker
// takes an ownership of its own inside the fenced mutation below, so the two
// overlap by design: the count never drops to zero between the synchronous
// freeze finishing and the worker starting, and it is not left inflated once
// this goroutine returns.
func (c *Controller) freeze(s *runSlot, ep Epoch, bg context.Context) (FinishAccepted, error) {
	defer c.endOwner(s)

	gens, bases := c.registrations()

	syncCtx, cancel := context.WithTimeout(bg, c.budgets.FinishSync)
	defer cancel()

	genOut := c.runGenerationPhase(syncCtx, PhaseFinishFreeze, s.runID, ep, gens, c.budgets.PhaseFreeze)
	baseOut := c.runBaselinePhase(syncCtx, PhaseFinishFinal, s.runID, ep, bases, c.budgets.PhaseFinal)

	boundaries := make([]CollectorBoundary, 0, len(genOut.boundaries)+len(baseOut.boundaries))
	boundaries = append(boundaries, genOut.boundaries...)
	boundaries = append(boundaries, baseOut.boundaries...)

	validity := worse(genOut.validity, baseOut.validity)
	validity = worse(validity, c.applySpread(s.runID, boundaries, "finish"))

	accepted := FinishAccepted{
		RunID:            s.runID,
		Epoch:            ep,
		Validity:         validity,
		Collectors:       boundaries,
		GenerationWindow: computeWindow(boundaries, keepGeneration),
		BoundaryWindow:   computeWindow(boundaries, nil),
		AcceptedAt:       c.now(),
	}

	err := c.commit(ep, func(slot *runSlot) {
		slot.finish = accepted
		slot.finishSet = true
		slot.validity = worse(slot.validity, validity)
		slot.genFinal = genOut.handles
		slot.baseF = baseOut.handles

		work := finishWork{
			runID:      slot.runID,
			trigger:    slot.trigger,
			startedAt:  slot.start.StartedAt,
			validity:   slot.validity,
			boundaries: append(cloneBoundaries(slot.start.Collectors), boundaries...),
			genFinal:   genOut.handles,
			baseStart:  slot.base,
			baseFinal:  baseOut.handles,
			genStart:   slot.gen,
		}
		// Re-arm the lease as the worker takes over. FinishRun armed it before
		// the synchronous freeze, which is what bounds that freeze; leaving it
		// there would charge the worker for whatever the freeze already spent
		// and let the watchdog abort a worker that is inside its own budget —
		// while reporting "finish-lease-expired", a claim about the worker. One
		// lease per phase keeps the reason true.
		slot.lease = c.now().Add(c.budgets.FinishLease)
		// Registering the worker inside the fenced mutation, while this
		// goroutine's own ownership is still held, means an abort can never
		// observe zero owners between the freeze and the worker and conclude
		// the run is idle.
		c.beginOwnerLocked(slot)
		go c.finishWorker(slot, ep, bg, work)
	})
	if err != nil {
		c.releaseGeneration(genOut.handles)
		c.releaseBaseline(baseOut.handles)
		return FinishAccepted{}, fmt.Errorf("runctl: run %s was aborted while freezing: %w", s.runID, ErrRunAborted)
	}
	return accepted.clone(), nil
}

// finishWorker drains, collects and publishes. Every write to shared state
// goes through publish or commit, so a worker whose run has been aborted can
// run to completion without any of its output ever becoming visible.
func (c *Controller) finishWorker(s *runSlot, ep Epoch, bg context.Context, w finishWork) {
	defer c.endOwner(s)

	// Handles are freed before the run becomes observably finished, so "the
	// run is done" and "the collectors got their resources back" cannot be
	// observed out of order. The deferred call is the safety net for the early
	// returns; releasing twice is a no-op by contract.
	released := false
	releaseAll := func() {
		if released {
			return
		}
		released = true
		c.releaseGeneration(w.genFinal)
		c.releaseGeneration(w.genStart)
		c.releaseBaseline(w.baseStart)
		c.releaseBaseline(w.baseFinal)
	}
	defer releaseAll()

	boundaries := cloneBoundaries(w.boundaries)
	dropped := make(map[string]bool, len(boundaries))
	for _, b := range boundaries {
		if b.Dropped {
			dropped[b.Name] = true
		}
	}
	validity := w.validity

	drainCtx, cancelDrain := context.WithTimeout(bg, c.budgets.Drain)
	for _, h := range w.genFinal {
		rc, ok := c.generationByName(h.Collector)
		if !ok {
			continue
		}
		err := safeErr(rc.reg.Name, "Drain", func() error { return rc.coll.Drain(drainCtx, h) })
		switch {
		case err == nil:
		case isPanic(err):
			// A panicking Drain proves nothing about whether in-flight work
			// left the generation alone, so the section is dropped rather than
			// read while it may still be changing under the reader.
			boundaries = append(boundaries, c.noteCollectFailure(w.runID, rc.reg, KindGeneration, err))
			dropped[h.Collector] = true
			validity = worse(validity, failValidity(rc.reg.Required))
		default:
			// The generation still holds whatever did complete, so the section
			// is kept and only marked as incomplete.
			boundaries = append(boundaries, CollectorBoundary{
				Name:      h.Collector,
				Kind:      KindGeneration,
				Required:  rc.reg.Required,
				Phase:     PhaseCollect,
				Committed: true,
				Code:      CodeDrainTimeout,
				Err:       err.Error(),
			})
			validity = worse(validity, ValidityPartial)
		}
	}
	cancelDrain()

	buildCtx, cancelBuild := context.WithTimeout(bg, c.budgets.SnapshotBuild)
	defer cancelBuild()

	sections := make(map[string]any)
	for _, h := range w.genFinal {
		rc, ok := c.generationByName(h.Collector)
		if !ok || dropped[h.Collector] {
			continue
		}
		value, err := collectGeneration(buildCtx, rc, h)
		if err != nil {
			boundaries = append(boundaries, c.noteCollectFailure(w.runID, rc.reg, KindGeneration, err))
			dropped[h.Collector] = true
			validity = worse(validity, failValidity(rc.reg.Required))
			continue
		}
		sections[h.Collector] = value
	}

	finalByName := make(map[string]BaselineHandle, len(w.baseFinal))
	for _, h := range w.baseFinal {
		finalByName[h.Collector] = h
	}
	for _, base := range w.baseStart {
		rc, ok := c.baselineByName(base.Collector)
		if !ok || dropped[base.Collector] {
			continue
		}
		final, ok := finalByName[base.Collector]
		if !ok {
			// Only one edge of the interval exists; a delta cannot be derived
			// from it and inventing one would be worse than omitting it.
			continue
		}
		value, err := collectBaseline(buildCtx, rc, base, final)
		if err != nil {
			boundaries = append(boundaries, c.noteCollectFailure(w.runID, rc.reg, KindBaseline, err))
			dropped[base.Collector] = true
			validity = worse(validity, failValidity(rc.reg.Required))
			continue
		}
		sections[base.Collector] = value
	}

	snap := &Snapshot{
		RunID:            w.runID,
		Epoch:            ep,
		Trigger:          w.trigger,
		Sections:         sections,
		Collectors:       boundaries,
		GenerationWindow: computeWindow(boundaries, keepGeneration),
		BoundaryWindow:   computeWindow(boundaries, nil),
		StartedAt:        w.startedAt,
		FinishedAt:       c.now(),
	}

	if c.enrich != nil {
		enrichCtx, cancelEnrich := context.WithTimeout(bg, c.budgets.Enrich)
		// The hook is caller-supplied code running on this background
		// goroutine, where an escaping panic would take the measured process
		// down with nobody left to catch it. It gets the same deal as a
		// collector: the failure costs the run its "valid" verdict, nothing
		// more.
		err := safeHook("enrich", func() error { return c.enrich(enrichCtx, snap) })
		if err != nil {
			validity = worse(validity, ValidityPartial)
		}
		if isPanic(err) {
			c.recordHealth(HealthContractViolation, fmt.Sprintf("run %s: %s", w.runID, err.Error()))
		}
		cancelEnrich()
	}
	snap.Validity = validity
	releaseAll()

	if err := c.publish(ep, snap); err != nil {
		// Fenced: this run was aborted. Publishing nothing is the whole point.
		return
	}
	_ = c.commit(ep, func(slot *runSlot) {
		slot.state = StateFinished
		// The run's verdict may degrade here, but FinishAccepted must not: it
		// was already handed to a caller, and a replayed FinishRun has to
		// return exactly what the first one did. The final verdict lives on
		// the run record and the snapshot instead.
		slot.validity = worse(slot.validity, validity)
		slot.since = c.now()
		slot.gen, slot.genFinal, slot.base, slot.baseF = nil, nil, nil, nil
		slot.lease = time.Time{}
		c.sealLocked(slot)
		if slot.cancel != nil {
			slot.cancel()
		}
	})
}

// collectGeneration derives a generation section, refusing to start once the
// build budget is gone.
func collectGeneration(ctx context.Context, rc registeredGeneration, h GenerationHandle) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot build budget exhausted: %w", err)
	}
	return safeResult(rc.reg.Name, "Collect", func() (any, error) { return rc.coll.Collect(h) })
}

// collectBaseline derives a baseline section from two frozen samples.
func collectBaseline(ctx context.Context, rc registeredBaseline, base, final BaselineHandle) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot build budget exhausted: %w", err)
	}
	return safeResult(rc.reg.Name, "Collect", func() (any, error) { return rc.coll.Collect(base, final) })
}

// noteCollectFailure records a section that could not be derived and reports a
// panicking collector through health as well, because a collector bug is
// actionable in a way that a timeout is not.
func (c *Controller) noteCollectFailure(runID string, reg Registration, kind string, err error) CollectorBoundary {
	if isPanic(err) {
		c.recordHealth(HealthContractViolation, fmt.Sprintf("run %s: %s", runID, err.Error()))
	}
	return collectFailure(reg, kind, err)
}

// collectFailure records a section that could not be derived. A panic is
// reported with the contract-violation code because it is a bug in the
// collector rather than a condition of the run; everything else is an ordinary
// collect failure.
func collectFailure(reg Registration, kind string, err error) CollectorBoundary {
	code := CodeCollectFailed
	if isPanic(err) {
		code = CodeContractViolation
	}
	return CollectorBoundary{
		Name:     reg.Name,
		Kind:     kind,
		Required: reg.Required,
		Phase:    PhaseCollect,
		Code:     code,
		Err:      err.Error(),
		Dropped:  true,
	}
}
