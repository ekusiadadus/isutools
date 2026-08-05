package hoststats

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

func TestHostStatsName(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{}, newClock(fixtureTime))
	if c.Name() != CollectorName {
		t.Fatalf("Name() = %q, want %q", c.Name(), CollectorName)
	}
}

func TestHostStatsNew_UnsupportedOS(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{ProcFS: fstest.MapFS{}}); !errors.Is(err, ErrUnsupportedOS) {
		t.Fatalf("New() error = %v, want ErrUnsupportedOS when procfs has no meminfo", err)
	}
}

func TestHostStatsEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value string
		want  bool
	}{
		{value: "", want: true},
		{value: "on", want: true},
		{value: "1", want: true},
		{value: "off", want: false},
		{value: "OFF", want: false},
		{value: " false ", want: false},
		{value: "0", want: false},
		{value: "no", want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run("value="+tt.value, func(t *testing.T) {
			t.Parallel()
			env := testEnv{EnvEnable: tt.value}
			if got := Enabled(env.get); got != tt.want {
				t.Fatalf("Enabled(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
	if !Enabled(nil) {
		t.Fatal("Enabled(nil) must default to on by reading the process environment")
	}
}

func TestHostStatsCapture_ReadsEverySource(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{EnvRole: "db"}, newClock(fixtureTime))
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil || !res.Committed {
		t.Fatalf("CaptureBaseline() = %+v, %v; want a committed sample", res, err)
	}
	if !res.At.Equal(fixtureTime) || !res.Handle.SampledAt.Equal(fixtureTime) {
		t.Fatalf("At = %v / %v, want %v", res.At, res.Handle.SampledAt, fixtureTime)
	}
	sample := res.Handle.Sample().(*Sample)

	if len(sample.Codes) != 0 {
		t.Fatalf("codes = %v, want none on a complete host", sample.Codes)
	}
	if sample.Phase != runctl.PhaseStartBaseline {
		t.Fatalf("phase = %q, want %q", sample.Phase, runctl.PhaseStartBaseline)
	}
	if sample.Mem.AvailableBytes != 8158474*1024 {
		t.Fatalf("meminfo = %+v, want the converted values", sample.Mem)
	}
	if !sample.HasMajFault || sample.MajFault != 4200 {
		t.Fatalf("pgmajfault = %d (present=%v), want 4200", sample.MajFault, sample.HasMajFault)
	}
	if len(sample.Disks) != 2 {
		t.Fatalf("disks = %v, want the two whole devices", sample.Disks)
	}
	if len(sample.PSI) != 3 || len(sample.FS) != 2 {
		t.Fatalf("psi = %v, fs = %v; want all three resources and both mounts", sample.PSI, sample.FS)
	}
	if sample.CGroup == nil || sample.CGroup.Scope != ScopeAgentCGroup {
		t.Fatalf("cgroup = %+v, want the agent's own cgroup", sample.CGroup)
	}
	if sample.Identity.Hostname != "isu1" || sample.Identity.Role != "db" {
		t.Fatalf("identity = %+v, want the injected hostname and role", sample.Identity)
	}
	if sample.Identity.MachineIDHash == "" || len(sample.Identity.MachineIDHash) != idHashLen {
		t.Fatalf("machine id hash = %q, want %d hex characters", sample.Identity.MachineIDHash, idHashLen)
	}

	clock := newClock(fixtureTime.Add(30 * time.Second))
	c.opt.Now = clock.Now
	final, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil || !final.Committed {
		t.Fatalf("CaptureFinal() = %+v, %v; want a committed sample", final, err)
	}
	if final.Handle.Phase != runctl.PhaseFinishFinal {
		t.Fatalf("phase = %q, want %q", final.Handle.Phase, runctl.PhaseFinishFinal)
	}
	if !final.At.Equal(fixtureTime.Add(30 * time.Second)) {
		t.Fatalf("final At = %v, want the closing boundary", final.At)
	}
}

func TestHostStatsCommittedMatrix(t *testing.T) {
	t.Parallel()
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name          string
		ctx           context.Context
		breakMeminfo  bool
		wantCommitted bool
		wantErr       error
	}{
		{
			name:          "meminfo readable commits even with other sources missing",
			ctx:           context.Background(),
			wantCommitted: true,
		},
		{
			name:    "an expired budget commits nothing",
			ctx:     expired,
			wantErr: context.Canceled,
		},
		{
			name:         "an unreadable meminfo has no sample at all",
			ctx:          context.Background(),
			breakMeminfo: true,
			wantErr:      ErrNoSource,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestCollector(t, testEnv{}, newClock(fixtureTime))
			if tt.breakMeminfo {
				c.opt.ProcFS = fstest.MapFS{}
			}
			res, err := c.CaptureBaseline(tt.ctx, "run-1", 1)
			if res.Committed != tt.wantCommitted {
				t.Fatalf("Committed = %v, want %v", res.Committed, tt.wantCommitted)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if res.At.IsZero() {
				t.Fatal("a failed sample must still report when it was attempted")
			}
			if !res.Handle.Zero() {
				t.Fatal("an uncommitted sample must not hand out a handle")
			}
		})
	}
}

func TestHostStatsCapture_Idempotent(t *testing.T) {
	t.Parallel()
	clock := newClock(fixtureTime)
	c := newTestCollector(t, testEnv{}, clock)

	first, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v, want nil", err)
	}
	// Moving the clock proves the retry replays the original boundary instead
	// of taking a new one: a re-sampled boundary would silently move the point
	// the whole run is measured against.
	clock.set(fixtureTime.Add(time.Minute))

	second, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() retry error = %v, want nil", err)
	}
	if !second.At.Equal(first.At) || !second.Handle.SampledAt.Equal(first.Handle.SampledAt) {
		t.Fatalf("retry At = %v, want %v", second.At, first.At)
	}
	if second.Handle.Sample() != first.Handle.Sample() {
		t.Fatal("retry must replay the same frozen sample")
	}
	if !second.Committed {
		t.Fatal("retry Committed = false, want true: the sample is fixed for this run")
	}

	// A different phase of the same run is a different boundary.
	final, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal() error = %v, want nil", err)
	}
	if final.At.Equal(first.At) {
		t.Fatal("the closing boundary must be its own sample")
	}
}

