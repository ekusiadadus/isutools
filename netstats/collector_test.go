package netstats

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// devFile renders a /proc/net/dev whose rows carry the given counters. The
// columns we do not read are left at zero, so a test that cares about column
// positions uses devLine instead.
func devFile(devices map[string]devCounters) string {
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	sort.Strings(names)

	out := netDevHeader
	for _, name := range names {
		counters := devices[name]
		var columns [netDevFields]uint64
		columns[colRxBytes] = counters.RxBytes
		columns[colRxPackets] = counters.RxPackets
		columns[colRxErrors] = counters.RxErrors
		columns[colRxDropped] = counters.RxDropped
		columns[colTxBytes] = counters.TxBytes
		columns[colTxPackets] = counters.TxPackets
		columns[colTxErrors] = counters.TxErrors
		columns[colTxDropped] = counters.TxDropped
		out += devLine(name, columns)
	}
	return out
}

// procFSWith builds a procfs from /proc-relative paths.
func procFSWith(files map[string]string) fstest.MapFS {
	out := make(fstest.MapFS, len(files))
	for name, content := range files {
		out[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return out
}

// stepClock advances by a fixed step on every reading, so a test can tell a
// replayed sample from a freshly taken one by its timestamp alone.
type stepClock struct {
	mu   sync.Mutex
	at   time.Time
	step time.Duration
}

func newStepClock(step time.Duration) *stepClock {
	return &stepClock{at: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC), step: step}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(c.step)
	return c.at
}

// newTestCollector builds a collector over injected filesystems and a stepping
// clock.
func newTestCollector(procFiles, sysFiles map[string]string) (*Collector, *stepClock) {
	clock := newStepClock(time.Second)
	c := New(procFSWith(procFiles), sysFSWith(sysFiles))
	c.now = clock.Now
	return c, clock
}

// twoNICProc is the fixture most capture tests use.
func twoNICProc() map[string]string {
	return map[string]string{
		"net/dev": devFile(map[string]devCounters{
			"lo":   {RxBytes: 999, TxBytes: 999},
			"eth0": {RxBytes: 1000, TxBytes: 2000, RxPackets: 10, TxPackets: 20},
		}),
		"net/sockstat":  "sockets: used 40\nTCP: inuse 12 orphan 1 tw 57 alloc 20 mem 4\n",
		"net/sockstat6": "TCP6: inuse 3\n",
	}
}

func twoNICSys() map[string]string {
	return map[string]string{
		linkAttrPath("eth0", "speed"): "1000\n",
		linkAttrPath("eth0", "mtu"):   "1500\n",
	}
}

// TestCollectorName pins the section name. The package is netstats but the
// section is "network", because the run coordinator's registry names sections
// after what they describe.
func TestCollectorName(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	if got := c.Name(); got != "network" {
		t.Fatalf("Name() = %q, want %q", got, "network")
	}
}

// TestCaptureReadsAllSources checks that one capture fills the socket summary,
// the interface counters and the link attributes.
func TestCaptureReadsAllSources(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v", err)
	}
	s, ok := res.Handle.Sample().(*sample)
	if !ok {
		t.Fatalf("Sample() = %T, want *sample", res.Handle.Sample())
	}
	want := TCPSummary{InUse: 12, TimeWait: 57, Orphan: 1, InUse6: 3}
	if s.TCP != want {
		t.Fatalf("TCP = %+v, want %+v", s.TCP, want)
	}
	if len(s.Devs) != 1 {
		t.Fatalf("devices = %v, want eth0 only (loopback excluded)", s.Devs)
	}
	if s.Link["eth0"].SpeedMbit != 1000 || s.Link["eth0"].MTU != 1500 {
		t.Fatalf("link attrs = %+v", s.Link["eth0"])
	}
	if len(s.Notes) != 0 {
		t.Fatalf("notes = %v, want none", s.Notes)
	}
	if res.Handle.Phase != runctl.PhaseStartBaseline || res.Handle.Collector != "network" {
		t.Fatalf("handle = %+v", res.Handle)
	}
	if !res.At.Equal(res.Handle.SampledAt) {
		t.Fatalf("At = %v, SampledAt = %v; want equal", res.At, res.Handle.SampledAt)
	}
}

