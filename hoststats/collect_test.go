package hoststats

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// interval builds a base/final pair ten seconds apart with the closing sample
// already advanced by the given counters.
func intervalSamples(seconds time.Duration) (*Sample, *Sample) {
	base := newSample(runctl.PhaseStartBaseline, fixtureTime)
	final := newSample(runctl.PhaseFinishFinal, fixtureTime.Add(seconds))
	final.Disks["sda"] = DiskRaw{
		ReadSectors:  1000 + 20000,
		WriteSectors: 2000 + 40000,
		IOTicksMS:    500 + 5000,
		WeightedMS:   800 + 10000,
	}
	final.MajFault = 1000 + 250
	final.Mem.AvailableBytes = 6 << 30
	final.PSI["cpu"] = PSIRaw{SomeAvg10: 9, SomeAvg60: 8, SomeTotalUS: 3_000_000, HasFull: true, FullAvg10: 7, FullTotalUS: 1_000_000}
	final.PSI["memory"] = PSIRaw{SomeAvg10: 4, SomeTotalUS: 3_000_000, FullAvg10: 2, FullTotalUS: 750_000, HasFull: true}
	final.PSI["io"] = PSIRaw{SomeAvg10: 6, SomeTotalUS: 6_000_000, HasFull: true, FullTotalUS: 2_000_000}
	final.FS = map[string]FSRaw{
		"/":              {TotalBytes: 100, AvailBytes: 50},
		"/var/lib/mysql": {TotalBytes: 200, AvailBytes: 20},
	}
	return base, final
}

func buildFrom(t *testing.T, base, final *Sample) *Section {
	t.Helper()
	return buildSection(base.At, final.At, base, final)
}

func TestHostStatsCollect_IntervalValues(t *testing.T) {
	t.Parallel()
	base, final := intervalSamples(10 * time.Second)
	section := buildFrom(t, base, final)

	if section.Partial || len(section.Codes) != 0 {
		t.Fatalf("section = partial %v, codes %v; want a clean interval", section.Partial, section.Codes)
	}
	if !nearlyEqual(section.Interval.Seconds, 10) {
		t.Fatalf("interval = %v, want 10s", section.Interval.Seconds)
	}
	if section.Memory.PageMajorFaults != 250 {
		t.Fatalf("major faults = %d, want the interval delta 250", section.Memory.PageMajorFaults)
	}
	if section.Memory.AvailableBaseline != 8<<30 || section.Memory.AvailableFinal != 6<<30 {
		t.Fatalf("memory = %+v, want both boundaries reported", section.Memory)
	}

	if len(section.Disks) != 1 {
		t.Fatalf("disks = %+v, want one device", section.Disks)
	}
	disk := section.Disks[0]
	if disk.ReadBytes != 20000*sectorSize || disk.WriteBytes != 40000*sectorSize {
		t.Fatalf("disk bytes = %+v, want sectors converted at 512 bytes", disk)
	}
	if disk.IOTimeMillis != 5000 {
		t.Fatalf("io time = %d, want 5000", disk.IOTimeMillis)
	}
	if got := floatValue(t, "read MB/s", disk.ReadMBPerSec); !nearlyEqual(got, 1.024) {
		t.Fatalf("read MB/s = %v, want 1.024", got)
	}
	if got := floatValue(t, "write MB/s", disk.WriteMBPerSec); !nearlyEqual(got, 2.048) {
		t.Fatalf("write MB/s = %v, want 2.048", got)
	}
	if got := floatValue(t, "util%", disk.UtilPercent); !nearlyEqual(got, 50) {
		t.Fatalf("util%% = %v, want 50", got)
	}
	if got := floatValue(t, "queue", disk.QueueAvg); !nearlyEqual(got, 1) {
		t.Fatalf("queue = %v, want 1", got)
	}

	if section.PSI == nil {
		t.Fatal("psi = nil, want the three resources")
	}
	if got := floatValue(t, "cpu some stall", section.PSI.CPU.SomeStallRatio); !nearlyEqual(got, 0.2) {
		t.Fatalf("cpu some stall = %v, want 0.2", got)
	}
	if section.PSI.CPU.FullAvg10 != 0 || section.PSI.CPU.FullStallRatio != nil {
		t.Fatalf("cpu full = %+v, want it omitted: system-level cpu full is zero by definition", section.PSI.CPU)
	}
	if got := floatValue(t, "memory full stall", section.PSI.Memory.FullStallRatio); !nearlyEqual(got, 0.025) {
		t.Fatalf("memory full stall = %v, want 0.025", got)
	}
	if section.PSI.IO.SomeAvg10 != 6 {
		t.Fatalf("io avg10 = %v, want the closing boundary's value", section.PSI.IO.SomeAvg10)
	}

	if len(section.Filesystems) != 2 || section.Filesystems[0].Path != "/" {
		t.Fatalf("filesystems = %+v, want both mounts sorted by path", section.Filesystems)
	}
	if section.Filesystems[0].AvailBaseline != 60 || section.Filesystems[0].AvailFinal != 50 {
		t.Fatalf("root filesystem = %+v, want both boundaries", section.Filesystems[0])
	}
	if section.Filesystems[1].AvailBaseline != 0 || section.Filesystems[1].AvailFinal != 20 {
		t.Fatalf("data filesystem = %+v, want the boundary it was seen at", section.Filesystems[1])
	}
	if section.Identity.Hostname != "isu1" {
		t.Fatalf("identity = %+v, want the opening boundary's identity", section.Identity)
	}
}