func TestHostStatsCapture_StaleEpoch(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{}, newClock(fixtureTime))

	if _, err := c.CaptureBaseline(context.Background(), "run-2", 2); err != nil {
		t.Fatalf("CaptureBaseline(epoch 2) error = %v, want nil", err)
	}
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if !errors.Is(err, runctl.ErrStaleEpoch) {
		t.Fatalf("error = %v, want runctl.ErrStaleEpoch", err)
	}
	if res.Committed {
		t.Fatal("a fenced call must not report a committed sample")
	}
}

func TestHostStatsCapture_NewEpochPrunesOldSamples(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{}, newClock(fixtureTime))
	if _, err := c.CaptureBaseline(context.Background(), "run-1", 1); err != nil {
		t.Fatalf("CaptureBaseline(epoch 1) error = %v, want nil", err)
	}
	if _, err := c.CaptureBaseline(context.Background(), "run-2", 2); err != nil {
		t.Fatalf("CaptureBaseline(epoch 2) error = %v, want nil", err)
	}
	if len(c.samples) != 1 || len(c.results) != 1 {
		t.Fatalf("retained %d samples and %d results, want only the current epoch", len(c.samples), len(c.results))
	}
}

func TestHostStatsRelease_Idempotent(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{}, newClock(fixtureTime))
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v, want nil", err)
	}

	c.Release(res.Handle)
	c.Release(res.Handle)
	c.Release(runctl.BaselineHandle{})
	c.Release(runctl.NewBaselineHandle("run-1", 1, "someone-else", runctl.PhaseStartBaseline, fixtureTime, &Sample{}))

	if len(c.samples) != 0 || len(c.results) != 0 {
		t.Fatalf("after Release: %d samples, %d results; want none", len(c.samples), len(c.results))
	}
	// The handle keeps its own copy, so a released boundary can still be
	// collected.
	if _, ok := res.Handle.Sample().(*Sample); !ok {
		t.Fatal("Release must not empty the handle it was given")
	}
}

