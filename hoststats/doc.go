// Package hoststats measures host resources over one measurement run and
// records the identity of the host the agent is actually looking at.
//
// Existing collectors report per-process CPU and RSS, which answers "what did
// my process do" but never "did the machine have memory, disk or IO left".
// This package fills that gap from Linux procfs, sysfs and cgroup v2, and it
// deliberately reports where it looked: inside a container the very same files
// describe a namespace rather than the machine, so a number without its
// identity and cgroup scope is unreadable.
//
// It implements runctl.BaselineCollector. Both boundaries return an immutable
// handle carrying a deep-copied Sample, and Collect derives the interval from
// those two frozen samples alone — it performs no I/O whatsoever, so a
// snapshot can never drift while it is being built.
//
// Everything here is fail-open: hoststats is an optional collector, so a
// missing file, an unsupported kernel or an exhausted budget degrades one
// source at a time and never fails the run or the measured application.
package hoststats

// Collector name and the environment variables this package reads. The flag
// switches the feature on and off; the two cgroup variables are configuration
// for a feature that is already on.
const (
	// CollectorName is the runctl registration name and snapshot section key.
	CollectorName = "hoststats"

	// EnvEnable turns host collection off when set to "off". Default on.
	EnvEnable = "ISUTOOLS_HOSTSTATS"
	// EnvRole labels this agent ("app", "db", "dns", "proxy", ...). Free text:
	// multi-host aggregation displays it, nothing branches on it.
	EnvRole = "ISUTOOLS_ROLE"
	// EnvCGroupScope set to "host" declares that this agent lives in the
	// initial cgroup namespace. It is never inferred: inside a cgroup
	// namespace both /proc/self/cgroup and mountinfo are virtualised, so no
	// in-process check can tell the two cases apart.
	EnvCGroupScope = "ISUTOOLS_CGROUP_SCOPE"
	// EnvCGroupPath names the cgroup to read, relative to the cgroup2 mount
	// root. It exists because the agent and the measured service (mysqld, for
	// example) often live in different cgroups.
	EnvCGroupPath = "ISUTOOLS_CGROUP_PATH"
)

// cgroup.scope values. Which cgroup was read is as load-bearing as the limits
// themselves, so the scope travels with them everywhere.
const (
	// ScopeConfigured is the cgroup named by EnvCGroupPath.
	ScopeConfigured = "configured-cgroup"
	// ScopeHost is an operator's explicit declaration that the visible cgroup
	// tree is the host's. Only EnvCGroupScope produces it.
	ScopeHost = "host"
	// ScopeVisibleRoot is the root of whatever cgroup tree is visible. It is
	// the default because it is the one thing that is always true.
	ScopeVisibleRoot = "visible-root"
	// ScopeAgentCGroup is the agent's own cgroup, which may well be a
	// different cgroup from the service under measurement.
	ScopeAgentCGroup = "agent-cgroup"
)

// Stable Section and Disk codes. The set is closed: transports and templates
// switch on these strings.
const (
	// CodeNotCapturedPrefix marks a source that was skipped, with the source
	// name appended ("not-captured:psi").
	CodeNotCapturedPrefix = "not-captured:"
	// CodeCounterRewindPrefix marks a counter that went backwards, with the
	// device name or "vmstat" appended.
	CodeCounterRewindPrefix = "counter-rewind:"
	// CodeCounterRewind is the per-metric form used in Disk.Code.
	CodeCounterRewind = "counter-rewind"
	// CodeLimitChanged reports a cgroup limit that changed mid-run.
	CodeLimitChanged = "limit-changed"
	// CodeBootIDChanged reports a reboot inside the interval.
	CodeBootIDChanged = "boot-id-changed"
	// CodeMachineIDChanged reports that the samples came from two machines.
	CodeMachineIDChanged = "machine-id-changed"
)

// Source names used in "not-captured:<source>" codes.
const (
	SourceVMStat    = "vmstat"
	SourceDiskstats = "diskstats"
	SourcePSI       = "psi"
	SourceStatfs    = "statfs"
	SourceCGroup    = "cgroup"
)

// Fixed display notes. They are constants rather than template text because
// both of them exist to prevent a specific misreading, and a misreading
// prevented only in one of two renderers is not prevented at all.
const (
	// DiskUtilNote warns that util% is not saturation on multi-queue devices:
	// an NVMe drive at 100% "io time" can still be far from its limit.
	DiskUtilNote = "util% はデバイスが IO 処理中だった時間の割合です。NVMe などの multi-queue デバイスでは 100% でも飽和を意味しません。"
	// CGroupScopeNote warns that the agent's limits may not be the measured
	// service's limits.
	CGroupScopeNote = "cgroup の値は scope が示す cgroup のものです。agent と観測対象サービスが別 cgroup の場合、ここの上限は観測対象の上限ではありません。"
)

// Internal procfs, sysfs and cgroupfs paths. They are relative because every
// tree is injected as an fs.FS; only the namespace links below are absolute,
// since readlink(2) cannot be reached through fs.FS.
const (
	pathMeminfo       = "meminfo"
	pathVMStat        = "vmstat"
	pathDiskstats     = "diskstats"
	pathPressureDir   = "pressure"
	pathSelfCGroup    = "self/cgroup"
	pathSelfMountinfo = "self/mountinfo"
	pathBootID        = "sys/kernel/random/boot_id"
	pathMachineID     = "machine-id"
	pathSysBlock      = "block"

	fileCPUMax        = "cpu.max"
	fileMemoryMax     = "memory.max"
	fileMemoryCurrent = "memory.current"

	nsDir = "/proc/self/ns/"
)