func TestHostStatsZeroInterval_NilRates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		duration time.Duration
	}{
		{name: "identical boundaries", duration: 0},
		{name: "below a millisecond", duration: 999 * time.Microsecond},
		{name: "boundaries out of order", duration: -time.Second},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base, final := intervalSamples(tt.duration)
			section := buildFrom(t, base, final)

			disk := section.Disks[0]
			if disk.ReadMBPerSec != nil || disk.WriteMBPerSec != nil || disk.UtilPercent != nil || disk.QueueAvg != nil {
				t.Fatalf("disk = %+v, want every rate nil on an unusable interval", disk)
			}
			if section.PSI.CPU.SomeStallRatio != nil || section.PSI.Memory.FullStallRatio != nil {
				t.Fatalf("psi = %+v, want stall ratios nil", section.PSI)
			}
			// Deltas are still derivable; only the division is not.
			if disk.ReadBytes != 20000*sectorSize || section.Memory.PageMajorFaults != 250 {
				t.Fatalf("section = %+v, want the deltas kept", section)
			}
		})
	}
}

func TestHostStatsCounterRewind_PartialNotDropped(t *testing.T) {
	t.Parallel()
	base, final := intervalSamples(10 * time.Second)
	final.Disks["sda"] = DiskRaw{ReadSectors: 1, WriteSectors: 1, IOTicksMS: 1, WeightedMS: 1}
	final.MajFault = 5

	section := buildFrom(t, base, final)

	if !section.Partial {
		t.Fatal("a rewound counter makes the section partial")
	}
	if !containsString(section.Codes, CodeCounterRewindPrefix+"sda") {
		t.Fatalf("codes = %v, want the device named", section.Codes)
	}
	if !containsString(section.Codes, CodeCounterRewindPrefix+SourceVMStat) {
		t.Fatalf("codes = %v, want the vmstat rewind recorded", section.Codes)
	}
	disk := section.Disks[0]
	if disk.Code != CodeCounterRewind {
		t.Fatalf("disk code = %q, want %q", disk.Code, CodeCounterRewind)
	}
	if disk.ReadBytes != 0 || disk.ReadMBPerSec != nil || disk.QueueAvg != nil {
		t.Fatalf("disk = %+v, want no interval derived from a rewound counter", disk)
	}
	if section.Memory.PageMajorFaults != 0 {
		t.Fatalf("major faults = %d, want none derived", section.Memory.PageMajorFaults)
	}
	// The point observations are exactly what a rewind does not invalidate,
	// and they are the reason Collect does not fail the section.
	if section.Memory.AvailableFinal == 0 || section.CGroup == nil || section.Identity.Hostname == "" {
		t.Fatalf("section = %+v, want the point observations kept", section)
	}
	if !hasHealthKey(section.HealthNotes(), HealthCounterRewind) {
		t.Fatalf("health = %+v, want %s", section.HealthNotes(), HealthCounterRewind)
	}
}