func TestHostStatsBudget_PartialSourcesCommitted(t *testing.T) {
	t.Parallel()
	clock := newClock(fixtureTime)
	c := newTestCollector(t, testEnv{}, clock)

	// The budget expires the instant sampling starts: the required source is
	// already in hand, every optional source is not.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.hook(cancel)

	res, err := c.CaptureBaseline(ctx, "run-1", 1)
	if err != nil || !res.Committed {
		t.Fatalf("CaptureBaseline() = %+v, %v; want a committed partial sample", res, err)
	}
	sample := res.Handle.Sample().(*Sample)
	if sample.Mem.TotalBytes == 0 {
		t.Fatal("the required source must be captured before the budget is checked")
	}
	for _, source := range []string{SourceVMStat, SourceDiskstats, SourcePSI, SourceStatfs, SourceCGroup} {
		if !containsString(sample.Codes, CodeNotCapturedPrefix+source) {
			t.Fatalf("codes = %v, want %s recorded as skipped", sample.Codes, source)
		}
	}
}

// TestHostStatsCapture_ConcurrentBoundariesShareOneSample fixes both halves of
// the locking rule: a slow source must not serialise other callers behind the
// collector's mutex, and two callers that sample the same boundary at once
// must still end up with one frozen sample rather than two.
func TestHostStatsCapture_ConcurrentBoundariesShareOneSample(t *testing.T) {
	t.Parallel()
	const callers = 2
	entered := make(chan struct{}, callers)
	release := make(chan struct{})

	opt := testOptions(testEnv{}, newClock(fixtureTime))
	opt.DataDir = ""
	opt.Statfs = func(string) (FSRaw, error) {
		entered <- struct{}{}
		<-release
		return FSRaw{TotalBytes: 100, AvailBytes: 50}, nil
	}
	c, err := New(opt)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	results := make([]runctl.SampleResult, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.CaptureBaseline(context.Background(), "run-1", 1)
		}(i)
	}
	// Every caller has to reach the sampling seam. Under a lock held across
	// sampling only the first one could, and this would time out.
	for i := 0; i < callers; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("a second boundary could not start sampling: the collector holds its lock across reads")
		}
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil || !results[i].Committed {
			t.Fatalf("caller %d = %+v, %v; want a committed sample", i, results[i], err)
		}
	}
	if !results[0].At.Equal(results[1].At) {
		t.Fatalf("At = %v and %v: two callers moved the boundary apart", results[0].At, results[1].At)
	}
	if results[0].Handle.Sample() != results[1].Handle.Sample() {
		t.Fatal("the two callers were handed different samples for one boundary")
	}
	if len(c.samples) != 1 || len(c.results) != 1 {
		t.Fatalf("retained %d samples and %d results, want exactly one boundary", len(c.samples), len(c.results))
	}
}

// TestHostStatsCapture_EpochBumpedWhileSamplingIsFenced covers the other
// consequence of sampling outside the lock: a newer epoch can arrive while a
// boundary is still reading. Publishing that reading afterwards would
// resurrect a run the controller has already displaced, so it is fenced
// exactly like a late call.
func TestHostStatsCapture_EpochBumpedWhileSamplingIsFenced(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32

	opt := testOptions(testEnv{}, newClock(fixtureTime))
	opt.DataDir = ""
	opt.Statfs = func(string) (FSRaw, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return FSRaw{TotalBytes: 100, AvailBytes: 50}, nil
	}
	c, err := New(opt)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	type outcome struct {
		res runctl.SampleResult
		err error
	}
	old := make(chan outcome, 1)
	go func() {
		res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
		old <- outcome{res: res, err: err}
	}()

	<-entered
	if _, err := c.CaptureBaseline(context.Background(), "run-2", 2); err != nil {
		t.Fatalf("CaptureBaseline(epoch 2) error = %v, want nil", err)
	}
	close(release)

	got := <-old
	if !errors.Is(got.err, runctl.ErrStaleEpoch) {
		t.Fatalf("error = %v, want runctl.ErrStaleEpoch for a sample overtaken mid-read", got.err)
	}
	if got.res.Committed || !got.res.Handle.Zero() {
		t.Fatalf("result = %+v, want nothing published for a fenced epoch", got.res)
	}
	if got.res.At.IsZero() {
		t.Fatal("a fenced sample must still report when it was attempted")
	}
	if len(c.samples) != 1 || len(c.results) != 1 {
		t.Fatalf("retained %d samples and %d results, want only the current epoch", len(c.samples), len(c.results))
	}
}

