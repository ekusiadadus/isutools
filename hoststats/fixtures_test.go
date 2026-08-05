package hoststats

import (
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// Kernel text fixtures. They are kept verbatim (extra fields, blank lines and
// all) because the parsers exist to survive real kernel output.
const (
	fixtureMeminfo = `MemTotal:       16316948 kB
MemFree:         1048576 kB
MemAvailable:    8158474 kB
Buffers:          131072 kB
Cached:          4194304 kB
SwapCached:            0 kB
Dirty:             12288 kB
SwapTotal:       2097152 kB
SwapFree:        1572864 kB
HugePages_Total:       0
`

	fixtureVMStat = `nr_free_pages 262144
pgfault 12345678
pgmajfault 4200
pgpgin 999
`

	// sda1 is a partition of sda and must not be counted twice; nvme0n1 is a
	// whole device despite the digit-suffixed name.
	fixtureDiskstats = `   8       0 sda 100 0 2000 500 200 0 4000 900 0 1200 1400 0 0 0 0 0 0
   8       1 sda1 50 0 1000 250 100 0 2000 450 0 600 700 0 0 0 0 0 0
 259       0 nvme0n1 10 0 40 5 20 0 80 9 0 12 14 0 0 0 0 0 0
`

	fixturePSICPU = `some avg10=0.10 avg60=0.20 avg300=0.30 total=1000000
`

	fixturePSIMemory = `some avg10=1.00 avg60=2.00 avg300=3.00 total=2000000
full avg10=0.50 avg60=0.60 avg300=0.70 total=1000000
`

	fixturePSIIO = `some avg10=5.00 avg60=6.00 avg300=7.00 total=3000000
full avg10=4.00 avg60=4.50 avg300=5.00 total=1500000
`

	fixtureMountinfo = `25 30 0:22 / /proc rw,nosuid,nodev,noexec,relatime shared:5 - proc proc rw
28 30 0:24 / /sys rw,nosuid,nodev,noexec,relatime shared:7 - sysfs sysfs rw
31 28 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate
`

	fixtureSelfCGroup = "0::/system.slice/isutools.service\n"
	fixtureBootID     = "11111111-2222-3333-4444-555555555555\n"
	fixtureMachineID  = "0123456789abcdef0123456789abcdef\n"

	cgroupRoot     = "/sys/fs/cgroup"
	agentCGroupRel = "system.slice/isutools.service"
	mysqlCGroupRel = "system.slice/mysqld.service"
)

var fixtureTime = time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)

func mapFile(content string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(content)}
}

func mapDir() *fstest.MapFile {
	return &fstest.MapFile{Mode: fs.ModeDir | 0o555}
}

func newProcFS() fstest.MapFS {
	return fstest.MapFS{
		pathMeminfo:       mapFile(fixtureMeminfo),
		pathVMStat:        mapFile(fixtureVMStat),
		pathDiskstats:     mapFile(fixtureDiskstats),
		"pressure/cpu":    mapFile(fixturePSICPU),
		"pressure/memory": mapFile(fixturePSIMemory),
		"pressure/io":     mapFile(fixturePSIIO),
		pathSelfCGroup:    mapFile(fixtureSelfCGroup),
		pathSelfMountinfo: mapFile(fixtureMountinfo),
		pathBootID:        mapFile(fixtureBootID),
	}
}

func newSysFS() fstest.MapFS {
	return fstest.MapFS{
		"block/sda":      mapDir(),
		"block/nvme0n1":  mapDir(),
		"block/sda/sda1": mapDir(),
	}
}

func newEtcFS() fstest.MapFS {
	return fstest.MapFS{pathMachineID: mapFile(fixtureMachineID)}
}

func newCGroupFS() fstest.MapFS {
	return fstest.MapFS{
		fileCPUMax:                         mapFile("max 100000\n"),
		fileMemoryMax:                      mapFile("max\n"),
		fileMemoryCurrent:                  mapFile("1073741824\n"),
		agentCGroupRel + "/cpu.max":        mapFile("200000 100000\n"),
		agentCGroupRel + "/memory.max":     mapFile("2147483648\n"),
		agentCGroupRel + "/memory.current": mapFile("536870912\n"),
		mysqlCGroupRel + "/cpu.max":        mapFile("400000 100000\n"),
		mysqlCGroupRel + "/memory.max":     mapFile("8589934592\n"),
		mysqlCGroupRel + "/memory.current": mapFile("4294967296\n"),
		// A cgroup that resolves but exposes no limit file at all.
		"nolimits/" + fileMemoryCurrent: mapFile("1\n"),
	}
}

// testEnv is a process-independent environment so tests never fight over
// os.Setenv and can run in parallel.
type testEnv map[string]string

func (e testEnv) get(key string) string { return e[key] }

// testClock hands out an explicit boundary time. Intervals are set, not
// waited for, so a rate assertion is exact.
type testClock struct {
	mu  sync.Mutex
	now time.Time
	// onNow runs before each reading, which lets a test expire a budget at the
	// exact instant sampling starts.
	onNow func()
}

func newClock(at time.Time) *testClock { return &testClock{now: at} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	hook := c.onNow
	now := c.now
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return now
}