func TestHostStatsBootIDChanged_DropsDeltas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		mutate   func(*Sample)
		wantCode string
	}{
		{
			name:     "reboot",
			mutate:   func(s *Sample) { s.Identity.BootIDHash = "9999888877776666" },
			wantCode: CodeBootIDChanged,
		},
		{
			name:     "different machine",
			mutate:   func(s *Sample) { s.Identity.MachineIDHash = "9999888877776666" },
			wantCode: CodeMachineIDChanged,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base, final := intervalSamples(10 * time.Second)
			tt.mutate(final)

			section := buildFrom(t, base, final)

			if !containsString(section.Codes, tt.wantCode) || !section.Partial {
				t.Fatalf("section codes = %v (partial %v), want %s", section.Codes, section.Partial, tt.wantCode)
			}
			disk := section.Disks[0]
			if disk.ReadBytes != 0 || disk.IOTimeMillis != 0 || disk.ReadMBPerSec != nil || disk.UtilPercent != nil {
				t.Fatalf("disk = %+v, want every interval value dropped", disk)
			}
			if section.Memory.PageMajorFaults != 0 {
				t.Fatalf("major faults = %d, want none across a host change", section.Memory.PageMajorFaults)
			}
			if section.PSI.CPU.SomeStallRatio != nil || section.PSI.IO.SomeStallRatio != nil {
				t.Fatalf("psi = %+v, want stall ratios dropped", section.PSI)
			}
			if section.PSI.CPU.SomeAvg10 == 0 || section.Memory.AvailableFinal == 0 {
				t.Fatalf("section = %+v, want the point observations kept", section)
			}
			if !hasHealthKey(section.HealthNotes(), HealthHostChanged) {
				t.Fatalf("health = %+v, want %s", section.HealthNotes(), HealthHostChanged)
			}
		})
	}
}

func TestHostStatsCollect_DeviceChurn(t *testing.T) {
	t.Parallel()
	base, final := intervalSamples(10 * time.Second)
	base.Disks["sdc"] = DiskRaw{ReadSectors: 10}
	final.Disks["sdb"] = DiskRaw{ReadSectors: 500, WriteSectors: 100, IOTicksMS: 10, WeightedMS: 20}

	section := buildFrom(t, base, final)

	if len(section.Disks) != 2 {
		t.Fatalf("disks = %+v, want the two devices present at the close", section.Disks)
	}
	if section.Disks[0].Device != "sda" || section.Disks[1].Device != "sdb" {
		t.Fatalf("disks = %+v, want them sorted by device", section.Disks)
	}
	appeared := section.Disks[1]
	if !appeared.Appeared {
		t.Fatalf("sdb = %+v, want it marked as appeared", appeared)
	}
	if appeared.ReadBytes != 0 || appeared.ReadMBPerSec != nil {
		t.Fatalf("sdb = %+v, want no interval for a device with no baseline", appeared)
	}
	if section.Partial {
		t.Fatal("a device appearing is reported, not a degradation of the section")
	}
}

