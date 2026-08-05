package netstats

import (
	"fmt"
	"sort"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// bitsPerByte and mbitDivisor convert a byte counter into Mbit/s. Mbit/s is
// the unit the NIC's own speed attribute uses, so the two can be read side by
// side without arithmetic in the reader's head.
const (
	bitsPerByte = 8
	mbitDivisor = 1e6
)

// Collect derives the interval from two frozen samples.
//
// It reads nothing but base.Sample(), final.Sample() and the two SampledAt
// timestamps: no /proc, no /sys, no clock, no field of the collector. That is
// what makes a run's numbers reproducible — load applied after the closing
// boundary cannot leak into an interval that was already fixed.
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) {
	baseSample, ok := base.Sample().(*sample)
	if !ok || baseSample == nil {
		return nil, fmt.Errorf("netstats: baseline sample has type %T, want *netstats.sample", base.Sample())
	}
	finalSample, ok := final.Sample().(*sample)
	if !ok || finalSample == nil {
		return nil, fmt.Errorf("netstats: final sample has type %T, want *netstats.sample", final.Sample())
	}
	return buildStats(baseSample, finalSample, final.SampledAt.Sub(base.SampledAt)), nil
}

// buildStats is the whole interval computation, split out so it can be tested
// without building handles.
func buildStats(base, final *sample, interval time.Duration) *NetworkStats {
	notes := newNoteSet()
	for _, note := range base.Notes {
		notes.add(note.Key, note.Detail)
	}
	for _, note := range final.Notes {
		notes.add(note.Key, note.Detail)
	}

	out := &NetworkStats{
		// The socket summary is a gauge, so only the closing observation is
		// reported. Subtracting two gauges would invent a number that means
		// nothing.
		TCP:        final.TCP,
		Interfaces: make([]Interface, 0, len(final.Devs)),
	}
	for _, name := range sortedNames(final.Devs) {
		out.Interfaces = append(out.Interfaces, buildInterface(name, base, final, interval, notes))
	}
	out.Health = notes.notes()
	return out
}

// buildInterface derives one NIC's row.
func buildInterface(name string, base, final *sample, interval time.Duration, notes *noteSet) Interface {
	end := final.Devs[name]
	iface := Interface{Name: name}

	start, existed := base.Devs[name]
	switch {
	case !existed:
		// The counters are cumulative since the NIC was created, not since the
		// run started. Reporting them as an interval would invent traffic that
		// never happened during the run, so the row keeps its identity and
		// nothing else.
		iface.Appeared = true
	case rewound(start, end):
		iface.Code = CodeCounterRewind
		notes.add(HealthCounterRewind, name)
	default:
		iface.RxBytes = end.RxBytes - start.RxBytes
		iface.TxBytes = end.TxBytes - start.TxBytes
		iface.RxPackets = end.RxPackets - start.RxPackets
		iface.TxPackets = end.TxPackets - start.TxPackets
		iface.RxErrors = end.RxErrors - start.RxErrors
		iface.TxErrors = end.TxErrors - start.TxErrors
		iface.RxDropped = end.RxDropped - start.RxDropped
		iface.TxDropped = end.TxDropped - start.TxDropped
		// A non-positive interval yields no rate at all. Handles can be
		// rebuilt from JSON, where the monotonic clock reading is lost, so the
		// guard is <= 0 rather than == 0.
		if interval > 0 {
			iface.RxMbitPerSec = mbitPerSec(iface.RxBytes, interval)
			iface.TxMbitPerSec = mbitPerSec(iface.TxBytes, interval)
		}
	}

	iface.SpeedMbit = mergeAttr(name, "speed", base.Link[name].SpeedMbit, final.Link[name].SpeedMbit, notes)
	iface.MTU = mergeAttr(name, "mtu", base.Link[name].MTU, final.Link[name].MTU, notes)
	return iface
}

// mergeAttr picks the value to display for a link attribute.
//
// The closing value wins, because that is what the host looked like when the
// snapshot was taken. A disagreement between the boundaries is reported so the
// reader knows the displayed value did not hold for the whole interval — but
// only when both boundaries actually had a value: a NIC that came up mid-run
// is a normal path, not a change worth a note.
func mergeAttr(name, attr string, baseValue, finalValue int64, notes *noteSet) int64 {
	if finalValue == 0 {
		return baseValue
	}
	if baseValue != 0 && baseValue != finalValue {
		notes.add(HealthLinkChanged, fmt.Sprintf("%s:%s %d->%d", name, attr, baseValue, finalValue))
	}
	return finalValue
}

// rewound reports whether any counter went backwards, which means the NIC was
// reset or replaced and no delta over this interval is meaningful.
func rewound(start, end devCounters) bool {
	return end.RxBytes < start.RxBytes ||
		end.TxBytes < start.TxBytes ||
		end.RxPackets < start.RxPackets ||
		end.TxPackets < start.TxPackets ||
		end.RxErrors < start.RxErrors ||
		end.TxErrors < start.TxErrors ||
		end.RxDropped < start.RxDropped ||
		end.TxDropped < start.TxDropped
}

// mbitPerSec converts a byte delta into Mbit/s. The result is a pointer so an
// absent rate is distinguishable from a measured zero.
func mbitPerSec(bytes uint64, interval time.Duration) *float64 {
	rate := float64(bytes) * bitsPerByte / mbitDivisor / interval.Seconds()
	return &rate
}

// sortedNames orders interfaces so two identical runs produce identical JSON.
func sortedNames(devices map[string]devCounters) []string {
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
