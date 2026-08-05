package netstats

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// handlePair freezes two samples into handles separated by interval.
func handlePair(base, final *sample, interval time.Duration) (runctl.BaselineHandle, runctl.BaselineHandle) {
	at := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	return runctl.NewBaselineHandle("run-1", 1, "network", runctl.PhaseStartBaseline, at, base),
		runctl.NewBaselineHandle("run-1", 1, "network", runctl.PhaseFinishFinal, at.Add(interval), final)
}

// collectPair runs Collect over two samples and asserts the happy path.
func collectPair(t *testing.T, c *Collector, base, final *sample, interval time.Duration) *NetworkStats {
	t.Helper()
	baseHandle, finalHandle := handlePair(base, final, interval)
	value, err := c.Collect(baseHandle, finalHandle)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	stats, ok := value.(*NetworkStats)
	if !ok {
		t.Fatalf("Collect() = %T, want *NetworkStats", value)
	}
	return stats
}

// findInterface returns the row for name.
func findInterface(t *testing.T, stats *NetworkStats, name string) Interface {
	t.Helper()
	for _, iface := range stats.Interfaces {
		if iface.Name == name {
			return iface
		}
	}
	t.Fatalf("interface %q missing from %+v", name, stats.Interfaces)
	return Interface{}
}

// findNote returns the aggregated note for a key.
func findNote(t *testing.T, stats *NetworkStats, key string) HealthNote {
	t.Helper()
	for _, note := range stats.Health {
		if note.Key == key {
			return note
		}
	}
	t.Fatalf("health note %q missing from %+v", key, stats.Health)
	return HealthNote{}
}

// TestCollectDeltaAndRates fixes the interval arithmetic, including the byte
// to Mbit/s conversion that makes the rate comparable with the link speed:
// 12,500,000 B/s is exactly 100 Mbit/s.
func TestCollectDeltaAndRates(t *testing.T) {
	base := &sample{
		Devs: map[string]devCounters{"eth0": {
			RxBytes: 1_000_000, TxBytes: 2_000_000,
			RxPackets: 100, TxPackets: 200,
			RxErrors: 1, TxErrors: 2, RxDropped: 3, TxDropped: 4,
		}},
		Link: map[string]linkAttrs{"eth0": {SpeedMbit: 1000, MTU: 1500}},
	}
	final := &sample{
		TCP: TCPSummary{InUse: 12, TimeWait: 57, Orphan: 1, InUse6: 3},
		Devs: map[string]devCounters{"eth0": {
			RxBytes: 1_000_000 + 25_000_000, TxBytes: 2_000_000 + 1_250_000,
			RxPackets: 1100, TxPackets: 700,
			RxErrors: 6, TxErrors: 9, RxDropped: 3, TxDropped: 14,
		}},
		Link: map[string]linkAttrs{"eth0": {SpeedMbit: 1000, MTU: 1500}},
	}

	c := New(nil, nil)
	stats := collectPair(t, c, base, final, 2*time.Second)

	if stats.TCP != final.TCP {
		t.Fatalf("TCP = %+v, want the closing observation %+v", stats.TCP, final.TCP)
	}
	eth0 := findInterface(t, stats, "eth0")
	want := Interface{
		Name:      "eth0",
		RxBytes:   25_000_000,
		TxBytes:   1_250_000,
		RxPackets: 1000,
		TxPackets: 500,
		RxErrors:  5,
		TxErrors:  7,
		RxDropped: 0,
		TxDropped: 10,
		SpeedMbit: 1000,
		MTU:       1500,
	}
	got := eth0
	got.RxMbitPerSec, got.TxMbitPerSec = nil, nil
	if got != want {
		t.Fatalf("interface = %+v, want %+v", got, want)
	}
	// 25 MB over 2 s = 12.5 MB/s = 100 Mbit/s.
	assertRate(t, eth0.RxMbitPerSec, 100)
	// 1.25 MB over 2 s = 625 kB/s = 5 Mbit/s.
	assertRate(t, eth0.TxMbitPerSec, 5)
	if len(stats.Health) != 0 {
		t.Fatalf("health = %+v, want none", stats.Health)
	}
}