func TestHostStatsCollect_CGroup(t *testing.T) {
	t.Parallel()
	t.Run("limit changed", func(t *testing.T) {
		t.Parallel()
		base, final := intervalSamples(10 * time.Second)
		base.CGroup = &CGroupRaw{Scope: ScopeConfigured, Path: mysqlCGroupRel, CPUMaxCores: ratePtr(2), MemoryCurrentBytes: 100}
		final.CGroup = &CGroupRaw{Scope: ScopeConfigured, Path: mysqlCGroupRel, CPUMaxCores: ratePtr(4), MemoryCurrentBytes: 200}

		section := buildFrom(t, base, final)
		if section.CGroup == nil {
			t.Fatal("cgroup = nil, want the closing reading")
		}
		if section.CGroup.Code != CodeLimitChanged || !containsString(section.Codes, CodeLimitChanged) {
			t.Fatalf("cgroup = %+v, codes = %v; want the limit change flagged", section.CGroup, section.Codes)
		}
		if got := floatValue(t, "cpu max", section.CGroup.CPUMaxCores); got != 4 {
			t.Fatalf("cpu max = %v, want the closing limit", got)
		}
		if section.CGroup.MemoryCurrentBaseline != 100 || section.CGroup.MemoryCurrentFinal != 200 {
			t.Fatalf("cgroup usage = %+v, want both boundaries", section.CGroup)
		}
		if section.CGroup.CPUMaxCores == final.CGroup.CPUMaxCores {
			t.Fatal("the section must not alias the frozen sample's limit pointer")
		}
	})

	t.Run("unlimited stays unlimited", func(t *testing.T) {
		t.Parallel()
		base, final := intervalSamples(10 * time.Second)
		section := buildFrom(t, base, final)
		if section.CGroup == nil || section.CGroup.Code != "" {
			t.Fatalf("cgroup = %+v, want no limit change between two unlimited readings", section.CGroup)
		}
	})

	t.Run("memory limit change", func(t *testing.T) {
		t.Parallel()
		base, final := intervalSamples(10 * time.Second)
		base.CGroup = &CGroupRaw{MemoryMaxBytes: new(uint64)}
		final.CGroup = &CGroupRaw{}
		section := buildFrom(t, base, final)
		if section.CGroup.Code != CodeLimitChanged {
			t.Fatalf("cgroup = %+v, want a limit removed to count as a change", section.CGroup)
		}
	})

	t.Run("skipped at the close", func(t *testing.T) {
		t.Parallel()
		base, final := intervalSamples(10 * time.Second)
		final.CGroup = nil
		if section := buildFrom(t, base, final); section.CGroup != nil {
			t.Fatalf("cgroup = %+v, want nil without a closing reading", section.CGroup)
		}
	})

	t.Run("no baseline reading", func(t *testing.T) {
		t.Parallel()
		base, final := intervalSamples(10 * time.Second)
		base.CGroup = nil
		section := buildFrom(t, base, final)
		if section.CGroup == nil || section.CGroup.Code != "" || section.CGroup.MemoryCurrentBaseline != 0 {
			t.Fatalf("cgroup = %+v, want the closing reading with no change flagged", section.CGroup)
		}
	})
}

func TestHostStatsCollect_MissingSourcesDegrade(t *testing.T) {
	t.Parallel()
	base, final := intervalSamples(10 * time.Second)
	base.HasMajFault = false
	base.Disks = nil
	final.Disks = nil
	final.PSI = nil
	base.FS = nil
	final.FS = nil
	final.Codes = []string{CodeNotCapturedPrefix + SourcePSI, CodeNotCapturedPrefix + SourceDiskstats}
	base.Codes = []string{CodeNotCapturedPrefix + SourcePSI}

	section := buildFrom(t, base, final)

	if section.Disks != nil || section.PSI != nil || section.Filesystems != nil {
		t.Fatalf("section = %+v, want the missing sources omitted", section)
	}
	if section.Memory.TotalBytes == 0 {
		t.Fatalf("memory = %+v, want the required source still reported", section.Memory)
	}
	if section.Memory.PageMajorFaults != 0 {
		t.Fatalf("major faults = %d, want none without a baseline counter", section.Memory.PageMajorFaults)
	}
	want := []string{CodeNotCapturedPrefix + SourceDiskstats, CodeNotCapturedPrefix + SourcePSI}
	if len(section.Codes) != len(want) {
		t.Fatalf("codes = %v, want deduplicated and sorted %v", section.Codes, want)
	}
	for i, code := range want {
		if section.Codes[i] != code {
			t.Fatalf("codes = %v, want %v", section.Codes, want)
		}
	}
}

