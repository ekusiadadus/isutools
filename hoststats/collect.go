package hoststats

import (
	"fmt"
	"sort"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

const (
	// minIntervalSeconds is the shortest interval worth dividing by. Below a
	// millisecond the quotient is dominated by timestamp granularity, and a
	// nonsense rate is worse than an absent one.
	minIntervalSeconds = 0.001
	// sectorSize is the unit /proc/diskstats counts in, always 512 bytes
	// regardless of the device's real sector size.
	sectorSize      = 512
	bytesPerMB      = 1e6
	microsPerSecond = 1e6
	millisPerSecond = 1000
)

// Collect derives the interval from two frozen samples.
//
// It reads nothing but the two handles: no procfs, no sysfs, no syscalls, and
// none of the collector's own fields. That is what makes a snapshot immutable
// in practice and not just in intent — a Collect that peeked at live state
// would report the moment the report was built, not the run.
//
// It also never returns an error for a data problem. hoststats is optional, so
// an error here drops the entire host section from the snapshot, taking the
// memory, cgroup and identity point observations down with the one counter
// that misbehaved. Degradation is reported per metric instead.
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) {
	baseSample, err := sampleOf(base, "baseline")
	if err != nil {
		return nil, err
	}
	finalSample, err := sampleOf(final, "final")
	if err != nil {
		return nil, err
	}
	return buildSection(base.SampledAt, final.SampledAt, baseSample, finalSample), nil
}

// sampleOf recovers a handle's sample. A type mismatch is an error rather than
// a panic: measurement may not break the measured application.
func sampleOf(h runctl.BaselineHandle, label string) (*Sample, error) {
	sample, ok := h.Sample().(*Sample)
	if !ok || sample == nil {
		return nil, fmt.Errorf("hoststats: %s handle carries %T, want *hoststats.Sample", label, h.Sample())
	}
	return sample, nil
}

// buildSection is a free function so that "Collect touches no collector state"
// is enforced by the compiler rather than by review.
func buildSection(baseAt, finalAt time.Time, base, final *Sample) *Section {
	interval := Interval{
		BaselineAt: baseAt,
		FinalAt:    finalAt,
		Seconds:    finalAt.Sub(baseAt).Seconds(),
	}
	codes := newCodeSet()
	codes.addAll(base.Codes)
	codes.addAll(final.Codes)

	// A reboot or a swapped host resets every cumulative counter below, so the
	// deltas stop meaning anything while the point observations stay perfectly
	// valid. Dropping only the deltas keeps the rest.
	hostChanged := false
	if base.Identity.BootIDHash != final.Identity.BootIDHash {
		hostChanged = true
		codes.add(CodeBootIDChanged)
	}
	if base.Identity.MachineIDHash != final.Identity.MachineIDHash {
		hostChanged = true
		codes.add(CodeMachineIDChanged)
	}
	rates := interval.Seconds >= minIntervalSeconds && !hostChanged

	section := &Section{
		Identity:    base.Identity,
		Interval:    interval,
		Memory:      buildMemory(base, final, hostChanged, codes),
		Disks:       buildDisks(base, final, interval.Seconds, rates, hostChanged, codes),
		PSI:         buildPSI(base, final, interval.Seconds, rates),
		Filesystems: buildFilesystems(base, final),
		CGroup:      buildCGroup(base, final, codes),
	}
	section.Codes = codes.sorted()
	section.Partial = len(section.Codes) > 0
	section.health = healthNotes(base, final, section.Codes)
	return section
}

// buildMemory pairs the two boundaries' memory readings. Available memory is
// reported at both ends rather than as a delta because the level and the
// movement are different findings, and only the pair carries both.
func buildMemory(base, final *Sample, hostChanged bool, codes *codeSet) Memory {
	memory := Memory{
		TotalBytes:        final.Mem.TotalBytes,
		AvailableBaseline: base.Mem.AvailableBytes,
		AvailableFinal:    final.Mem.AvailableBytes,
		CachedBaseline:    base.Mem.CachedBytes,
		CachedFinal:       final.Mem.CachedBytes,
		DirtyBaseline:     base.Mem.DirtyBytes,
		DirtyFinal:        final.Mem.DirtyBytes,
		SwapTotalBytes:    final.Mem.SwapTotalBytes,
		SwapFreeBaseline:  base.Mem.SwapFreeBytes,
		SwapFreeFinal:     final.Mem.SwapFreeBytes,
	}
	if memory.TotalBytes == 0 {
		memory.TotalBytes = base.Mem.TotalBytes
	}
	switch {
	case hostChanged || !base.HasMajFault || !final.HasMajFault:
		// No delta is derivable; the field stays zero.
	case final.MajFault < base.MajFault:
		codes.add(CodeCounterRewindPrefix + SourceVMStat)
	default:
		memory.PageMajorFaults = final.MajFault - base.MajFault
	}
	return memory
}