// assertRate compares a rate pointer with an expected Mbit/s value.
func assertRate(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("rate = nil, want %v Mbit/s", want)
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Fatalf("rate = %v Mbit/s, want %v", *got, want)
	}
}

// TestCollectRateEdgeCases covers every reason a rate is absent. Absent is a
// nil pointer rather than zero, because zero reads as "idle" when the truth is
// "unknown".
func TestCollectRateEdgeCases(t *testing.T) {
	baseCounters := devCounters{RxBytes: 1000, TxBytes: 1000, RxPackets: 10, TxPackets: 10}

	tests := []struct {
		name         string
		base         map[string]devCounters
		final        devCounters
		interval     time.Duration
		wantAppeared bool
		wantCode     string
		wantHealth   string
	}{
		{
			name:         "appeared interface",
			base:         map[string]devCounters{},
			final:        devCounters{RxBytes: 5000, TxBytes: 5000},
			interval:     time.Second,
			wantAppeared: true,
		},
		{
			name:       "counter rewind",
			base:       map[string]devCounters{"eth0": baseCounters},
			final:      devCounters{RxBytes: 10, TxBytes: 10},
			interval:   time.Second,
			wantCode:   CodeCounterRewind,
			wantHealth: HealthCounterRewind,
		},
		{
			name:     "zero interval",
			base:     map[string]devCounters{"eth0": baseCounters},
			final:    devCounters{RxBytes: 2000, TxBytes: 2000, RxPackets: 20, TxPackets: 20},
			interval: 0,
		},
		{
			name:     "negative interval",
			base:     map[string]devCounters{"eth0": baseCounters},
			final:    devCounters{RxBytes: 2000, TxBytes: 2000, RxPackets: 20, TxPackets: 20},
			interval: -time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &sample{Devs: tt.base, Link: map[string]linkAttrs{}}
			final := &sample{
				Devs: map[string]devCounters{"eth0": tt.final},
				Link: map[string]linkAttrs{},
			}
			stats := collectPair(t, New(nil, nil), base, final, tt.interval)
			eth0 := findInterface(t, stats, "eth0")
			if eth0.RxMbitPerSec != nil || eth0.TxMbitPerSec != nil {
				t.Fatalf("rates = %v/%v, want nil", eth0.RxMbitPerSec, eth0.TxMbitPerSec)
			}
			if eth0.Appeared != tt.wantAppeared {
				t.Fatalf("Appeared = %v, want %v", eth0.Appeared, tt.wantAppeared)
			}
			if eth0.Code != tt.wantCode {
				t.Fatalf("Code = %q, want %q", eth0.Code, tt.wantCode)
			}
			if tt.wantAppeared || tt.wantCode != "" {
				if eth0.RxBytes != 0 || eth0.TxBytes != 0 {
					t.Fatalf("deltas = %d/%d, want zero when no interval can be derived",
						eth0.RxBytes, eth0.TxBytes)
				}
			}
			if tt.wantHealth != "" {
				if note := findNote(t, stats, tt.wantHealth); note.Detail != "eth0" {
					t.Fatalf("detail = %q, want the interface name", note.Detail)
				}
			}
		})
	}
}

