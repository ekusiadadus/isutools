package dbpool

import (
	"context"
	"fmt"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// CaptureBaseline samples every watched pool and freezes that set as the run's
// participants.
//
// Freezing here is what implements deferred activation: a pool watched later
// in the run is absent from this sample and therefore absent from the run's
// report. The alternative — giving it a baseline taken mid-run — would report
// a slice of the interval next to entries covering all of it, in a table whose
// rows are meant to be comparable.
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.capture(ctx, runID, ep, runctl.PhaseStartBaseline)
}

// CaptureFinal samples the run's frozen participants at the closing boundary.
// A participant unwatched mid-run contributes its farewell sample instead of a
// fresh read, so its interval ends where the pool did.
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.capture(ctx, runID, ep, runctl.PhaseFinishFinal)
}

// capture implements both boundaries. It is idempotent per (runID, epoch,
// phase) and returns a populated SampleResult even when it fails, because the
// Controller decides what a run is worth from Committed and must not be handed
// a zero value it cannot interpret.
func (c *Collector) capture(ctx context.Context, runID string, ep runctl.Epoch, phase runctl.Phase) (runctl.SampleResult, error) {
	if err := ctx.Err(); err != nil {
		return c.uncommitted(runID, ep, phase), fmt.Errorf("dbpool: capture %s for run %s: %w", phase, runID, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.run != nil && ep < c.run.epoch {
		return c.uncommitted(runID, ep, phase),
			fmt.Errorf("dbpool: capture %s for run %s at epoch %d, holding epoch %d: %w",
				phase, runID, ep, c.run.epoch, runctl.ErrStaleEpoch)
	}
	if c.run == nil || c.run.epoch != ep || c.run.runID != runID {
		c.run = &runSamples{runID: runID, epoch: ep}
		c.farewell = Sample{}
	}

	if phase == runctl.PhaseStartBaseline {
		return c.captureBaselineLocked(runID, ep), nil
	}
	return c.captureFinalLocked(runID, ep), nil
}

// captureBaselineLocked takes (or replays) the opening sample. The caller
// holds c.mu.
func (c *Collector) captureBaselineLocked(runID string, ep runctl.Epoch) runctl.SampleResult {
	if c.run.baseTaken && c.run.base != nil {
		return *c.run.base
	}
	// Notes describe one run, not the lifetime of the process. Starting a new
	// baseline drops the previous run's diagnostics before recording this run's
	// watch-set verdict.
	c.notes = nil
	c.noteSeen = make(map[string]struct{})
	if len(c.watch) == 0 && !c.everWatched {
		c.noteLocked(HealthNotRegistered + ": WatchDBPool was not called before the run started")
	}
	at := c.now()
	sample := make(Sample, len(c.watch))
	active := make(map[string]struct{}, len(c.watch))
	for id, w := range c.watch {
		stats, ok := safeStats(w.stats)
		if !ok {
			c.noteLocked(fmt.Sprintf("%s: %q panicked at the opening boundary and is excluded from this run",
				HealthSampleFailed, id))
			continue
		}
		sample[id] = PoolSample{Stats: stats, At: at, Display: w.display}
		active[id] = struct{}{}
	}

	res := c.result(runID, ep, runctl.PhaseStartBaseline, at, sample)
	c.run.active = active
	c.run.base, c.run.baseTaken = &res, true
	c.run.final, c.run.finalTaken = nil, false
	c.farewell = Sample{}
	return res
}

// captureFinalLocked takes (or replays) the closing sample. The caller holds
// c.mu.
func (c *Collector) captureFinalLocked(runID string, ep runctl.Epoch) runctl.SampleResult {
	if c.run.finalTaken && c.run.final != nil {
		return *c.run.final
	}
	// Without an opening boundary there is no participant set to close; fall
	// back to whatever is watched now so the result is still well formed.
	// Collect keys on the baseline sample, so such a run reports no entries.
	participants := c.run.active
	if participants == nil {
		participants = make(map[string]struct{}, len(c.watch))
		for id := range c.watch {
			participants[id] = struct{}{}
		}
	}

	at := c.now()
	sample := make(Sample, len(participants))
	for id := range participants {
		// The farewell wins over a live read: if the pool was unwatched and a
		// replacement watched under the same ID, reading the replacement would
		// silently splice two different pools into one interval.
		if farewell, ok := c.farewell[id]; ok {
			sample[id] = farewell
			continue
		}
		w, ok := c.watch[id]
		if !ok {
			c.noteLocked(fmt.Sprintf("%s: %q disappeared before the closing boundary and is dropped from this run",
				HealthSampleFailed, id))
			continue
		}
		stats, sampled := safeStats(w.stats)
		if !sampled {
			c.noteLocked(fmt.Sprintf("%s: %q panicked at the closing boundary and is dropped from this run",
				HealthSampleFailed, id))
			continue
		}
		sample[id] = PoolSample{Stats: stats, At: at, Display: w.display}
	}

	res := c.result(runID, ep, runctl.PhaseFinishFinal, at, sample)
	c.run.final, c.run.finalTaken = &res, true
	return res
}

// result wraps a finished sample in a committed SampleResult.
func (c *Collector) result(runID string, ep runctl.Epoch, phase runctl.Phase, at time.Time, sample Sample) runctl.SampleResult {
	return runctl.SampleResult{
		Handle:    runctl.NewBaselineHandle(runID, ep, Name, phase, at, sample),
		At:        at,
		Committed: true,
	}
}

// uncommitted is the result returned alongside an error: fully addressed, so
// the Controller can attribute the failure, but carrying an empty sample and
// Committed = false.
func (c *Collector) uncommitted(runID string, ep runctl.Epoch, phase runctl.Phase) runctl.SampleResult {
	at := c.now()
	return runctl.SampleResult{
		Handle:    runctl.NewBaselineHandle(runID, ep, Name, phase, at, Sample{}),
		At:        at,
		Committed: false,
	}
}

// Release drops the collector's own reference to a handle's sample. The handle
// keeps its copy — it is a value and Collect must keep working after a
// Release — so this only stops the collector from pinning a finished run.
// Idempotent, and a no-op for a zero handle or another collector's handle.
func (c *Collector) Release(h runctl.BaselineHandle) {
	if h.Collector != Name {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run == nil || c.run.runID != h.RunID || c.run.epoch != h.Epoch {
		return
	}
	switch h.Phase {
	case runctl.PhaseStartBaseline:
		c.run.base = nil
	case runctl.PhaseFinishFinal:
		c.run.final = nil
	}
}