// buildDisks derives per-device interval values, sorted by device so that two
// runs of the same host diff cleanly.
//
// Devices are keyed off the closing sample: a device that vanished mid-run has
// no closing observation to report, while one that appeared has no interval
// and says so.
func buildDisks(base, final *Sample, seconds float64, rates, hostChanged bool, codes *codeSet) []Disk {
	if len(final.Disks) == 0 {
		return nil
	}
	names := make([]string, 0, len(final.Disks))
	for name := range final.Disks {
		names = append(names, name)
	}
	sort.Strings(names)

	disks := make([]Disk, 0, len(names))
	for _, name := range names {
		end := final.Disks[name]
		disk := Disk{Device: name}
		start, existed := base.Disks[name]
		switch {
		case !existed:
			disk.Appeared = true
		case diskRewound(start, end):
			// Per-metric degradation: this device loses its interval, every
			// other device keeps its own.
			disk.Code = CodeCounterRewind
			codes.add(CodeCounterRewindPrefix + name)
		case hostChanged:
			// Counters restarted with the host; deltas stay zero.
		default:
			disk.ReadBytes = mulSaturate(end.ReadSectors-start.ReadSectors, sectorSize)
			disk.WriteBytes = mulSaturate(end.WriteSectors-start.WriteSectors, sectorSize)
			disk.IOTimeMillis = end.IOTicksMS - start.IOTicksMS
			if rates {
				weighted := end.WeightedMS - start.WeightedMS
				disk.ReadMBPerSec = ratePtr(float64(disk.ReadBytes) / bytesPerMB / seconds)
				disk.WriteMBPerSec = ratePtr(float64(disk.WriteBytes) / bytesPerMB / seconds)
				disk.UtilPercent = ratePtr(float64(disk.IOTimeMillis) / (seconds * millisPerSecond) * 100)
				disk.QueueAvg = ratePtr(float64(weighted) / (seconds * millisPerSecond))
			}
		}
		disks = append(disks, disk)
	}
	return disks
}

// diskRewound reports whether any counter of a device moved backwards, which
// means the device was reset, re-enumerated, or the host rebooted.
func diskRewound(start, end DiskRaw) bool {
	return end.ReadSectors < start.ReadSectors ||
		end.WriteSectors < start.WriteSectors ||
		end.IOTicksMS < start.IOTicksMS ||
		end.WeightedMS < start.WeightedMS
}

// buildPSI reports the kernel's own averages from the closing boundary and a
// stall ratio computed over exactly this run's interval. The averages describe
// the last 10 and 60 seconds whatever the run's length; the ratio is the only
// number that describes the run.
func buildPSI(base, final *Sample, seconds float64, rates bool) *PSI {
	if len(final.PSI) == 0 {
		return nil
	}
	psi := &PSI{}
	// System-level cpu "full" is zero by definition — a fully stalled CPU
	// cannot be observed from the CPU it stalled — so it is never rendered.
	psi.CPU = buildPSIResource(base, final, "cpu", seconds, rates, false)
	psi.Memory = buildPSIResource(base, final, "memory", seconds, rates, true)
	psi.IO = buildPSIResource(base, final, "io", seconds, rates, true)
	return psi
}