// TestCollectRewindDetectedOnEveryCounter checks that a rewind of any single
// counter demotes the row. A NIC reset moves them all, but a partial reset
// still makes the delta meaningless.
func TestCollectRewindDetectedOnEveryCounter(t *testing.T) {
	start := devCounters{
		RxBytes: 10, RxPackets: 10, RxErrors: 10, RxDropped: 10,
		TxBytes: 10, TxPackets: 10, TxErrors: 10, TxDropped: 10,
	}
	fields := map[string]func(*devCounters){
		"rx bytes":   func(d *devCounters) { d.RxBytes = 9 },
		"tx bytes":   func(d *devCounters) { d.TxBytes = 9 },
		"rx packets": func(d *devCounters) { d.RxPackets = 9 },
		"tx packets": func(d *devCounters) { d.TxPackets = 9 },
		"rx errors":  func(d *devCounters) { d.RxErrors = 9 },
		"tx errors":  func(d *devCounters) { d.TxErrors = 9 },
		"rx dropped": func(d *devCounters) { d.RxDropped = 9 },
		"tx dropped": func(d *devCounters) { d.TxDropped = 9 },
	}
	for name, rewind := range fields {
		t.Run(name, func(t *testing.T) {
			end := start
			rewind(&end)
			base := &sample{Devs: map[string]devCounters{"eth0": start}, Link: map[string]linkAttrs{}}
			final := &sample{Devs: map[string]devCounters{"eth0": end}, Link: map[string]linkAttrs{}}
			stats := collectPair(t, New(nil, nil), base, final, time.Second)
			if got := findInterface(t, stats, "eth0").Code; got != CodeCounterRewind {
				t.Fatalf("Code = %q, want %q", got, CodeCounterRewind)
			}
		})
	}
}

// TestCollectOrdersInterfacesAndDropsVanished checks that the table is
// deterministic and describes the closing sample: an interface that
// disappeared mid-run has no closing counters to show.
func TestCollectOrdersInterfacesAndDropsVanished(t *testing.T) {
	base := &sample{
		Devs: map[string]devCounters{"eth0": {}, "eth1": {}, "gone": {}},
		Link: map[string]linkAttrs{},
	}
	final := &sample{
		Devs: map[string]devCounters{"eth1": {}, "eth0": {}, "wg0": {}},
		Link: map[string]linkAttrs{},
	}
	stats := collectPair(t, New(nil, nil), base, final, time.Second)
	var names []string
	for _, iface := range stats.Interfaces {
		names = append(names, iface.Name)
	}
	if fmt.Sprint(names) != fmt.Sprint([]string{"eth0", "eth1", "wg0"}) {
		t.Fatalf("interfaces = %v, want them sorted and taken from the closing sample", names)
	}
}

// TestNetstatsLinkAttrsChanged_MTU checks that a link attribute changing
// mid-run displays the closing value and says so.
func TestNetstatsLinkAttrsChanged_MTU(t *testing.T) {
	base := &sample{
		Devs: map[string]devCounters{"eth0": {}},
		Link: map[string]linkAttrs{"eth0": {SpeedMbit: 1000, MTU: 1500}},
	}
	final := &sample{
		Devs: map[string]devCounters{"eth0": {}},
		Link: map[string]linkAttrs{"eth0": {SpeedMbit: 10000, MTU: 9000}},
	}
	stats := collectPair(t, New(nil, nil), base, final, time.Second)
	eth0 := findInterface(t, stats, "eth0")
	if eth0.MTU != 9000 || eth0.SpeedMbit != 10000 {
		t.Fatalf("attrs = speed %d, mtu %d; want the closing values", eth0.SpeedMbit, eth0.MTU)
	}
	note := findNote(t, stats, HealthLinkChanged)
	if !strings.Contains(note.Detail, "eth0:mtu 1500->9000") {
		t.Fatalf("detail = %q, want it to record the MTU change", note.Detail)
	}
	if !strings.Contains(note.Detail, "eth0:speed 1000->10000") {
		t.Fatalf("detail = %q, want it to record the speed change", note.Detail)
	}
}