// TestHostStatsWedgedStatfs_SkipsOnlyThatSource fixes the behaviour a wedged
// NFS or fuse mount must produce. statfs(2) cannot be cancelled, so the
// boundary has to be able to walk away from it: it must return inside the
// caller's deadline, commit everything else it read, and degrade the
// filesystem source to its own code — never hold the boundary, and never leave
// the abandoned call running once the mount answers again.
func TestHostStatsWedgedStatfs_SkipsOnlyThatSource(t *testing.T) {
	t.Parallel()
	// released unwedges the mount; returned proves the abandoned goroutine ran
	// to completion, which is what a leak check needs instead of a sleep.
	released := make(chan struct{})
	returned := make(chan struct{})

	opt := testOptions(testEnv{}, newClock(fixtureTime))
	// A single statfs target, so the seam is entered exactly once and
	// "returned" describes the whole abandoned read.
	opt.DataDir = ""
	opt.Statfs = func(path string) (FSRaw, error) {
		<-released
		close(returned)
		return FSRaw{}, errors.New("statfs " + path + ": mount is wedged")
	}
	c, err := New(opt)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	res, err := c.CaptureBaseline(ctx, "run-1", 1)
	elapsed := time.Since(started)

	if err != nil || !res.Committed {
		t.Fatalf("CaptureBaseline() = %+v, %v; want a committed partial sample", res, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the boundary took %v: it waited for the wedged mount instead of its deadline", elapsed)
	}
	sample := res.Handle.Sample().(*Sample)
	if len(sample.FS) != 0 {
		t.Fatalf("fs = %v, want nothing from a mount that never answered", sample.FS)
	}
	if !containsString(sample.Codes, CodeNotCapturedPrefix+SourceStatfs) {
		t.Fatalf("codes = %v, want the filesystem source recorded as skipped", sample.Codes)
	}
	if len(sample.Codes) != 1 {
		t.Fatalf("codes = %v, want a wedged mount to cost only its own source", sample.Codes)
	}
	if sample.CGroup == nil || sample.Mem.TotalBytes == 0 {
		t.Fatalf("sample = %+v, want every other source captured", sample)
	}

	section := collectSection(t, c, sample, sample)
	if !section.Partial || !containsString(section.Codes, CodeNotCapturedPrefix+SourceStatfs) {
		t.Fatalf("section = partial %v, codes %v; want the skip reported per metric", section.Partial, section.Codes)
	}
	if len(section.Filesystems) != 0 {
		t.Fatalf("filesystems = %v, want none", section.Filesystems)
	}

	// The abandoned read must finish and exit once the mount answers: it
	// delivers into a buffered channel, so returning from the seam is its last
	// blocking point and nothing of it survives into the next test.
	close(released)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned statfs never finished: the boundary leaked its goroutine")
	}
}