func TestHostStatsCollect_MemoryTotalFallsBackToBaseline(t *testing.T) {
	t.Parallel()
	base, final := intervalSamples(10 * time.Second)
	final.Mem.TotalBytes = 0
	if got := buildFrom(t, base, final).Memory.TotalBytes; got != base.Mem.TotalBytes {
		t.Fatalf("total = %d, want the baseline value %d", got, base.Mem.TotalBytes)
	}
}

func TestHostStatsCollect_UsesFrozenSamplesOnly(t *testing.T) {
	t.Parallel()
	clock := newClock(fixtureTime)
	c := newTestCollector(t, testEnv{}, clock)

	base, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v, want nil", err)
	}
	clock.set(fixtureTime.Add(10 * time.Second))
	final, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal() error = %v, want nil", err)
	}

	// Take the whole host away: no procfs, no sysfs, no syscalls, and no
	// bookkeeping left in the collector.
	failing := func(string) (string, error) { return "", fs.ErrPermission }
	c.opt.ProcFS = fstest.MapFS{}
	c.opt.SysFS = nil
	c.opt.EtcFS = nil
	c.opt.CGroupFS = nil
	c.cgroup = cgroupTarget{}
	c.opt.Statfs = func(string) (FSRaw, error) { return FSRaw{}, fs.ErrPermission }
	c.opt.Readlink = failing
	c.opt.EvalSymlinks = failing
	c.opt.Hostname = func() (string, error) { return "", fs.ErrPermission }
	c.opt.Now = func() time.Time {
		t.Error("Collect must not read the clock: the interval is already fixed")
		return time.Time{}
	}
	c.Release(base.Handle)
	c.Release(final.Handle)

	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		t.Fatalf("Collect() error = %v, want nil from frozen samples", err)
	}
	section, ok := value.(*Section)
	if !ok {
		t.Fatalf("Collect() = %T, want *Section", value)
	}
	if section.Memory.TotalBytes == 0 || len(section.Disks) == 0 || section.PSI == nil {
		t.Fatalf("section = %+v, want it built entirely from the handles", section)
	}
	if len(section.Filesystems) != 2 || section.CGroup == nil {
		t.Fatalf("section = %+v, want the statfs and cgroup readings from the samples", section)
	}
	if section.Identity.Hostname != "isu1" {
		t.Fatalf("identity = %+v, want the frozen identity", section.Identity)
	}
	if !section.Interval.BaselineAt.Equal(fixtureTime) || !nearlyEqual(section.Interval.Seconds, 10) {
		t.Fatalf("interval = %+v, want the boundaries the handles carry", section.Interval)
	}
}

func TestHostStatsCollect_TypeMismatch(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{}, newClock(fixtureTime))
	good := handleOf(newSample(runctl.PhaseStartBaseline, fixtureTime), runctl.PhaseStartBaseline, fixtureTime)
	wrong := runctl.NewBaselineHandle("run-1", 1, CollectorName, runctl.PhaseFinishFinal, fixtureTime, "not a sample")

	if _, err := c.Collect(wrong, good); err == nil {
		t.Fatal("Collect() error = nil, want a type mismatch reported rather than a panic")
	}
	if _, err := c.Collect(good, wrong); err == nil {
		t.Fatal("Collect() error = nil, want the closing handle checked too")
	}
	if _, err := c.Collect(runctl.BaselineHandle{}, good); err == nil {
		t.Fatal("Collect() error = nil, want an empty handle rejected")
	}
	var nilSample *Sample
	empty := runctl.NewBaselineHandle("run-1", 1, CollectorName, runctl.PhaseStartBaseline, fixtureTime, nilSample)
	if _, err := c.Collect(empty, good); err == nil {
		t.Fatal("Collect() error = nil, want a nil sample rejected")
	}
}

