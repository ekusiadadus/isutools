package hoststats

import (
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// Sample is one boundary's raw observation. It is built once, deep-copied into
// the handle that leaves the collector, and never written again: Collect must
// be able to derive an interval from fixed values alone, which is only true if
// nothing can edit a sample after its timestamp was taken.
type Sample struct {
	// Phase is the boundary this sample belongs to.
	Phase runctl.Phase
	// At is the boundary timestamp, taken immediately before the required
	// source so the boundary is one instant rather than the span of all reads.
	At time.Time
	// Identity is captured at both boundaries because a reboot or a swapped
	// host mid-run invalidates every cumulative counter below.
	Identity Identity
	// Mem holds /proc/meminfo converted to bytes.
	Mem MemRaw
	// MajFault is the cumulative pgmajfault counter from /proc/vmstat.
	MajFault uint64
	// HasMajFault distinguishes "zero major faults" from "vmstat unreadable".
	HasMajFault bool
	// Disks holds cumulative /proc/diskstats counters keyed by device.
	Disks map[string]DiskRaw
	// PSI holds pressure counters keyed by "cpu", "memory" and "io".
	PSI map[string]PSIRaw
	// FS holds statfs results keyed by mount path.
	FS map[string]FSRaw
	// CGroup is nil when cgroup reading was skipped.
	CGroup *CGroupRaw
	// CGroupSkip explains a nil CGroup for health reporting: "v1", "no-mount"
	// or "path-rejected:<code>".
	CGroupSkip string
	// Codes lists per-source skips as "not-captured:<source>".
	Codes []string
}

// MemRaw is one boundary's /proc/meminfo, already converted from kB to bytes.
type MemRaw struct {
	TotalBytes     uint64
	AvailableBytes uint64
	CachedBytes    uint64
	DirtyBytes     uint64
	SwapTotalBytes uint64
	SwapFreeBytes  uint64
}

// DiskRaw is one device's cumulative counters. Field numbers refer to the
// kernel's own numbering in Documentation/admin-guide/iostats.rst, counted
// after the device name.
type DiskRaw struct {
	ReadSectors  uint64 // field 3
	WriteSectors uint64 // field 7
	IOTicksMS    uint64 // field 10: milliseconds spent doing IO
	WeightedMS   uint64 // field 11: weighted IO milliseconds, i.e. queue integral
}

// PSIRaw is one pressure file: the kernel's own decaying averages plus the
// cumulative stall total we turn into an interval ratio.
type PSIRaw struct {
	SomeAvg10   float64
	SomeAvg60   float64
	SomeTotalUS uint64
	FullAvg10   float64
	FullAvg60   float64
	FullTotalUS uint64
	// HasFull records whether a full line existed. Kernels before 5.13 have
	// none for cpu, and "absent" must not be rendered as "zero pressure".
	HasFull bool
}

// FSRaw is one filesystem's size at one boundary.
type FSRaw struct {
	TotalBytes uint64
	AvailBytes uint64
}

// CGroupRaw is one boundary's cgroup v2 reading, together with the scope that
// says which cgroup it describes.
type CGroupRaw struct {
	Scope              string
	Path               string
	CPUMaxCores        *float64 // nil means "max", i.e. no quota
	MemoryMaxBytes     *uint64  // nil means "max", i.e. no limit
	MemoryCurrentBytes uint64
}

// clone deep-copies a sample. Handles are values that get copied and shared,
// so a shallow copy would let one holder's edit rewrite everyone else's frozen
// interval.
func (s *Sample) clone() *Sample {
	if s == nil {
		return nil
	}
	out := *s
	out.Disks = cloneDisks(s.Disks)
	out.PSI = clonePSI(s.PSI)
	out.FS = cloneFS(s.FS)
	out.Codes = cloneStrings(s.Codes)
	out.CGroup = s.CGroup.clone()
	return &out
}

// clone deep-copies a cgroup reading including its optional limits.
func (c *CGroupRaw) clone() *CGroupRaw {
	if c == nil {
		return nil
	}
	out := *c
	out.CPUMaxCores = cloneFloat(c.CPUMaxCores)
	out.MemoryMaxBytes = cloneUint(c.MemoryMaxBytes)
	return &out
}

func cloneDisks(in map[string]DiskRaw) map[string]DiskRaw {
	if in == nil {
		return nil
	}
	out := make(map[string]DiskRaw, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func clonePSI(in map[string]PSIRaw) map[string]PSIRaw {
	if in == nil {
		return nil
	}
	out := make(map[string]PSIRaw, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneFS(in map[string]FSRaw) map[string]FSRaw {
	if in == nil {
		return nil
	}
	out := make(map[string]FSRaw, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	return append([]string(nil), in...)
}

func cloneFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func cloneUint(v *uint64) *uint64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

// skip records that one source could not be read at this boundary.
func (s *Sample) skip(source string) {
	s.Codes = append(s.Codes, CodeNotCapturedPrefix+source)
}