// TestCollectLinkAttrsOneSidedAreQuiet checks the normal path where a NIC came
// up mid-run: taking the value that exists is not a change worth reporting.
func TestCollectLinkAttrsOneSidedAreQuiet(t *testing.T) {
	tests := []struct {
		name    string
		base    linkAttrs
		final   linkAttrs
		wantMTU int64
	}{
		{name: "only final", base: linkAttrs{}, final: linkAttrs{MTU: 9000}, wantMTU: 9000},
		{name: "only base", base: linkAttrs{MTU: 1500}, final: linkAttrs{}, wantMTU: 1500},
		{name: "identical", base: linkAttrs{MTU: 1500}, final: linkAttrs{MTU: 1500}, wantMTU: 1500},
		{name: "neither", base: linkAttrs{}, final: linkAttrs{}, wantMTU: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := &sample{
				Devs: map[string]devCounters{"eth0": {}},
				Link: map[string]linkAttrs{"eth0": tt.base},
			}
			final := &sample{
				Devs: map[string]devCounters{"eth0": {}},
				Link: map[string]linkAttrs{"eth0": tt.final},
			}
			stats := collectPair(t, New(nil, nil), base, final, time.Second)
			if got := findInterface(t, stats, "eth0").MTU; got != tt.wantMTU {
				t.Fatalf("MTU = %d, want %d", got, tt.wantMTU)
			}
			if len(stats.Health) != 0 {
				t.Fatalf("health = %+v, want none", stats.Health)
			}
		})
	}
}

// TestNetstatsLinkAttrsAppearedNIC_MTU checks that adopting the closing MTU of
// a NIC that appeared mid-run does not also make its rate computable.
func TestNetstatsLinkAttrsAppearedNIC_MTU(t *testing.T) {
	base := &sample{Devs: map[string]devCounters{}, Link: map[string]linkAttrs{}}
	final := &sample{
		Devs: map[string]devCounters{"eth1": {RxBytes: 5_000_000, TxBytes: 5_000_000}},
		Link: map[string]linkAttrs{"eth1": {SpeedMbit: 1000, MTU: 9000}},
	}
	stats := collectPair(t, New(nil, nil), base, final, time.Second)
	eth1 := findInterface(t, stats, "eth1")
	if !eth1.Appeared {
		t.Fatal("Appeared = false for an interface absent from the opening sample")
	}
	if eth1.MTU != 9000 || eth1.SpeedMbit != 1000 {
		t.Fatalf("attrs = speed %d, mtu %d; want the closing values", eth1.SpeedMbit, eth1.MTU)
	}
	if eth1.RxMbitPerSec != nil || eth1.TxMbitPerSec != nil {
		t.Fatal("an appeared interface must have no rate; its counters predate the run")
	}
	if len(stats.Health) != 0 {
		t.Fatalf("health = %+v, want none: a NIC coming up is a normal path", stats.Health)
	}
}

// TestNetstatsMTU_NoAdvice is the display-only regression guard. MTU may
// influence nothing but its own field: no health note, no interface code, no
// change to any other byte of the section. If a future advice ever reads MTU,
// it will have to read it from somewhere this test can see.
func TestNetstatsMTU_NoAdvice(t *testing.T) {
	fixture := func(mtu int64) *NetworkStats {
		attrs := linkAttrs{SpeedMbit: 1000}
		attrs.MTU = mtu
		base := &sample{
			Devs: map[string]devCounters{"eth0": {RxBytes: 1000, TxBytes: 1000}},
			Link: map[string]linkAttrs{"eth0": attrs},
		}
		final := &sample{
			TCP:  TCPSummary{InUse: 5, TimeWait: 9},
			Devs: map[string]devCounters{"eth0": {RxBytes: 13_500_000, TxBytes: 2_250_000}},
			Link: map[string]linkAttrs{"eth0": attrs},
		}
		return collectPair(t, New(nil, nil), base, final, time.Second)
	}

	// Rendering each fixture with its own mtu field blanked must give
	// byte-identical JSON: MTU is carried, never consumed.
	reference := ""
	for _, mtu := range []int64{0, 1500, 9000} {
		stats := fixture(mtu)
		if got := findInterface(t, stats, "eth0").MTU; got != mtu {
			t.Fatalf("MTU = %d, want %d carried through verbatim", got, mtu)
		}
		if len(stats.Health) != 0 {
			t.Fatalf("MTU %d produced health %+v; MTU is displayed, not judged", mtu, stats.Health)
		}
		if code := findInterface(t, stats, "eth0").Code; code != "" {
			t.Fatalf("MTU %d produced code %q", mtu, code)
		}
		stats.Interfaces[0].MTU = 0
		encoded, err := json.Marshal(stats)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if reference == "" {
			reference = string(encoded)
			continue
		}
		if string(encoded) != reference {
			t.Fatalf("MTU %d changed the rest of the section:\n got %s\nwant %s", mtu, encoded, reference)
		}
	}
}

