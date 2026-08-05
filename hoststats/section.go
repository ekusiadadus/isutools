package hoststats

import "time"

// Section is the interval value Collect derives from two frozen samples. It is
// what lands in the run snapshot under the "hoststats" key.
//
// Point observations (memory, filesystem usage, cgroup limits) are reported at
// both boundaries rather than as a delta: "available memory fell by 2 GB" and
// "available memory is 200 MB" are different findings, and only the pair
// carries both.
type Section struct {
	Identity    Identity  `json:"identity"`
	Interval    Interval  `json:"interval"`
	Memory      Memory    `json:"memory"`
	Disks       []Disk    `json:"disks,omitempty"`       // sorted by Device
	PSI         *PSI      `json:"psi,omitempty"`         // nil on kernels without PSI
	Filesystems []FSUsage `json:"filesystems,omitempty"` // sorted by Path
	CGroup      *CGroup   `json:"cgroup,omitempty"`      // nil when skipped
	// Partial marks a section that is usable but incomplete. It never fails
	// the run: hoststats is optional, so its worst outcome is a partial run.
	Partial bool `json:"partial,omitempty"`
	// Codes lists this package's stable codes, sorted. Empty means clean.
	Codes []string `json:"codes,omitempty"`

	// health carries the health notes derived at Collect time. It is
	// unexported so that the JSON shape stays the wire contract and health
	// stays a process-local concern; read it through HealthNotes.
	health []HealthNote
}

// HealthNotes returns the health observations for this section, in the fixed
// order of HealthKeys. The caller receives a copy.
func (s *Section) HealthNotes() []HealthNote {
	if s == nil || len(s.health) == 0 {
		return nil
	}
	return append([]HealthNote(nil), s.health...)
}

// Interval is the measured span between the two boundaries. Rates use these
// two timestamps rather than the run's boundary window, so another collector
// being slow cannot distort this host's throughput numbers.
type Interval struct {
	BaselineAt time.Time `json:"baseline_at"`
	FinalAt    time.Time `json:"final_at"`
	Seconds    float64   `json:"seconds"`
}

// Memory is the interval's memory picture, in bytes.
type Memory struct {
	TotalBytes        uint64 `json:"total_bytes"`
	AvailableBaseline uint64 `json:"available_baseline_bytes"`
	AvailableFinal    uint64 `json:"available_final_bytes"`
	CachedBaseline    uint64 `json:"cached_baseline_bytes"`
	CachedFinal       uint64 `json:"cached_final_bytes"`
	DirtyBaseline     uint64 `json:"dirty_baseline_bytes"`
	DirtyFinal        uint64 `json:"dirty_final_bytes"`
	SwapTotalBytes    uint64 `json:"swap_total_bytes"`
	SwapFreeBaseline  uint64 `json:"swap_free_baseline_bytes"`
	SwapFreeFinal     uint64 `json:"swap_free_final_bytes"`
	// PageMajorFaults is the interval delta of pgmajfault: page faults that
	// had to reach the disk, which is what memory pressure feels like.
	PageMajorFaults uint64 `json:"page_major_faults"`
}

// Disk is one block device's interval. Rate fields are pointers because "the
// interval was too short to divide by" and "the rate was zero" are different
// facts, and only nil says the first one.
type Disk struct {
	Device        string   `json:"device"`
	ReadBytes     uint64   `json:"read_bytes"`
	WriteBytes    uint64   `json:"write_bytes"`
	ReadMBPerSec  *float64 `json:"read_mb_per_s,omitempty"`
	WriteMBPerSec *float64 `json:"write_mb_per_s,omitempty"`
	IOTimeMillis  uint64   `json:"io_time_ms"`
	// UtilPercent is the share of the interval the device spent doing IO. See
	// DiskUtilNote: on multi-queue devices it is not saturation.
	UtilPercent *float64 `json:"util_percent,omitempty"`
	// QueueAvg is the average queue depth, from the kernel's weighted IO time.
	QueueAvg *float64 `json:"queue_avg,omitempty"`
	// Appeared marks a device absent at the baseline, so it has no interval.
	Appeared bool `json:"appeared,omitempty"`
	// Code is CodeCounterRewind when the device's counters went backwards.
	Code string `json:"code,omitempty"`
}

// PSI is pressure stall information for the three resources the kernel
// exposes.
type PSI struct {
	CPU    PSIResource `json:"cpu"`
	Memory PSIResource `json:"memory"`
	IO     PSIResource `json:"io"`
}

// PSIResource is one resource's pressure. The averages are the kernel's own
// decaying windows read at the closing boundary; the stall ratio is ours, over
// exactly this run's interval.
type PSIResource struct {
	SomeAvg10      float64  `json:"some_avg10"`
	SomeAvg60      float64  `json:"some_avg60"`
	SomeStallRatio *float64 `json:"some_stall_ratio,omitempty"`
	FullAvg10      float64  `json:"full_avg10,omitempty"`
	FullAvg60      float64  `json:"full_avg60,omitempty"`
	FullStallRatio *float64 `json:"full_stall_ratio,omitempty"`
}

// FSUsage is one filesystem at both boundaries, so growth during the run is
// visible rather than inferred.
type FSUsage struct {
	Path          string `json:"path"`
	TotalBytes    uint64 `json:"total_bytes"`
	AvailBaseline uint64 `json:"avail_baseline_bytes"`
	AvailFinal    uint64 `json:"avail_final_bytes"`
}

// CGroup is the cgroup v2 limits and usage, always accompanied by the scope
// that says which cgroup they belong to.
type CGroup struct {
	Scope                 string   `json:"scope"`
	Path                  string   `json:"path"`
	CPUMaxCores           *float64 `json:"cpu_max_cores,omitempty"`
	MemoryMaxBytes        *uint64  `json:"memory_max_bytes,omitempty"`
	MemoryCurrentBaseline uint64   `json:"memory_current_baseline_bytes"`
	MemoryCurrentFinal    uint64   `json:"memory_current_final_bytes"`
	// Code is CodeLimitChanged when a limit moved during the interval.
	Code string `json:"code,omitempty"`
}

// Identity is who and where this agent is. The multi-host hub deduplicates
// peers on these fields, and the namespace ids are the only evidence of what
// an agent inside a container can actually see.
type Identity struct {
	Hostname string `json:"hostname"`
	// MachineIDHash is sha256(machine-id) truncated to 16 hex characters. The
	// raw id identifies the host to anyone who reads a snapshot, and a
	// truncated hash is enough to tell two hosts apart.
	MachineIDHash string `json:"machine_id_hash"`
	// BootIDHash changes on reboot, which is how a mid-run reboot is caught.
	BootIDHash string `json:"boot_id_hash"`
	PIDNS      string `json:"pid_ns"`
	NetNS      string `json:"net_ns"`
	MntNS      string `json:"mnt_ns"`
	// CgroupNS says which namespace the cgroup values were read from. It is
	// always displayed next to cgroup.scope: scope says which cgroup, this
	// says from where, and neither alone fixes what was measured.
	CgroupNS     string `json:"cgroup_ns"`
	Role         string `json:"role,omitempty"`
	AgentVersion string `json:"agent_version"`
}