// TestNetstatsCapture_Idempotent checks that a retried boundary replays the
// first sample down to its timestamp. Sampling twice would move the boundary
// under a caller that only meant to retry.
func TestNetstatsCapture_Idempotent(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	first, err := c.CaptureBaseline(context.Background(), "run-1", 7)
	if err != nil {
		t.Fatalf("first CaptureBaseline() error = %v", err)
	}
	second, err := c.CaptureBaseline(context.Background(), "run-1", 7)
	if err != nil {
		t.Fatalf("second CaptureBaseline() error = %v", err)
	}
	if !first.At.Equal(second.At) {
		t.Fatalf("At = %v then %v; want the same measured moment", first.At, second.At)
	}
	if first.Handle.Sample() != second.Handle.Sample() {
		t.Fatal("the replayed handle carries a different sample")
	}
	if !second.Committed {
		t.Fatal("Committed = false on replay; it is a state predicate, not a did-I-do-it flag")
	}

	// The final boundary is a different phase and must be sampled separately.
	final, err := c.CaptureFinal(context.Background(), "run-1", 7)
	if err != nil {
		t.Fatalf("CaptureFinal() error = %v", err)
	}
	if final.At.Equal(first.At) {
		t.Fatal("CaptureFinal replayed the opening sample")
	}
	if final.Handle.Phase != runctl.PhaseFinishFinal {
		t.Fatalf("final phase = %q", final.Handle.Phase)
	}
}

// TestNetstatsCapture_Committed fixes the two legal outcomes: success commits,
// and a cancelled context fails with a measured moment rather than a zero
// value the coordinator cannot record.
func TestNetstatsCapture_Committed(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	ok, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil || !ok.Committed {
		t.Fatalf("CaptureBaseline() = %+v, error = %v; want a committed sample", ok, err)
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := c.CaptureFinal(expired, "run-1", 1)
	if err == nil {
		t.Fatal("CaptureFinal() with a cancelled context error = nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
	if res.Committed {
		t.Fatal("Committed = true for a sample that was never taken")
	}
	if res.At.IsZero() {
		t.Fatal("At is zero; the coordinator needs the moment even on failure")
	}
}

// TestNetstatsCapture_StaleEpoch checks the fence. A worker from an aborted
// run must not be able to overwrite the current run's sample.
func TestNetstatsCapture_StaleEpoch(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	if _, err := c.CaptureBaseline(context.Background(), "run-2", 5); err != nil {
		t.Fatalf("CaptureBaseline(epoch 5) error = %v", err)
	}
	res, err := c.CaptureBaseline(context.Background(), "run-1", 4)
	if !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("error = %v, want runctl.ErrStaleEpoch", err)
	}
	if res.Committed {
		t.Fatal("Committed = true for a fenced capture")
	}
	if _, err := c.CaptureFinal(context.Background(), "run-2", 5); err != nil {
		t.Fatalf("the current epoch must still be accepted, got %v", err)
	}
}

// TestCaptureEvictsSupersededEpochs checks that a session of aborted runs does
// not accumulate frozen samples nobody will ever collect.
func TestCaptureEvictsSupersededEpochs(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline(epoch 1) error = %v", err)
	}
	if _, err := c.CaptureBaseline(context.Background(), "run-2", 2); err != nil {
		t.Fatalf("CaptureBaseline(epoch 2) error = %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.samples) != 1 {
		t.Fatalf("retained samples = %d, want only the current epoch", len(c.samples))
	}
	for key := range c.samples {
		if key.epoch != 2 {
			t.Fatalf("retained epoch %d, want 2", key.epoch)
		}
	}
}

// TestNetstatsRelease_Idempotent checks that releasing twice, or releasing a
// handle this collector never issued, is harmless.
func TestNetstatsRelease_Idempotent(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v", err)
	}
	c.Release(res.Handle)
	c.Release(res.Handle)
	c.Release(runctl.BaselineHandle{})

	c.mu.Lock()
	retained := len(c.samples)
	c.mu.Unlock()
	if retained != 0 {
		t.Fatalf("retained samples = %d after release, want 0", retained)
	}
	// The released handle still carries its sample: Release frees the
	// collector's reference, not the caller's frozen data.
	if _, ok := res.Handle.Sample().(*sample); !ok {
		t.Fatal("the handle lost its sample when the collector released it")
	}
}

// TestNetstatsCaptureDegraded_SysfsUnreadable checks the fail-open rule: link
// attributes are a nicety, interface counters are the point of the section.
func TestNetstatsCaptureDegraded_SysfsUnreadable(t *testing.T) {
	tests := []struct {
		name     string
		sysFS    fs.FS
		wantNote bool
	}{
		{name: "sysfs not injected", sysFS: nil, wantNote: true},
		// An absent attribute is routine on virtual NICs and in namespaces, so
		// it must stay quiet.
		{name: "sysfs empty", sysFS: fstest.MapFS{}, wantNote: false},
		{
			name: "sysfs read fails",
			sysFS: errFS{
				FS:   sysFSWith(twoNICSys()),
				errs: map[string]error{linkAttrPath("eth0", "mtu"): fs.ErrPermission},
			},
			wantNote: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(procFSWith(twoNICProc()), tt.sysFS)
			res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
			if err != nil {
				t.Fatalf("CaptureBaseline() error = %v, want a degraded success", err)
			}
			s := res.Handle.Sample().(*sample)
			if s.Devs["eth0"].RxBytes != 1000 {
				t.Fatalf("devices = %v, want the procfs counters to survive", s.Devs)
			}
			if !tt.wantNote {
				if len(s.Notes) != 0 {
					t.Fatalf("notes = %v, want none", s.Notes)
				}
				return
			}
			if len(s.Notes) != 1 || s.Notes[0].Key != HealthSysfsUnreadable {
				t.Fatalf("notes = %v, want one %s", s.Notes, HealthSysfsUnreadable)
			}
		})
	}
}

