package runctl

import (
	"context"
	"fmt"
	"time"
)

// AbortRun abandons a run. The ordering below is the specification, not an
// implementation detail:
//
//  1. under the mutex, move to aborting and advance the Controller epoch, which
//     fences every worker still belonging to this run;
//  2. release the mutex, then cancel the run's context — waiting for a
//     collector while holding the mutex would deadlock a reset called from
//     inside an instrumented handler;
//  3. join the worker, bounded by the abort join budget;
//  4. release the handles, or leave that to the detached worker's own cleanup;
//  5. settle in aborted so the next run can start.
//
// Step 1 is what makes step 3's timeout harmless: a detached worker's commit
// and publish are already rejected, and its handles only reach the generation
// it was given, so it can neither publish this run's data nor corrupt the next
// run's. A join timeout costs delayed cleanup and nothing else.
//
// Aborting a run the Controller does not know is a successful no-op. Stopping
// something that is already stopped is always satisfied, and callers retrying
// an abort after a lost response must not be told the world is inconsistent.
func (c *Controller) AbortRun(ctx context.Context, runID, reason string) (AbortResult, error) {
	if reason == "" {
		reason = ReasonExplicit
	}
	for {
		c.mu.Lock()
		s := c.lookupLocked(runID)
		if s == nil {
			now := c.now()
			c.mu.Unlock()
			return AbortResult{RunID: runID, Reason: reason, AbortedAt: now}, nil
		}

		switch s.state {
		case StateAborted:
			result := s.abort
			c.mu.Unlock()
			return result.clone(), nil
		case StateAcknowledged, StateExpired:
			result := AbortResult{RunID: runID, Epoch: s.epoch, Reason: reason, AbortedAt: c.now()}
			c.mu.Unlock()
			return result, nil
		case StateAborting:
			changed := s.changed
			c.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return AbortResult{}, fmt.Errorf("runctl: waiting for the abort of run %s: %w", runID, ctx.Err())
			}
		}

		// Step 1: fence.
		s.state = StateAborting
		s.since = c.now()
		c.epoch++
		c.sealLocked(s)
		cancel, done, ep := s.cancel, s.done, s.epoch
		partial := committedNames(s.start.Collectors)
		event := RunTerminationEvent{
			RunID: runID, Epoch: ep, State: StateAborting,
			Validity: ValidityInvalid, Reason: reason, BoundaryAt: s.since,
		}
		c.notifyLocked(s)
		c.mu.Unlock()
		c.observeTermination(event)

		// Step 2: cancel outside the mutex.
		if cancel != nil {
			cancel()
		}

		// Step 3: bounded join.
		detached := false
		timer := time.NewTimer(c.budgets.AbortJoin)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			detached = true
		}

		// Steps 4 and 5.
		c.mu.Lock()
		now := c.now()
		s.state = StateAborted
		s.validity = ValidityInvalid
		s.reason = reason
		s.detached = detached
		s.since = now
		s.lease = time.Time{}
		s.snapshot = nil
		result := AbortResult{
			RunID:     runID,
			Epoch:     ep,
			Reason:    reason,
			Detached:  detached,
			AbortedAt: now,
			Partial:   partial,
		}
		s.abort = result
		var (
			gen  []GenerationHandle
			base []BaselineHandle
		)
		if !detached {
			gen = append(append(gen, s.gen...), s.genFinal...)
			base = append(append(base, s.base...), s.baseF...)
			s.gen, s.genFinal, s.base, s.baseF = nil, nil, nil, nil
		}
		c.notifyLocked(s)
		c.mu.Unlock()

		c.releaseGeneration(gen)
		c.releaseBaseline(base)

		if detached {
			c.recordHealth(HealthWorkerDetached, fmt.Sprintf("run %s: worker outlived the abort join budget and was detached", runID))
			go c.reap(s, done)
		}
		return result.clone(), nil
	}
}

// reap waits for a detached worker so its handles are eventually reclaimed.
// Correctness never depends on this: the run is already fenced. Only memory
// does, which is why giving up after the reap budget is acceptable.
func (c *Controller) reap(s *runSlot, done chan struct{}) {
	timer := time.NewTimer(c.budgets.DetachedReap)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		return
	case <-c.stop:
		return
	}

	c.mu.Lock()
	gen := append(append([]GenerationHandle(nil), s.gen...), s.genFinal...)
	base := append(append([]BaselineHandle(nil), s.base...), s.baseF...)
	s.gen, s.genFinal, s.base, s.baseF = nil, nil, nil, nil
	c.mu.Unlock()

	c.releaseGeneration(gen)
	c.releaseBaseline(base)
}
