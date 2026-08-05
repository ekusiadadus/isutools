package runctl

import (
	"fmt"
	"strings"
	"time"
)

// computeWindow returns the measured window spanned by the boundaries that
// pass the filter. Collectors that never ran carry a zero time and are
// excluded, because counting them would report a window of decades.
func computeWindow(boundaries []CollectorBoundary, keep func(CollectorBoundary) bool) BoundaryWindow {
	var w BoundaryWindow
	seen := false
	for _, b := range boundaries {
		if b.At.IsZero() || (keep != nil && !keep(b)) {
			continue
		}
		if !seen {
			w.Min, w.Max, seen = b.At, b.At, true
			continue
		}
		if b.At.Before(w.Min) {
			w.Min = b.At
		}
		if b.At.After(w.Max) {
			w.Max = b.At
		}
	}
	if seen {
		w.Spread = w.Max.Sub(w.Min)
	}
	return w
}

// keepGeneration selects generation-kind boundaries.
func keepGeneration(b CollectorBoundary) bool { return b.Kind == KindGeneration }

// spreadVerdict is the outcome of judging one window against its limit.
type spreadVerdict struct {
	validity  Validity
	offenders []string
}

// judgeSpread applies the spread rule to one window. The rule is deliberately
// two-tier: a window blown open only by optional collectors still yields a
// usable interval, so it degrades to partial; a window blown open by the
// required ones means the interval's own edges are untrustworthy and nothing
// downstream should compare against it.
func judgeSpread(boundaries []CollectorBoundary, keep func(CollectorBoundary) bool, limit time.Duration) spreadVerdict {
	all := computeWindow(boundaries, keep)
	if all.Spread <= limit {
		return spreadVerdict{validity: ValidityValid}
	}

	requiredOnly := func(b CollectorBoundary) bool {
		return b.Required && (keep == nil || keep(b))
	}
	req := computeWindow(boundaries, requiredOnly)

	v := ValidityPartial
	if req.Spread > limit {
		v = ValidityInvalid
	}

	offenders := make([]string, 0, len(boundaries))
	for _, b := range boundaries {
		if b.At.IsZero() || (keep != nil && !keep(b)) {
			continue
		}
		if b.At.Sub(all.Min) > limit {
			offenders = append(offenders, b.Name)
		}
	}
	return spreadVerdict{validity: v, offenders: offenders}
}

// applySpread judges both windows of a boundary, marks the offending
// collectors and reports the resulting validity. It mutates only the Code
// field of boundaries that have no code yet, so a collector that already
// failed keeps the more specific reason.
func (c *Controller) applySpread(runID string, boundaries []CollectorBoundary, phaseLabel string) Validity {
	gen := judgeSpread(boundaries, keepGeneration, c.budgets.SpreadGeneration)
	all := judgeSpread(boundaries, nil, c.budgets.SpreadBoundary)

	v := worse(gen.validity, all.validity)
	offenders := append(append([]string(nil), gen.offenders...), all.offenders...)
	if len(offenders) == 0 {
		return v
	}

	unique := make(map[string]bool, len(offenders))
	names := make([]string, 0, len(offenders))
	for _, name := range offenders {
		if unique[name] {
			continue
		}
		unique[name] = true
		names = append(names, name)
	}
	for i := range boundaries {
		if unique[boundaries[i].Name] && boundaries[i].Code == "" {
			boundaries[i].Code = CodeSpreadExceeded
		}
	}
	c.recordHealth(HealthBoundarySpread, fmt.Sprintf("run %s %s boundary spread exceeded: %s", runID, phaseLabel, strings.Join(names, ", ")))
	return v
}
