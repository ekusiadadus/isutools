package runctl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// generationOutcome is the result of running one generation phase.
type generationOutcome struct {
	boundaries []CollectorBoundary
	handles    []GenerationHandle
	validity   Validity
}

// baselineOutcome is the result of running one baseline phase.
type baselineOutcome struct {
	boundaries []CollectorBoundary
	handles    []BaselineHandle
	validity   Validity
}

// runGenerationPhase switches or freezes every generation collector in
// registration order. The steps stay sequential on purpose: they are pointer
// swaps that finish in microseconds, and their relative order is what keeps
// generations consistent with each other. Every collector is visited even
// after a required failure, so the process is left in one uniform generation
// rather than half-switched.
func (c *Controller) runGenerationPhase(ctx context.Context, phase Phase, runID string, ep Epoch, regs []registeredGeneration, budget time.Duration) generationOutcome {
	out := generationOutcome{validity: ValidityValid}
	if len(regs) == 0 {
		return out
	}

	phaseCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	for _, rc := range regs {
		b := CollectorBoundary{
			Name:     rc.reg.Name,
			Kind:     KindGeneration,
			Required: rc.reg.Required,
			Phase:    phase,
		}
		if err := phaseCtx.Err(); err != nil {
			b.Code = CodeNotCaptured
			b.Err = err.Error()
			b.Dropped = true
			out.validity = worse(out.validity, failValidity(rc.reg.Required))
			out.boundaries = append(out.boundaries, b)
			continue
		}

		opCtx, opCancel := context.WithTimeout(phaseCtx, c.budgets.PerCollectorGeneration)
		var (
			res BoundaryResult
			err error
		)
		if phase == PhaseStartBoundary {
			res, err = safeResult(rc.reg.Name, "BeginBoundary", func() (BoundaryResult, error) {
				return rc.coll.BeginBoundary(opCtx, runID, ep)
			})
		} else {
			res, err = safeResult(rc.reg.Name, "Freeze", func() (BoundaryResult, error) {
				return rc.coll.Freeze(opCtx, runID, ep)
			})
		}
		opCancel()

		b.At = res.At
		b.Committed = res.Committed
		switch {
		case err == nil && res.Committed:
			out.handles = append(out.handles, res.Handle)
		case err == nil:
			// Success without commit is a collector bug, not a runtime
			// condition, so it is escalated to the strongest verdict and
			// surfaced in health where it can be fixed.
			b.Code = CodeContractViolation
			b.Err = "collector reported success without committing the boundary"
			b.Dropped = true
			out.validity = worse(out.validity, ValidityInvalid)
			c.recordHealth(HealthContractViolation, fmt.Sprintf("run %s: %s returned nil error with Committed=false at %s", runID, rc.reg.Name, phase))
		default:
			b.Code = CodeBoundaryFailed
			if isPanic(err) {
				// A panic is a collector bug, not a runtime condition, so it
				// is coded like the other contract violations and surfaced in
				// health. The barrier zeroed the result, so the boundary is
				// treated as uncommitted and the section is dropped.
				b.Code = CodeContractViolation
				c.recordHealth(HealthContractViolation, fmt.Sprintf("run %s: %s", runID, err.Error()))
			}
			b.Err = err.Error()
			b.Dropped = droppedOnGenerationError(phase, res.Committed)
			if res.Committed {
				// The switch took effect: the handle is real and still has to
				// be drained and released, even though the section is dropped.
				out.handles = append(out.handles, res.Handle)
			}
			out.validity = worse(out.validity, failValidity(rc.reg.Required))
		}
		out.boundaries = append(out.boundaries, b)
	}
	return out
}

// droppedOnGenerationError decides whether a failed generation boundary loses
// its snapshot section. At the opening boundary the section has lost its start
// marker and can never produce an interval, so it is always dropped. At the
// closing boundary a committed freeze still holds the whole interval, so the
// data is kept and only the run's validity suffers.
func droppedOnGenerationError(phase Phase, committed bool) bool {
	if phase == PhaseStartBoundary {
		return true
	}
	return !committed
}

