package dbpool

import (
	"fmt"
	"sort"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// Collect derives the run's per-pool report from two frozen samples.
//
// It reads nothing but base.Sample() and final.Sample(): no (*sql.DB).Stats
// call, no registry lookup, not even the collector's own watch set. A snapshot
// is built after the run has closed, and a value read at that point would
// describe traffic the run never saw. Everything an Entry needs — Display and
// the per-pool timestamps included — therefore travels inside the sample.
//
// The result is []Entry ordered by TargetID, so a snapshot is byte-stable and
// two runs can be diffed line by line.
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) {
	baseSample, err := sampleOf(base, "baseline")
	if err != nil {
		return nil, err
	}
	finalSample, err := sampleOf(final, "final")
	if err != nil {
		return nil, err
	}

	// The baseline decides who is in the run: a pool watched mid-run appears
	// only in the closing sample, and reporting it here would undo deferred
	// activation.
	ids := make([]string, 0, len(baseSample))
	for id := range baseSample {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	entries := make([]Entry, 0, len(ids))
	for _, id := range ids {
		end, ok := finalSample[id]
		if !ok {
			// The pool had no closing observation at all (its sampler failed).
			// CaptureFinal already recorded why; an entry with no end would be
			// an interval nobody can interpret.
			continue
		}
		entries = append(entries, entryFor(id, baseSample[id], end))
	}
	return entries, nil
}

// sampleOf recovers this collector's sample type from a handle. A mismatch is
// an error and never a panic: a handle addressed to the wrong collector must
// cost one snapshot section, not the process.
func sampleOf(h runctl.BaselineHandle, kind string) (Sample, error) {
	raw := h.Sample()
	sample, ok := raw.(Sample)
	if !ok {
		return nil, fmt.Errorf("dbpool: %s handle of collector %q carries %T, want dbpool.Sample", kind, h.Collector, raw)
	}
	return sample, nil
}

// entryFor turns one pool's pair of samples into an Entry.
func entryFor(targetID string, base, final PoolSample) Entry {
	display := final.Display
	if display == "" {
		display = base.Display
	}
	entry := Entry{
		TargetID:   targetID,
		Display:    display,
		MaxOpen:    final.Stats.MaxOpenConnections,
		Open:       final.Stats.OpenConnections,
		InUse:      final.Stats.InUse,
		Idle:       final.Stats.Idle,
		BaselineAt: base.At,
		FinalAt:    final.At,
	}
	if final.Unwatched {
		entry.Partial = true
		entry.Code = CodeUnwatchedMidRun
	}
	if rewound(base.Stats, final.Stats) {
		// A rewind changes what the numbers mean, so it outranks a truncated
		// interval for the Code: the reader has to know these are absolute
		// values before anything else.
		entry.Partial = true
		entry.Code = CodeCounterRewind
		entry.WaitCount = final.Stats.WaitCount
		entry.WaitDuration = final.Stats.WaitDuration
		entry.MaxIdleClosed = final.Stats.MaxIdleClosed
		entry.MaxIdleTimeClosed = final.Stats.MaxIdleTimeClosed
		entry.MaxLifetimeClosed = final.Stats.MaxLifetimeClosed
		return entry
	}
	entry.WaitCount = final.Stats.WaitCount - base.Stats.WaitCount
	entry.WaitDuration = final.Stats.WaitDuration - base.Stats.WaitDuration
	entry.MaxIdleClosed = final.Stats.MaxIdleClosed - base.Stats.MaxIdleClosed
	entry.MaxIdleTimeClosed = final.Stats.MaxIdleTimeClosed - base.Stats.MaxIdleTimeClosed
	entry.MaxLifetimeClosed = final.Stats.MaxLifetimeClosed - base.Stats.MaxLifetimeClosed
	return entry
}
