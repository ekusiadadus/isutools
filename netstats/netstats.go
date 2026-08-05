// Package netstats reports network observations for a benchmark run: a TCP
// socket summary observed at each boundary and per-interface throughput,
// packet, error and drop counters accumulated between them.
//
// Everything here is display-only. No value produced by this package feeds an
// advisor threshold, deliberately: interval averages cannot see instantaneous
// saturation, /proc/net/sockstat cannot tell an inbound TIME_WAIT socket from
// an outbound one, and a single NIC's MTU says nothing about whether the whole
// path agrees on it. Judging those is left to the reader.
//
// Both filesystems are injected separately because procfs and sysfs are
// different mounts: a collector rooted at /proc cannot reach
// /sys/class/net/<if>/speed. Injection also makes every parser testable with
// fstest.MapFS on a non-Linux development host.
package netstats

// Health keys this package reports. The set is fixed at four so a run's health
// stays readable on a host with dozens of virtual interfaces; a new condition
// reuses one of these keys with a different detail rather than adding a key.
const (
	// HealthSysfsUnreadable reports link attributes that were present but
	// unusable (non-numeric or out of range). A missing file is not reported:
	// virtual NICs, network namespaces and older kernels legitimately lack
	// these attributes, so reporting absence would fire on every run.
	HealthSysfsUnreadable = "netstats-sysfs-unreadable"

	// HealthLinkChanged reports a link attribute whose value differed between
	// the two boundaries. The final value is displayed; the note exists so the
	// reader knows the displayed value did not hold for the whole interval.
	HealthLinkChanged = "netstats-link-changed"

	// HealthCounterRewind reports an interface whose counters went backwards,
	// which happens when a NIC is reset or replaced mid-run. Its rates are
	// suppressed rather than shown as an implausible spike.
	HealthCounterRewind = "netstats-counter-rewind"

	// HealthProcUnreadable reports a /proc/net file that could not be parsed.
	// Collection continues with whatever did parse (fail-open): a broken
	// sockstat must not cost the reader the interface table.
	HealthProcUnreadable = "netstats-proc-unreadable"
)

// CodeCounterRewind marks an interface whose counters rewound during the
// interval. Its deltas and rates are omitted; the row is kept so the reader
// still sees that the interface existed.
const CodeCounterRewind = "counter-rewind"

// maxHealthDetails bounds how many items one health note enumerates. A host
// with 64 veth devices would otherwise produce a note nobody reads.
const maxHealthDetails = 8

// HealthNote is one degradation, aggregated per key. It is carried in the
// section rather than pushed into a registry because Collect must be pure:
// the wiring layer copies these into the process-wide health registry.
type HealthNote struct {
	Key string `json:"key"`
	// Detail enumerates up to maxHealthDetails offending items, comma
	// separated, in a stable order.
	Detail string `json:"detail,omitempty"`
}

// NetworkStats is the network section of a run's snapshot.
type NetworkStats struct {
	TCP        TCPSummary   `json:"tcp"`
	Interfaces []Interface  `json:"interfaces"`
	Health     []HealthNote `json:"health,omitempty"`
}

// TCPSummary is a point observation of /proc/net/sockstat and
// /proc/net/sockstat6 taken at the closing boundary. These are gauges, not
// counters, so they are never turned into a delta.
type TCPSummary struct {
	InUse int64 `json:"in_use"`
	// TimeWait counts sockets in TIME_WAIT. It distinguishes neither direction
	// nor local port ownership, so it cannot by itself demonstrate ephemeral
	// port exhaustion.
	TimeWait int64 `json:"time_wait"`
	Orphan   int64 `json:"orphan"`
	InUse6   int64 `json:"in_use6"`
}

// Interface is one NIC's interval counters. The loopback device is excluded by
// default: its traffic is process-to-process and would dominate the table
// without describing the network.
type Interface struct {
	Name string `json:"name"`
	// Appeared marks an interface absent from the opening sample. Its
	// cumulative counters are not an interval delta, so no delta and no rate
	// are reported for it.
	Appeared bool `json:"appeared,omitempty"`
	// Code is a stable machine-readable reason the interval values are absent;
	// empty means the row is complete.
	Code string `json:"code,omitempty"`

	RxBytes   uint64 `json:"rx_bytes"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	TxPackets uint64 `json:"tx_packets"`
	RxErrors  uint64 `json:"rx_errors"`
	TxErrors  uint64 `json:"tx_errors"`
	RxDropped uint64 `json:"rx_dropped"`
	TxDropped uint64 `json:"tx_dropped"`

	// RxMbitPerSec and TxMbitPerSec are Mbit/s so they can be compared with
	// SpeedMbit directly. They are nil, not zero, when no rate can be derived
	// (appeared interface, counter rewind, non-positive interval): zero would
	// read as "idle" when the truth is "unknown".
	RxMbitPerSec *float64 `json:"rx_mbit_per_s,omitempty"`
	TxMbitPerSec *float64 `json:"tx_mbit_per_s,omitempty"`

	// SpeedMbit is /sys/class/net/<if>/speed. Zero means "not accepted" and
	// disappears from JSON; zero is not a real link speed, so it is a safe
	// sentinel.
	SpeedMbit int64 `json:"speed_mbit,omitempty"`
	// MTU is /sys/class/net/<if>/mtu, displayed verbatim. No judgement is
	// attached to it: a jumbo frame only helps when every hop agrees, which a
	// single NIC's attribute cannot show.
	MTU int64 `json:"mtu,omitempty"`
}