// runBaselinePhase samples every baseline collector. Sampling runs in parallel
// because the width of the boundary window is a data-quality property: the
// longer the sampling takes end to end, the less the run's edges mean.
// SerialOnly collectors run first, on their own, and pay for that with a wider
// window.
func (c *Controller) runBaselinePhase(ctx context.Context, phase Phase, runID string, ep Epoch, regs []registeredBaseline, budget time.Duration) baselineOutcome {
	out := baselineOutcome{validity: ValidityValid}
	if len(regs) == 0 {
		return out
	}

	phaseCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type slotResult struct {
		boundary  CollectorBoundary
		handle    BaselineHandle
		hasHandle bool
		validity  Validity
	}
	results := make([]slotResult, len(regs))

	capture := func(i int, rc registeredBaseline) {
		b, h, ok, v := c.captureBaseline(phaseCtx, phase, runID, ep, rc)
		results[i] = slotResult{boundary: b, handle: h, hasHandle: ok, validity: v}
	}

	for i, rc := range regs {
		if rc.reg.SerialOnly {
			capture(i, rc)
		}
	}

	sem := make(chan struct{}, BaselineConcurrency)
	var wg sync.WaitGroup
	for i, rc := range regs {
		if rc.reg.SerialOnly {
			continue
		}
		wg.Add(1)
		go func(i int, rc registeredBaseline) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-phaseCtx.Done():
				results[i] = slotResult{
					boundary: notCapturedBoundary(rc, phase, phaseCtx.Err()),
					validity: failValidity(rc.reg.Required),
				}
				return
			}
			capture(i, rc)
		}(i, rc)
	}
	wg.Wait()

	for _, r := range results {
		out.boundaries = append(out.boundaries, r.boundary)
		out.validity = worse(out.validity, r.validity)
		if r.hasHandle {
			out.handles = append(out.handles, r.handle)
		}
	}
	return out
}

// captureBaseline samples one baseline collector under its own budget and
// classifies the outcome.
func (c *Controller) captureBaseline(phaseCtx context.Context, phase Phase, runID string, ep Epoch, rc registeredBaseline) (CollectorBoundary, BaselineHandle, bool, Validity) {
	b := CollectorBoundary{
		Name:     rc.reg.Name,
		Kind:     KindBaseline,
		Required: rc.reg.Required,
		Phase:    phase,
	}
	if err := phaseCtx.Err(); err != nil {
		return notCapturedBoundary(rc, phase, err), BaselineHandle{}, false, failValidity(rc.reg.Required)
	}

	opCtx, opCancel := context.WithTimeout(phaseCtx, c.budgets.PerCollectorBaseline)
	var (
		res SampleResult
		err error
	)
	if phase == PhaseStartBaseline {
		res, err = safeResult(rc.reg.Name, "CaptureBaseline", func() (SampleResult, error) {
			return rc.coll.CaptureBaseline(opCtx, runID, ep)
		})
	} else {
		res, err = safeResult(rc.reg.Name, "CaptureFinal", func() (SampleResult, error) {
			return rc.coll.CaptureFinal(opCtx, runID, ep)
		})
	}
	opCancel()

	b.At = res.At
	b.Committed = res.Committed
	switch {
	case err == nil && res.Committed:
		return b, res.Handle, true, ValidityValid
	case err == nil:
		b.Code = CodeContractViolation
		b.Err = "collector reported success without committing the sample"
		b.Dropped = true
		c.recordHealth(HealthContractViolation, fmt.Sprintf("run %s: %s returned nil error with Committed=false at %s", runID, rc.reg.Name, phase))
		return b, BaselineHandle{}, false, ValidityInvalid
	default:
		// A deadline or cancellation means the collector never got to produce
		// a sample within its slice of the phase; anything else is a genuine
		// collector failure. The distinction matters to whoever reads the
		// snapshot: one is capacity, the other is a bug.
		b.Code = CodeBoundaryFailed
		switch {
		case isPanic(err):
			// A collector that panicked has a bug; the barrier already zeroed
			// its result, so no sample can be trusted from it.
			b.Code = CodeContractViolation
			c.recordHealth(HealthContractViolation, fmt.Sprintf("run %s: %s", runID, err.Error()))
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			b.Code = CodeNotCaptured
		}
		b.Err = err.Error()
		b.Dropped = true
		// A committed-but-failed sample still has to be released, so the
		// handle is kept even though the section is dropped.
		hasHandle := res.Committed && !res.Handle.Zero()
		return b, res.Handle, hasHandle, failValidity(rc.reg.Required)
	}
}

// notCapturedBoundary records a collector the phase budget never reached.
func notCapturedBoundary(rc registeredBaseline, phase Phase, err error) CollectorBoundary {
	b := CollectorBoundary{
		Name:     rc.reg.Name,
		Kind:     KindBaseline,
		Required: rc.reg.Required,
		Phase:    phase,
		Code:     CodeNotCaptured,
		Dropped:  true,
	}
	if err != nil {
		b.Err = err.Error()
	}
	return b
}
