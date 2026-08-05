package hoststats

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/internal/runctl"
)

// Options configures a Collector. Every source is a seam, for two reasons:
// this package parses kernel text formats that must be testable on a
// developer's macOS laptop, and procfs and sysfs are separate trees — a
// collector rooted at /proc structurally cannot read /sys/block, which is
// exactly the bug that made the old process collector unable to tell a disk
// from a partition.
//
// A nil or empty field falls back to the OS implementation, so the zero value
// is the production configuration.
type Options struct {
	// ProcFS is /proc. Also the required source: New fails without it.
	ProcFS fs.FS
	// SysFS is /sys, used to tell whole devices from partitions.
	SysFS fs.FS
	// EtcFS is /etc, the source of machine-id.
	EtcFS fs.FS
	// CGroupFS is the cgroup2 mount. Defaults to the mount resolved from
	// /proc/self/mountinfo.
	CGroupFS fs.FS
	// CGroupRoot is that mount's absolute path, needed to check that a
	// configured cgroup path does not escape the mount through a symlink.
	CGroupRoot string

	// Statfs, Readlink and EvalSymlinks are syscalls that fs.FS cannot express,
	// so they are injected as functions instead.
	Statfs       func(path string) (FSRaw, error)
	Readlink     func(name string) (string, error)
	EvalSymlinks func(name string) (string, error)
	// Hostname resolves this host's name.
	Hostname func() (string, error)
	// Getenv reads configuration. Injected so tests need no process-global
	// environment.
	Getenv func(key string) string

	// DataDir is the second statfs target, typically the database data
	// directory: the root filesystem filling up and the data volume filling up
	// are different incidents.
	DataDir string
	// Now supplies boundary timestamps.
	Now func() time.Time
}

// withDefaults returns a copy in which every unset seam carries its OS
// implementation. It never mutates the receiver.
func (o Options) withDefaults() Options {
	out := o
	if out.ProcFS == nil {
		out.ProcFS = os.DirFS("/proc")
	}
	if out.SysFS == nil {
		out.SysFS = os.DirFS("/sys")
	}
	if out.EtcFS == nil {
		out.EtcFS = os.DirFS("/etc")
	}
	if out.Statfs == nil {
		out.Statfs = osStatfs
	}
	if out.Readlink == nil {
		out.Readlink = os.Readlink
	}
	if out.EvalSymlinks == nil {
		out.EvalSymlinks = filepath.EvalSymlinks
	}
	if out.Hostname == nil {
		out.Hostname = os.Hostname
	}
	if out.Getenv == nil {
		out.Getenv = os.Getenv
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	return out
}

// Enabled reports whether the ISUTOOLS_HOSTSTATS flag leaves host collection
// on. It defaults to on because the reads are a handful of small files at two
// boundaries; the flag exists so an operator can prove that by turning it off.
// A nil getenv reads the process environment.
func Enabled(getenv func(key string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	switch strings.ToLower(strings.TrimSpace(getenv(EnvEnable))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// runKey identifies one boundary of one run. The epoch is part of the key
// because a preempted run and its successor can share a run ID's neighbourhood
// in time but never a sample.
type runKey struct {
	RunID string
	Epoch runctl.Epoch
	Phase runctl.Phase
}

// Collector implements runctl.BaselineCollector for host resources.
//
// It keeps its samples keyed by (runID, epoch, phase) so that a retried
// boundary returns the first answer verbatim, timestamp included: a retry that
// re-samples would silently move the boundary the whole run is measured
// against.
type Collector struct {
	opt Options
	// cgroup is resolved once in New. The decision "which cgroup may we read"
	// depends on mounts and configuration, not on the moment of sampling, and
	// re-deciding it per boundary could produce two boundaries describing two
	// different cgroups.
	cgroup cgroupTarget
	// agentVersion is resolved once: build info cannot change at runtime.
	agentVersion string

	mu      sync.Mutex
	results map[runKey]runctl.SampleResult
	samples map[runKey]*Sample
	// epoch is the highest epoch accepted so far. Anything older is fenced.
	epoch runctl.Epoch
}

// New builds a Collector. It returns ErrUnsupportedOS when there is no procfs
// to read, so the caller can decline to register the collector at all rather
// than registering one that fails every boundary and drags the run to partial.
func New(o Options) (*Collector, error) {
	if o.ProcFS == nil && runtime.GOOS != "linux" {
		return nil, fmt.Errorf("%w: GOOS=%s", ErrUnsupportedOS, runtime.GOOS)
	}
	opt := o.withDefaults()
	if _, err := fs.Stat(opt.ProcFS, pathMeminfo); err != nil {
		return nil, fmt.Errorf("%w: stat %s: %w", ErrUnsupportedOS, pathMeminfo, err)
	}
	c := &Collector{
		opt:          opt,
		cgroup:       resolveCGroup(opt),
		agentVersion: buildinfo.Get().Short(),
		results:      make(map[runKey]runctl.SampleResult),
		samples:      make(map[runKey]*Sample),
	}
	return c, nil
}

// Name is the registration name and the snapshot section key.
func (c *Collector) Name() string { return CollectorName }

// readFile reads a whole file from an fs.FS, tolerating a nil filesystem so
// that every caller can stay a straight line.
func readFile(fsys fs.FS, name string) ([]byte, error) {
	if fsys == nil {
		return nil, fmt.Errorf("read %s: no filesystem", name)
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return data, nil
}