// buildPSIResource builds one resource's pressure view.
func buildPSIResource(base, final *Sample, name string, seconds float64, rates, allowFull bool) PSIResource {
	end, ok := final.PSI[name]
	if !ok {
		return PSIResource{}
	}
	start, hasStart := base.PSI[name]
	resource := PSIResource{SomeAvg10: end.SomeAvg10, SomeAvg60: end.SomeAvg60}
	if rates && hasStart && end.SomeTotalUS >= start.SomeTotalUS {
		resource.SomeStallRatio = stallRatio(end.SomeTotalUS-start.SomeTotalUS, seconds)
	}
	if !allowFull || !end.HasFull {
		return resource
	}
	resource.FullAvg10 = end.FullAvg10
	resource.FullAvg60 = end.FullAvg60
	if rates && hasStart && start.HasFull && end.FullTotalUS >= start.FullTotalUS {
		resource.FullStallRatio = stallRatio(end.FullTotalUS-start.FullTotalUS, seconds)
	}
	return resource
}

// stallRatio converts stalled microseconds into a share of the interval.
func stallRatio(deltaUS uint64, seconds float64) *float64 {
	return ratePtr(float64(deltaUS) / (seconds * microsPerSecond))
}

// buildFilesystems reports every path either boundary saw, sorted by path.
func buildFilesystems(base, final *Sample) []FSUsage {
	if len(base.FS) == 0 && len(final.FS) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(base.FS)+len(final.FS))
	mounts := make([]string, 0, len(base.FS)+len(final.FS))
	for _, source := range []map[string]FSRaw{base.FS, final.FS} {
		for mount := range source {
			if _, done := seen[mount]; done {
				continue
			}
			seen[mount] = struct{}{}
			mounts = append(mounts, mount)
		}
	}
	sort.Strings(mounts)

	usages := make([]FSUsage, 0, len(mounts))
	for _, mount := range mounts {
		start, hasStart := base.FS[mount]
		end, hasEnd := final.FS[mount]
		usage := FSUsage{Path: mount}
		if hasStart {
			usage.TotalBytes = start.TotalBytes
			usage.AvailBaseline = start.AvailBytes
		}
		if hasEnd {
			usage.TotalBytes = end.TotalBytes
			usage.AvailFinal = end.AvailBytes
		}
		usages = append(usages, usage)
	}
	return usages
}

// buildCGroup reports the closing boundary's limits, plus usage at both ends.
// A limit that moved mid-run is flagged rather than averaged: the run was
// measured under two different ceilings, and the reader has to know.
func buildCGroup(base, final *Sample, codes *codeSet) *CGroup {
	if final.CGroup == nil {
		return nil
	}
	cgroup := &CGroup{
		Scope:              final.CGroup.Scope,
		Path:               final.CGroup.Path,
		CPUMaxCores:        cloneFloat(final.CGroup.CPUMaxCores),
		MemoryMaxBytes:     cloneUint(final.CGroup.MemoryMaxBytes),
		MemoryCurrentFinal: final.CGroup.MemoryCurrentBytes,
	}
	if base.CGroup == nil {
		return cgroup
	}
	cgroup.MemoryCurrentBaseline = base.CGroup.MemoryCurrentBytes
	if !sameLimits(base.CGroup, final.CGroup) {
		cgroup.Code = CodeLimitChanged
		codes.add(CodeLimitChanged)
	}
	return cgroup
}

// sameLimits compares two boundaries' cgroup ceilings, treating "unlimited" as
// a value rather than as missing data.
func sameLimits(start, end *CGroupRaw) bool {
	return sameFloat(start.CPUMaxCores, end.CPUMaxCores) && sameUint(start.MemoryMaxBytes, end.MemoryMaxBytes)
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameUint(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ratePtr boxes a derived rate. Rates are pointers so that "not derivable"
// stays distinguishable from "zero".
func ratePtr(value float64) *float64 {
	out := value
	return &out
}

// codeSet accumulates Section codes without duplicates and hands them back in
// a stable order, so the same run state always renders the same section.
type codeSet struct {
	seen map[string]struct{}
	list []string
}

func newCodeSet() *codeSet {
	return &codeSet{seen: make(map[string]struct{})}
}

func (s *codeSet) add(code string) {
	if code == "" {
		return
	}
	if _, ok := s.seen[code]; ok {
		return
	}
	s.seen[code] = struct{}{}
	s.list = append(s.list, code)
}

func (s *codeSet) addAll(codes []string) {
	for _, code := range codes {
		s.add(code)
	}
}

func (s *codeSet) sorted() []string {
	if len(s.list) == 0 {
		return nil
	}
	out := append([]string(nil), s.list...)
	sort.Strings(out)
	return out
}