// TestCaptureFailsWithoutNetDev checks the one fatal source: without the
// interface table there is nothing to report.
func TestCaptureFailsWithoutNetDev(t *testing.T) {
	tests := []struct {
		name   string
		procFS fs.FS
	}{
		{name: "procfs not injected", procFS: nil},
		{name: "net/dev missing", procFS: fstest.MapFS{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(tt.procFS, sysFSWith(twoNICSys()))
			res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
			if err == nil {
				t.Fatal("CaptureBaseline() error = nil, want a failure")
			}
			if res.Committed {
				t.Fatal("Committed = true without an interface table")
			}
			if res.At.IsZero() {
				t.Fatal("At is zero on failure")
			}
		})
	}
}

// TestCaptureDegradedSockstat checks that a broken socket summary costs the
// summary only, and that a missing IPv6 file stays quiet because hosts with
// IPv6 disabled simply do not have one.
func TestCaptureDegradedSockstat(t *testing.T) {
	tests := []struct {
		name      string
		sockstat  string
		sockstat6 string
		omit      []string
		wantNotes int
	}{
		{name: "healthy", sockstat: "TCP: inuse 1\n", sockstat6: "TCP6: inuse 2\n"},
		{name: "sockstat6 missing", sockstat: "TCP: inuse 1\n", omit: []string{"net/sockstat6"}},
		{name: "sockstat missing", sockstat6: "TCP6: inuse 2\n", omit: []string{"net/sockstat"}, wantNotes: 0},
		{name: "sockstat unparseable", sockstat: "nothing here\n", sockstat6: "TCP6: inuse 2\n", wantNotes: 1},
		{name: "sockstat6 unparseable", sockstat: "TCP: inuse 1\n", sockstat6: "garbage\n", wantNotes: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := twoNICProc()
			files["net/sockstat"] = tt.sockstat
			files["net/sockstat6"] = tt.sockstat6
			for _, name := range tt.omit {
				delete(files, name)
			}
			c := New(procFSWith(files), sysFSWith(twoNICSys()))
			res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
			if err != nil {
				t.Fatalf("CaptureBaseline() error = %v, want a degraded success", err)
			}
			s := res.Handle.Sample().(*sample)
			if len(s.Notes) != tt.wantNotes {
				t.Fatalf("notes = %v, want %d", s.Notes, tt.wantNotes)
			}
			for _, note := range s.Notes {
				if note.Key != HealthProcUnreadable {
					t.Fatalf("note key = %q, want %q", note.Key, HealthProcUnreadable)
				}
			}
		})
	}
}

// TestCaptureReportsUnreadableSockstat checks the read-error path, which is
// distinct from an absent file: a sockstat we are not allowed to read is worth
// saying out loud.
func TestCaptureReportsUnreadableSockstat(t *testing.T) {
	for _, name := range []string{"net/sockstat", "net/sockstat6"} {
		t.Run(name, func(t *testing.T) {
			procFS := errFS{
				FS:   procFSWith(twoNICProc()),
				errs: map[string]error{name: fs.ErrPermission},
			}
			c := New(procFS, sysFSWith(twoNICSys()))
			res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
			if err != nil {
				t.Fatalf("CaptureBaseline() error = %v, want a degraded success", err)
			}
			s := res.Handle.Sample().(*sample)
			if len(s.Notes) != 1 || s.Notes[0].Key != HealthProcUnreadable {
				t.Fatalf("notes = %v, want one %s", s.Notes, HealthProcUnreadable)
			}
			if !strings.Contains(s.Notes[0].Detail, name) {
				t.Fatalf("detail = %q, want it to name %s", s.Notes[0].Detail, name)
			}
		})
	}
}