func TestHostStatsPSIAbsent_SkipsPSIOnly(t *testing.T) {
	t.Parallel()
	opt := testOptions(testEnv{}, newClock(fixtureTime))
	procFS := newProcFS()
	for _, resource := range psiResources {
		delete(procFS, "pressure/"+resource)
	}
	opt.ProcFS = procFS

	c, err := New(opt)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil || !res.Committed {
		t.Fatalf("CaptureBaseline() = %+v, %v; want a committed sample", res, err)
	}
	sample := res.Handle.Sample().(*Sample)
	if len(sample.PSI) != 0 {
		t.Fatalf("psi = %v, want none on a kernel without PSI", sample.PSI)
	}
	if !containsString(sample.Codes, CodeNotCapturedPrefix+SourcePSI) {
		t.Fatalf("codes = %v, want the PSI skip recorded", sample.Codes)
	}
	if len(sample.Codes) != 1 {
		t.Fatalf("codes = %v, want only PSI skipped", sample.Codes)
	}
	section := collectSection(t, c, sample, sample)
	if section.PSI != nil {
		t.Fatal("a section built from PSI-less samples must omit PSI")
	}
	if !section.Partial {
		t.Fatal("a skipped source makes the section partial")
	}
}

func TestHostStatsSample_DeepCopyIsolation(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{}, newClock(fixtureTime))
	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v, want nil", err)
	}
	handleSample := res.Handle.Sample().(*Sample)
	key := runKey{RunID: "run-1", Epoch: 1, Phase: runctl.PhaseStartBaseline}
	stored := c.samples[key]
	if stored == handleSample {
		t.Fatal("the handle must carry its own copy, not the collector's sample")
	}

	// The collector's copy must not be reachable through the handle.
	stored.Disks["sda"] = DiskRaw{ReadSectors: 999999}
	stored.Codes = append(stored.Codes, "mutated")
	stored.CGroup.MemoryCurrentBytes = 1
	if handleSample.Disks["sda"].ReadSectors == 999999 {
		t.Fatal("mutating the collector's sample changed the frozen one")
	}
	if containsString(handleSample.Codes, "mutated") {
		t.Fatal("codes are shared between the collector and the handle")
	}
	if handleSample.CGroup.MemoryCurrentBytes == 1 {
		t.Fatal("the cgroup reading is shared between the collector and the handle")
	}

	// And the reverse: a caller poking at the handle cannot rewrite what the
	// collector kept.
	handleSample.Disks["nvme0n1"] = DiskRaw{ReadSectors: 7}
	if stored.Disks["nvme0n1"].ReadSectors == 7 {
		t.Fatal("the collector's sample is reachable from the handle")
	}
}

func TestSampleCloneHandlesEmptyValues(t *testing.T) {
	t.Parallel()
	var nilSample *Sample
	if nilSample.clone() != nil {
		t.Fatal("cloning nothing must produce nothing")
	}
	empty := (&Sample{}).clone()
	if empty.Disks != nil || empty.PSI != nil || empty.FS != nil || empty.Codes != nil || empty.CGroup != nil {
		t.Fatalf("clone of an empty sample = %+v, want empty fields preserved", empty)
	}
	limits := &CGroupRaw{CPUMaxCores: ratePtr(2), MemoryMaxBytes: new(uint64)}
	cloned := limits.clone()
	if cloned.CPUMaxCores == limits.CPUMaxCores || cloned.MemoryMaxBytes == limits.MemoryMaxBytes {
		t.Fatal("cgroup limits must be copied, not aliased")
	}
}

func TestHostStatsCapture_UnparsableMeminfo(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{}, newClock(fixtureTime))
	c.opt.ProcFS = fstest.MapFS{pathMeminfo: mapFile("MemFree: 1 kB\n")}

	res, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if !errors.Is(err, ErrNoSource) {
		t.Fatalf("error = %v, want ErrNoSource", err)
	}
	if res.Committed {
		t.Fatal("Committed = true, want false when the required source is unusable")
	}
}

func TestHostStatsNew_RejectsNonLinuxWithoutInjection(t *testing.T) {
	t.Parallel()
	_, err := New(Options{})
	if runtime.GOOS == "linux" {
		// On Linux the zero value is the production configuration, so the
		// only failure mode left is a host without /proc.
		if err != nil && !errors.Is(err, ErrUnsupportedOS) {
			t.Fatalf("New() error = %v, want nil or ErrUnsupportedOS", err)
		}
		return
	}
	if !errors.Is(err, ErrUnsupportedOS) {
		t.Fatalf("New() error = %v, want ErrUnsupportedOS off Linux", err)
	}
}
