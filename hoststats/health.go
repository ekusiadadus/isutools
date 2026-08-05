package hoststats

import "strings"

// Health keys this package reports. They live in their own namespace, separate
// from runctl's, and the set is fixed at five: a health view that grows a key
// per condition stops being readable, so a new condition reuses a key with a
// different message.
const (
	// HealthSourceSkipped reports sources that could not be read.
	HealthSourceSkipped = "hoststats-source-skipped"
	// HealthCGroupPathRejected reports a rejected ISUTOOLS_CGROUP_PATH. It is
	// separate from a plain skip because it means an operator's explicit
	// configuration did not take effect.
	HealthCGroupPathRejected = "hoststats-cgroup-path-rejected"
	// HealthCGroupV1 reports a host without cgroup v2.
	HealthCGroupV1 = "hoststats-cgroup-v1"
	// HealthCounterRewind reports counters that moved backwards.
	HealthCounterRewind = "hoststats-counter-rewind"
	// HealthHostChanged reports that the two boundaries saw different hosts or
	// different boots, which voids every interval delta.
	HealthHostChanged = "hoststats-host-changed"
)

// HealthNote is one health observation derived from a Section. Callers copy it
// into whatever health registry they own; this package keeps no global state.
type HealthNote struct {
	Key     string
	Message string
}

// HealthKeys returns every health key this package can report, in the order
// notes are emitted. Tests pin the list so the set cannot grow unnoticed.
func HealthKeys() []string {
	return []string{
		HealthSourceSkipped,
		HealthCGroupPathRejected,
		HealthCGroupV1,
		HealthCounterRewind,
		HealthHostChanged,
	}
}

// healthNotes derives a section's health from the frozen samples and the codes
// already computed for it. At most one note per key is produced, so a registry
// keyed by name loses nothing.
func healthNotes(base, final *Sample, codes []string) []HealthNote {
	var notes []HealthNote
	if skipped := suffixesWithPrefix(codes, CodeNotCapturedPrefix); len(skipped) > 0 {
		notes = append(notes, HealthNote{
			Key:     HealthSourceSkipped,
			Message: "sources not captured: " + strings.Join(skipped, ", "),
		})
	}
	switch skip := cgroupSkipOf(base, final); {
	case strings.HasPrefix(skip, cgroupSkipRejectPrefix):
		notes = append(notes, HealthNote{
			Key:     HealthCGroupPathRejected,
			Message: EnvCGroupPath + " rejected: " + strings.TrimPrefix(skip, cgroupSkipRejectPrefix),
		})
	case skip == cgroupSkipV1:
		notes = append(notes, HealthNote{
			Key:     HealthCGroupV1,
			Message: "cgroup v2 is not available; cgroup limits were skipped",
		})
	}
	if rewound := suffixesWithPrefix(codes, CodeCounterRewindPrefix); len(rewound) > 0 {
		notes = append(notes, HealthNote{
			Key:     HealthCounterRewind,
			Message: "counters went backwards: " + strings.Join(rewound, ", "),
		})
	}
	if changed := hostChangeCodes(codes); len(changed) > 0 {
		notes = append(notes, HealthNote{
			Key:     HealthHostChanged,
			Message: "host changed during the interval: " + strings.Join(changed, ", "),
		})
	}
	return notes
}

// cgroupSkipOf prefers the closing boundary's reason, since that is the one
// the reported limits would have come from.
func cgroupSkipOf(base, final *Sample) string {
	if final.CGroupSkip != "" {
		return final.CGroupSkip
	}
	return base.CGroupSkip
}

// suffixesWithPrefix collects the trailing part of every code sharing a
// prefix, so one note can name every affected source or device.
func suffixesWithPrefix(codes []string, prefix string) []string {
	var out []string
	for _, code := range codes {
		if strings.HasPrefix(code, prefix) {
			out = append(out, strings.TrimPrefix(code, prefix))
		}
	}
	return out
}

// hostChangeCodes lists the identity codes present.
func hostChangeCodes(codes []string) []string {
	var out []string
	for _, code := range codes {
		if code == CodeBootIDChanged || code == CodeMachineIDChanged {
			out = append(out, code)
		}
	}
	return out
}