// TestStoreFencesRacingEpochs drives the window between reading a sample and
// publishing it. The read happens outside the lock, so a run that started
// while we were reading must still win.
func TestStoreFencesRacingEpochs(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	key := cacheKey{runID: "run-1", epoch: 3, phase: runctl.PhaseStartBaseline}
	result := runctl.SampleResult{At: time.Now(), Committed: true}

	c.mu.Lock()
	c.epoch = 9
	c.mu.Unlock()
	if _, err := c.store(key, 3, result); !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("store() error = %v, want runctl.ErrStaleEpoch", err)
	}

	// A second publication of the same boundary replays the first, so two
	// racing callers cannot disagree about when the boundary was taken.
	current := cacheKey{runID: "run-2", epoch: 9, phase: runctl.PhaseStartBaseline}
	first := runctl.SampleResult{At: time.Now(), Committed: true}
	if _, err := c.store(current, 9, first); err != nil {
		t.Fatalf("store() error = %v", err)
	}
	second, err := c.store(current, 9, runctl.SampleResult{At: time.Now().Add(time.Hour), Committed: true})
	if err != nil {
		t.Fatalf("store() error = %v", err)
	}
	if !second.At.Equal(first.At) {
		t.Fatalf("At = %v, want the first publication %v", second.At, first.At)
	}
}

// TestCaptureReportsMalformedNetDevRows checks that a corrupt row is reported
// while every healthy interface still reaches the table.
func TestCaptureReportsMalformedNetDevRows(t *testing.T) {
	files := twoNICProc()
	files["net/dev"] += "  eth1: 1 2 3\n"
	c := New(procFSWith(files), sysFSWith(twoNICSys()))
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v", err)
	}
	s := res.Handle.Sample().(*sample)
	if _, ok := s.Devs["eth0"]; !ok || len(s.Devs) != 1 {
		t.Fatalf("devices = %v, want eth0 only", s.Devs)
	}
	if len(s.Notes) != 1 || s.Notes[0].Key != HealthProcUnreadable {
		t.Fatalf("notes = %v, want one %s", s.Notes, HealthProcUnreadable)
	}
	if !strings.Contains(s.Notes[0].Detail, "eth1") {
		t.Fatalf("detail = %q, want it to name the offending row", s.Notes[0].Detail)
	}
}

// TestCaptureIsConcurrencySafe runs the boundary from several goroutines at
// once: the coordinator samples baseline collectors in parallel, and a retry
// racing the first call must still yield one sample.
func TestCaptureIsConcurrencySafe(t *testing.T) {
	c, _ := newTestCollector(twoNICProc(), twoNICSys())
	const workers = 8
	var wg sync.WaitGroup
	results := make([]runctl.SampleResult, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = c.CaptureBaseline(context.Background(), "run-1", 3)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: CaptureBaseline() error = %v", i, err)
		}
	}
	for i := 1; i < workers; i++ {
		if !results[i].At.Equal(results[0].At) {
			t.Fatalf("worker %d sampled at %v, worker 0 at %v; want one shared sample",
				i, results[i].At, results[0].At)
		}
	}
}

// TestDefaultCollectorIsUsable checks the package-level instance the library
// registers. It must exist and be named even on a host without procfs.
func TestDefaultCollectorIsUsable(t *testing.T) {
	if Default == nil || Default.Name() != "network" {
		t.Fatalf("Default = %v", Default)
	}
	// No assertion on the capture result: this test also runs on hosts with no
	// /proc, where failing to sample is the correct behaviour.
	if _, err := Default.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Logf("capture on this host failed as expected off Linux: %v", err)
	}
}

// TestHealthKeys enumerates the package's health keys. The list is fixed on
// purpose: a new key per condition would turn a run's health into noise.
func TestHealthKeys(t *testing.T) {
	keys := []string{
		HealthSysfsUnreadable,
		HealthLinkChanged,
		HealthCounterRewind,
		HealthProcUnreadable,
	}
	want := []string{
		"netstats-counter-rewind",
		"netstats-link-changed",
		"netstats-proc-unreadable",
		"netstats-sysfs-unreadable",
	}
	sort.Strings(keys)
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("health keys = %v, want %v", keys, want)
	}
}