// TestCollectJSONShape pins the wire shape: absent link attributes and absent
// rates disappear instead of being reported as zero.
func TestCollectJSONShape(t *testing.T) {
	base := &sample{Devs: map[string]devCounters{}, Link: map[string]linkAttrs{}}
	final := &sample{
		TCP:  TCPSummary{InUse: 1, TimeWait: 2, Orphan: 3, InUse6: 4},
		Devs: map[string]devCounters{"eth0": {}},
		Link: map[string]linkAttrs{},
	}
	stats := collectPair(t, New(nil, nil), base, final, time.Second)
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	text := string(encoded)
	for _, absent := range []string{"speed_mbit", "mtu", "rx_mbit_per_s", "tx_mbit_per_s", "health", "code"} {
		if strings.Contains(text, absent) {
			t.Fatalf("JSON contains %q, want it omitted: %s", absent, text)
		}
	}
	for _, present := range []string{"in_use6", "rx_bytes", "tx_dropped", "appeared"} {
		if !strings.Contains(text, present) {
			t.Fatalf("JSON is missing %q: %s", present, text)
		}
	}
}

// panicFS fails the test if anything opens it. It is how Collect's purity is
// proven rather than asserted in a comment.
type panicFS struct{ t *testing.T }

func (p panicFS) Open(name string) (fs.File, error) {
	p.t.Fatalf("Collect performed I/O: opened %q", name)
	return nil, fs.ErrNotExist
}

// TestNetstatsCollect_UsesFrozenSamplesOnly is the conformance test for the
// rule that makes a run reproducible: the interval comes from the two frozen
// samples, never from the collector or the filesystems. A sysfs attribute read
// late — after the closing boundary — would be caught here.
func TestNetstatsCollect_UsesFrozenSamplesOnly(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	ctx := context.Background()
	baseRes, err := c.CaptureBaseline(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v", err)
	}

	// Everything the host could still tell us changes between the boundaries.
	procFS := procFSWith(map[string]string{
		"net/dev": devFile(map[string]devCounters{
			"eth0": {RxBytes: 13_500_000, TxBytes: 3_000_000, RxPackets: 60, TxPackets: 70},
		}),
		"net/sockstat":  "TCP: inuse 99 orphan 9 tw 999\n",
		"net/sockstat6": "TCP6: inuse 9\n",
	})
	c.procFS = procFS
	c.sysFS = sysFSWith(map[string]string{
		linkAttrPath("eth0", "speed"): "10000\n",
		linkAttrPath("eth0", "mtu"):   "9000\n",
	})
	finalRes, err := c.CaptureFinal(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal() error = %v", err)
	}

	// Load applied after the closing boundary, plus filesystems that scream if
	// touched, plus a collector whose cache has been released.
	c.procFS = panicFS{t: t}
	c.sysFS = panicFS{t: t}
	c.Release(baseRes.Handle)
	c.Release(finalRes.Handle)

	interval := finalRes.At.Sub(baseRes.At)
	value, err := c.Collect(baseRes.Handle, finalRes.Handle)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	stats := value.(*NetworkStats)
	eth0 := findInterface(t, stats, "eth0")
	if eth0.RxBytes != 13_500_000-1000 {
		t.Fatalf("RxBytes = %d, want the frozen delta", eth0.RxBytes)
	}
	if eth0.MTU != 9000 || eth0.SpeedMbit != 10000 {
		t.Fatalf("attrs = speed %d, mtu %d; want the closing sample's frozen values",
			eth0.SpeedMbit, eth0.MTU)
	}
	if stats.TCP.TimeWait != 999 {
		t.Fatalf("TimeWait = %d, want the closing observation", stats.TCP.TimeWait)
	}
	assertRate(t, eth0.RxMbitPerSec, float64(13_500_000-1000)*8/1e6/interval.Seconds())

	// A second Collect over the same handles must produce the same numbers:
	// nothing was consumed and nothing was read live.
	again, err := c.Collect(baseRes.Handle, finalRes.Handle)
	if err != nil {
		t.Fatalf("second Collect() error = %v", err)
	}
	first, _ := json.Marshal(stats)
	second, _ := json.Marshal(again)
	if string(first) != string(second) {
		t.Fatalf("Collect is not repeatable:\n%s\n%s", first, second)
	}
}

