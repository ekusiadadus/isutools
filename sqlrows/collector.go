package sqlrows

import (
	"context"
	"fmt"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// CaptureBaseline samples the opening boundary.
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.capture(ctx, runID, ep, runctl.PhaseStartBaseline)
}

// CaptureFinal samples the closing boundary. It also fetches the digest texts
// of the rows that lead the interval, because Collect is not allowed to do
// I/O and the interesting digests are only known once both readings exist.
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error) {
	return c.capture(ctx, runID, ep, runctl.PhaseFinishFinal)
}

// capture implements both boundaries.
//
// Committed follows the collector contract exactly: it is a statement about
// the world ("a sample is fixed for this run"), not about this call. Replaying
// a boundary therefore returns the cached result — At included — instead of
// re-reading counters that have moved on in the meantime.
func (c *Collector) capture(ctx context.Context, runID string, ep runctl.Epoch, phase runctl.Phase) (runctl.SampleResult, error) {
	key := resultKey{run: runKey{runID: runID, epoch: ep}, phase: phase}

	c.mu.Lock()
	if cached, ok := c.results[key]; ok {
		c.mu.Unlock()
		return cached.res, cached.err
	}
	if ep < c.latest {
		latest := c.latest
		c.mu.Unlock()
		// Fenced: this run was displaced by a newer one, and publishing a
		// sample for it would resurrect data the controller has abandoned.
		return runctl.SampleResult{}, fmt.Errorf("%w: sqlrows saw epoch %d after %d", runctl.ErrStaleEpoch, ep, latest)
	}
	if ep > c.latest {
		c.latest = ep
		c.evictBeforeLocked(ep)
	}
	probes := make(map[string]probeResult, len(c.probes))
	for id, probe := range c.probes {
		probes[id] = probe
	}
	var baseline *Sample
	if phase == runctl.PhaseFinishFinal {
		baseline = c.pending[key.run]
	}
	c.mu.Unlock()

	outcome := c.sampleTargets(ctx, phase, probes, baseline)
	at := c.now()

	res := runctl.SampleResult{
		Handle:    runctl.NewBaselineHandle(runID, ep, Name, phase, at, outcome.sample),
		At:        at,
		Committed: outcome.captured > 0 || outcome.failed == 0,
	}
	var err error
	if !res.Committed {
		err = fmt.Errorf("%w: %d target(s) failed", ErrNoTargetCaptured, outcome.failed)
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = fmt.Errorf("%w (%v)", err, ctxErr)
		}
	}

	c.mu.Lock()
	for id, probe := range outcome.probes {
		c.probes[id] = probe
	}
	if phase == runctl.PhaseStartBaseline {
		c.pending[key.run] = outcome.sample
	}
	c.results[key] = cachedResult{res: res, err: err}
	c.mu.Unlock()

	return res, err
}

// Collect derives the interval from two frozen samples.
//
// It reads nothing but the two handles: no database, no registry, not even the
// collector's own pending map. That is the whole point of carrying the sample
// inside the handle, and it is why a snapshot built minutes after the run
// still describes the run.
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) {
	baseSample, err := sampleOf(base)
	if err != nil {
		return nil, err
	}
	finalSample, err := sampleOf(final)
	if err != nil {
		return nil, err
	}
	return buildSection(baseSample, finalSample), nil
}

// sampleOf extracts a *Sample from a handle. A type mismatch is an error
// rather than a panic: a wrongly wired collector must degrade the snapshot,
// not kill the process it is measuring.
func sampleOf(h runctl.BaselineHandle) (*Sample, error) {
	value := h.Sample()
	if value == nil {
		return nil, fmt.Errorf("%w: %s handle carries no sample", ErrSampleType, h.Phase)
	}
	sample, ok := value.(*Sample)
	if !ok {
		return nil, fmt.Errorf("%w: %s handle carries %T", ErrSampleType, h.Phase, value)
	}
	return sample, nil
}

// Release frees what a handle pins. It is idempotent, and safe on a zero
// handle, because runctl releases both edges of an interval even when only one
// of them was ever taken.
func (c *Collector) Release(h runctl.BaselineHandle) {
	if h.Zero() && h.RunID == "" {
		return
	}
	key := resultKey{run: runKey{runID: h.RunID, epoch: h.Epoch}, phase: h.Phase}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.results, key)
	if h.Phase == runctl.PhaseStartBaseline {
		delete(c.pending, key.run)
	}
}

// QuerySampleTextSupported reports whether the target's digest table has a
// QUERY_SAMPLE_TEXT column, and whether that is known at all.
//
// The answer is a by-product of this package's capability probe, and query
// plan capture needs it: exposing it here is what keeps that consumer from
// paying for a second probe — and from probing a target sqlrows has already
// ruled out.
func (c *Collector) QuerySampleTextSupported(targetID string) (supported, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	probe, ok := c.probes[targetID]
	// An unsafe connection yields no verdict either: the probe stopped before
	// it looked at any column, and a consumer must not read "no sample text"
	// into a question that was never asked.
	if !ok || !probe.probed || probe.failed || probe.unsafeConn {
		return false, false
	}
	return probe.hasQuerySampleText, true
}

// evictBeforeLocked drops state belonging to fenced epochs. Without it a run
// that was preempted between its boundaries would keep its baseline sample
// alive until the process exits. The caller holds c.mu.
func (c *Collector) evictBeforeLocked(ep runctl.Epoch) {
	for key := range c.pending {
		if key.epoch < ep {
			delete(c.pending, key)
		}
	}
	for key := range c.results {
		if key.run.epoch < ep {
			delete(c.results, key)
		}
	}
}