func (c *testClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func (c *testClock) hook(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onNow = f
}

func fakeStatfs(sizes map[string]FSRaw) func(string) (FSRaw, error) {
	return func(path string) (FSRaw, error) {
		raw, ok := sizes[path]
		if !ok {
			return FSRaw{}, fmt.Errorf("statfs %s: %w", path, fs.ErrNotExist)
		}
		return raw, nil
	}
}

func fakeReadlink(links map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		target, ok := links[name]
		if !ok {
			return "", fmt.Errorf("readlink %s: %w", name, fs.ErrNotExist)
		}
		return target, nil
	}
}

// fakeEvalSymlinks resolves the paths a test declares and reports everything
// else as missing, which is what filepath.EvalSymlinks does for real.
func fakeEvalSymlinks(resolved map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		target, ok := resolved[name]
		if !ok {
			return "", fmt.Errorf("eval %s: %w", name, fs.ErrNotExist)
		}
		return target, nil
	}
}

func defaultStatfsSizes() map[string]FSRaw {
	return map[string]FSRaw{
		"/":              {TotalBytes: 50_000_000_000, AvailBytes: 20_000_000_000},
		"/var/lib/mysql": {TotalBytes: 200_000_000_000, AvailBytes: 120_000_000_000},
	}
}

func defaultNamespaces() map[string]string {
	return map[string]string{
		nsDir + "pid":    "pid:[4026531836]",
		nsDir + "net":    "net:[4026531840]",
		nsDir + "mnt":    "mnt:[4026531841]",
		nsDir + "cgroup": "cgroup:[4026531835]",
	}
}

func defaultSymlinks() map[string]string {
	return map[string]string{
		cgroupRoot:                        cgroupRoot,
		cgroupRoot + "/" + agentCGroupRel: cgroupRoot + "/" + agentCGroupRel,
		cgroupRoot + "/" + mysqlCGroupRel: cgroupRoot + "/" + mysqlCGroupRel,
		cgroupRoot + "/nolimits":          cgroupRoot + "/nolimits",
		// A symlink inside the mount pointing outside it.
		cgroupRoot + "/escape-symlink": "/etc/escape-target",
	}
}

// testOptions is a fully injected, deterministic host.
func testOptions(env testEnv, clock *testClock) Options {
	return Options{
		ProcFS:       newProcFS(),
		SysFS:        newSysFS(),
		EtcFS:        newEtcFS(),
		CGroupFS:     newCGroupFS(),
		CGroupRoot:   cgroupRoot,
		Statfs:       fakeStatfs(defaultStatfsSizes()),
		Readlink:     fakeReadlink(defaultNamespaces()),
		EvalSymlinks: fakeEvalSymlinks(defaultSymlinks()),
		Hostname:     func() (string, error) { return "isu1", nil },
		Getenv:       env.get,
		DataDir:      "/var/lib/mysql",
		Now:          clock.Now,
	}
}

func newTestCollector(t *testing.T, env testEnv, clock *testClock) *Collector {
	t.Helper()
	c, err := New(testOptions(env, clock))
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return c
}

// handleOf wraps a sample the way a boundary would, so Collect tests can build
// exactly the pair of samples they want to reason about.
func handleOf(sample *Sample, phase runctl.Phase, at time.Time) runctl.BaselineHandle {
	return runctl.NewBaselineHandle("run-1", 1, CollectorName, phase, at, sample)
}

// newSample is a minimal, complete sample a test can then perturb.
func newSample(phase runctl.Phase, at time.Time) *Sample {
	return &Sample{
		Phase: phase,
		At:    at,
		Identity: Identity{
			Hostname:      "isu1",
			MachineIDHash: "aaaabbbbccccdddd",
			BootIDHash:    "1111222233334444",
			PIDNS:         "pid:[4026531836]",
			CgroupNS:      "cgroup:[4026531835]",
			AgentVersion:  "test",
		},
		Mem: MemRaw{
			TotalBytes:     16 << 30,
			AvailableBytes: 8 << 30,
			CachedBytes:    4 << 30,
			DirtyBytes:     1 << 20,
			SwapTotalBytes: 2 << 30,
			SwapFreeBytes:  2 << 30,
		},
		MajFault:    1000,
		HasMajFault: true,
		Disks: map[string]DiskRaw{
			"sda": {ReadSectors: 1000, WriteSectors: 2000, IOTicksMS: 500, WeightedMS: 800},
		},
		PSI: map[string]PSIRaw{
			"cpu":    {SomeAvg10: 1, SomeAvg60: 2, SomeTotalUS: 1_000_000},
			"memory": {SomeAvg10: 3, SomeTotalUS: 2_000_000, FullAvg10: 1, FullTotalUS: 500_000, HasFull: true},
			"io":     {SomeAvg10: 5, SomeTotalUS: 3_000_000, HasFull: true, FullTotalUS: 1_000_000},
		},
		FS: map[string]FSRaw{
			"/": {TotalBytes: 100, AvailBytes: 60},
		},
		CGroup: &CGroupRaw{
			Scope:              ScopeVisibleRoot,
			MemoryCurrentBytes: 1 << 30,
		},
	}
}

func floatValue(t *testing.T, name string, value *float64) float64 {
	t.Helper()
	if value == nil {
		t.Fatalf("%s = nil, want a value", name)
	}
	return *value
}

func nearlyEqual(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