// TestCollectRejectsForeignSamples checks that a mismatched handle is an error
// and never a panic: measurement may not break the measured application.
func TestCollectRejectsForeignSamples(t *testing.T) {
	c := New(nil, nil)
	good := runctl.NewBaselineHandle("run-1", 1, "network", runctl.PhaseStartBaseline,
		time.Now(), &sample{Devs: map[string]devCounters{}, Link: map[string]linkAttrs{}})
	tests := []struct {
		name        string
		base, final runctl.BaselineHandle
	}{
		{
			name:  "foreign baseline",
			base:  runctl.NewBaselineHandle("run-1", 1, "network", runctl.PhaseStartBaseline, time.Now(), "not a sample"),
			final: good,
		},
		{
			name:  "foreign final",
			base:  good,
			final: runctl.NewBaselineHandle("run-1", 1, "network", runctl.PhaseFinishFinal, time.Now(), 42),
		},
		{
			name:  "empty handles",
			base:  runctl.BaselineHandle{},
			final: runctl.BaselineHandle{},
		},
		{
			name:  "typed nil sample",
			base:  runctl.NewBaselineHandle("run-1", 1, "network", runctl.PhaseStartBaseline, time.Now(), (*sample)(nil)),
			final: good,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.Collect(tt.base, tt.final); err == nil {
				t.Fatal("Collect() error = nil, want a typed error rather than a panic")
			}
		})
	}
}

// TestReadLinkAttrs_MTUHealthCapped checks that a host full of broken
// interfaces yields one readable note instead of dozens.
func TestReadLinkAttrs_MTUHealthCapped(t *testing.T) {
	const nics = 32
	devices := make(map[string]devCounters, nics)
	sysFiles := make(map[string]string, nics)
	for i := 0; i < nics; i++ {
		name := fmt.Sprintf("veth%02d", i)
		devices[name] = devCounters{RxBytes: 1, TxBytes: 1}
		sysFiles[linkAttrPath(name, "mtu")] = "abc\n"
	}
	c := New(procFSWith(map[string]string{"net/dev": devFile(devices)}), sysFSWith(sysFiles))

	ctx := context.Background()
	baseRes, err := c.CaptureBaseline(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v", err)
	}
	finalRes, err := c.CaptureFinal(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal() error = %v", err)
	}
	value, err := c.Collect(baseRes.Handle, finalRes.Handle)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	stats := value.(*NetworkStats)

	if len(stats.Health) != 1 {
		t.Fatalf("health = %+v, want exactly one aggregated note", stats.Health)
	}
	note := stats.Health[0]
	if note.Key != HealthSysfsUnreadable {
		t.Fatalf("key = %q, want %q", note.Key, HealthSysfsUnreadable)
	}
	details := strings.Split(note.Detail, ",")
	if len(details) != maxHealthDetails {
		t.Fatalf("detail lists %d items, want %d: %q", len(details), maxHealthDetails, note.Detail)
	}
	if details[0] != "veth00:mtu=abc" {
		t.Fatalf("first detail = %q, want veth00:mtu=abc", details[0])
	}
	if len(stats.Interfaces) != nics {
		t.Fatalf("interfaces = %d, want all %d rows despite the unreadable attribute",
			len(stats.Interfaces), nics)
	}
}