func TestHostStatsCollect_EndToEnd(t *testing.T) {
	t.Parallel()
	clock := newClock(fixtureTime)
	c := newTestCollector(t, testEnv{EnvCGroupPath: mysqlCGroupRel}, clock)

	base, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline() error = %v, want nil", err)
	}

	// Advance the disk counters the way a benchmark would.
	procFS := newProcFS()
	procFS[pathDiskstats] = mapFile("   8       0 sda 200 0 4000 900 400 0 8000 1800 0 2400 2800 0 0 0 0 0 0\n")
	procFS[pathVMStat] = mapFile("pgmajfault 4300\n")
	c.opt.ProcFS = procFS
	clock.set(fixtureTime.Add(20 * time.Second))

	final, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal() error = %v, want nil", err)
	}

	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	section := value.(*Section)
	if section.Partial {
		t.Fatalf("section codes = %v, want a clean run", section.Codes)
	}
	if section.Memory.PageMajorFaults != 100 {
		t.Fatalf("major faults = %d, want 100", section.Memory.PageMajorFaults)
	}
	if len(section.Disks) != 1 || section.Disks[0].Device != "sda" {
		t.Fatalf("disks = %+v, want only the device present at the close", section.Disks)
	}
	if section.Disks[0].ReadBytes != 2000*sectorSize {
		t.Fatalf("read bytes = %d, want the sector delta converted", section.Disks[0].ReadBytes)
	}
	if section.CGroup == nil || section.CGroup.Scope != ScopeConfigured || section.CGroup.Path != mysqlCGroupRel {
		t.Fatalf("cgroup = %+v, want the configured cgroup", section.CGroup)
	}
	if section.CGroup.MemoryCurrentFinal != 4294967296 {
		t.Fatalf("cgroup usage = %+v, want the configured cgroup's usage", section.CGroup)
	}
}

func TestBuildPSIResourceEdgeCases(t *testing.T) {
	t.Parallel()
	base, final := intervalSamples(10 * time.Second)
	// A full counter that went backwards loses its ratio while the averages,
	// which are point observations, stay.
	final.PSI["memory"] = PSIRaw{SomeAvg10: 4, SomeTotalUS: 3_000_000, FullAvg10: 2, FullTotalUS: 1, HasFull: true}
	// A resource whose closing sample has no full line reports none.
	final.PSI["io"] = PSIRaw{SomeAvg10: 6, SomeTotalUS: 6_000_000}
	// A resource absent from the closing sample is empty rather than wrong.
	delete(final.PSI, "cpu")

	section := buildFrom(t, base, final)
	if section.PSI.Memory.FullStallRatio != nil {
		t.Fatalf("memory = %+v, want no ratio from a rewound counter", section.PSI.Memory)
	}
	if section.PSI.Memory.FullAvg10 != 2 {
		t.Fatalf("memory = %+v, want the averages kept", section.PSI.Memory)
	}
	if section.PSI.IO.FullAvg10 != 0 || section.PSI.IO.FullStallRatio != nil {
		t.Fatalf("io = %+v, want no full values without a full line", section.PSI.IO)
	}
	if section.PSI.CPU != (PSIResource{}) {
		t.Fatalf("cpu = %+v, want it empty when the closing sample has none", section.PSI.CPU)
	}

	// A resource missing from the baseline has no interval to divide.
	delete(base.PSI, "io")
	if got := buildFrom(t, base, final); got.PSI.IO.SomeStallRatio != nil {
		t.Fatalf("io = %+v, want no ratio without a baseline", got.PSI.IO)
	}
}

func TestCodeSetIgnoresEmptyCodes(t *testing.T) {
	t.Parallel()
	codes := newCodeSet()
	codes.add("")
	codes.addAll([]string{"b", "a", "b", ""})
	got := codes.sorted()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("sorted() = %v, want deduplicated and sorted codes", got)
	}
	if newCodeSet().sorted() != nil {
		t.Fatal("an empty set must render as no codes at all")
	}
}
