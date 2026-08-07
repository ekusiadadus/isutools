package procstats

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestCollectorMeasuresResetToSnapshotInterval(t *testing.T) {
	files := fixtureFS(1000, 2, map[int]procFixture{
		10: {comm: "worker pool", utime: 70, stime: 30, starttime: 100, rssPages: 3},
		20: {comm: "memory", utime: 10, stime: 10, starttime: 200, rssPages: 9},
	})
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c := New(
		WithFS(files),
		WithClockTicks(100),
		WithPageSize(4096),
		WithClock(func() time.Time { return now }),
	)
	if err := c.Reset(); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}

	setSystemStat(files, 1400, 2)
	setProcess(files, 10, procFixture{comm: "worker pool", utime: 100, stime: 40, starttime: 100, rssPages: 5})
	setProcess(files, 20, procFixture{comm: "memory", utime: 15, stime: 15, starttime: 200, rssPages: 10})
	now = now.Add(2 * time.Second)

	got := c.Snapshot()
	if got.IntervalJiffies != 400 || got.CPUs != 2 {
		t.Fatalf("interval = %d jiffies, CPUs = %d; want 400, 2", got.IntervalJiffies, got.CPUs)
	}
	if got.StartedAt.IsZero() || got.EndedAt.Sub(got.StartedAt) != 2*time.Second {
		t.Fatalf("interval times = %v..%v", got.StartedAt, got.EndedAt)
	}
	if len(got.TopCPU) != 2 || len(got.TopRSS) != 2 {
		t.Fatalf("top lengths = CPU %d, RSS %d; want 2, 2", len(got.TopCPU), len(got.TopRSS))
	}
	worker := got.TopCPU[0]
	if worker.PID != 10 || worker.Command != "worker pool" || worker.DeltaJiffies != 40 {
		t.Fatalf("top CPU = %+v", worker)
	}
	if !closeEnough(worker.CPUPercent, 20) || !closeEnough(worker.CPUSeconds, 0.4) {
		t.Fatalf("worker CPU = %.3f%%, %.3fs; want 20%%, 0.4s", worker.CPUPercent, worker.CPUSeconds)
	}
	if worker.RSSBytes != 5*4096 {
		t.Fatalf("worker RSS = %d; want %d", worker.RSSBytes, 5*4096)
	}
	if got.TopRSS[0].PID != 20 || got.TopRSS[0].RSSBytes != 10*4096 {
		t.Fatalf("top RSS = %+v", got.TopRSS[0])
	}
	if got.Health.Status != StatusOK || got.Health.Partial || got.Health.Dropped != 0 {
		t.Fatalf("health = %+v; want ok", got.Health)
	}
}

func TestTimelinePointAggregatesCumulativeHostAndProcessCounters(t *testing.T) {
	t.Parallel()
	files := fixtureFS(1000, 2, map[int]procFixture{
		10: {comm: "worker", utime: 70, stime: 30, starttime: 100, rssPages: 3},
		20: {comm: "memory", utime: 10, stime: 10, starttime: 200, rssPages: 9},
	})
	c := New(WithFS(files), WithPageSize(4096))
	point, err := c.TimelinePoint()
	if err != nil {
		t.Fatal(err)
	}
	if point.TotalJiffies != 1000 || point.BusyJiffies != 1000 || point.IOWaitJiffies != 0 ||
		point.ProcessJiffies != 120 || point.CPUs != 2 || point.RSSBytes != 12*4096 {
		t.Fatalf("TimelinePoint() = %#v", point)
	}
}

func TestCollectorBoundsAndSortsTopLists(t *testing.T) {
	files := fixtureFS(100, 1, map[int]procFixture{
		1: {comm: "one", utime: 1, starttime: 10, rssPages: 1},
		2: {comm: "two", utime: 2, starttime: 20, rssPages: 2},
		3: {comm: "three", utime: 3, starttime: 30, rssPages: 3},
	})
	c := New(WithFS(files), WithTopN(2))
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	setSystemStat(files, 200, 1)
	setProcess(files, 1, procFixture{comm: "one", utime: 31, starttime: 10, rssPages: 30})
	setProcess(files, 2, procFixture{comm: "two", utime: 22, starttime: 20, rssPages: 20})
	setProcess(files, 3, procFixture{comm: "three", utime: 13, starttime: 30, rssPages: 10})

	got := c.Snapshot()
	if ids := processIDs(got.TopCPU); fmt.Sprint(ids) != "[1 2]" {
		t.Fatalf("TopCPU PIDs = %v; want [1 2]", ids)
	}
	if ids := processIDs(got.TopRSS); fmt.Sprint(ids) != "[1 2]" {
		t.Fatalf("TopRSS PIDs = %v; want [1 2]", ids)
	}
}

func TestCollectorHandlesAppearedDisappearedAndReusedPID(t *testing.T) {
	files := fixtureFS(1000, 2, map[int]procFixture{
		1: {comm: "old-one", utime: 100, starttime: 10, rssPages: 1},
		2: {comm: "gone", utime: 100, starttime: 20, rssPages: 1},
	})
	c := New(WithFS(files), WithClockTicks(100), WithPageSize(4096))
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	setSystemStat(files, 1200, 2)
	setProcess(files, 1, procFixture{comm: "new-one", utime: 20, starttime: 900, rssPages: 4})
	deleteProcess(files, 2)
	setProcess(files, 3, procFixture{comm: "appeared", utime: 15, starttime: 950, rssPages: 3})

	got := c.Snapshot()
	byPID := make(map[int]Process)
	for _, p := range got.TopCPU {
		byPID[p.PID] = p
	}
	if !byPID[1].PIDReused || !byPID[1].Appeared || byPID[1].DeltaJiffies != 20 {
		t.Fatalf("reused process = %+v", byPID[1])
	}
	if !byPID[3].Appeared || byPID[3].PIDReused || byPID[3].DeltaJiffies != 15 {
		t.Fatalf("appeared process = %+v", byPID[3])
	}
	if got.Health.Status != StatusPartial || !got.Health.Partial || got.Health.Dropped != 1 {
		t.Fatalf("health = %+v; want only the reused PID marked partial", got.Health)
	}
	if joined := strings.Join(got.Health.Errors, "\n"); strings.Contains(joined, "disappeared") || !strings.Contains(joined, "reused") {
		t.Fatalf("health errors = %q; want only PID reuse, not ordinary exit", joined)
	}
}

func TestCollectorIgnoresUntrackedProcessExit(t *testing.T) {
	files := fixtureFS(1000, 2, map[int]procFixture{
		1: {comm: "application", utime: 100, starttime: 10, rssPages: 4},
		2: {comm: "short-lived helper", utime: 5, starttime: 20, rssPages: 1},
	})
	c := New(WithFS(files))
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	setSystemStat(files, 1200, 2)
	setProcess(files, 1, procFixture{comm: "application", utime: 120, starttime: 10, rssPages: 4})
	deleteProcess(files, 2)

	got := c.Snapshot()
	if got.Health.Status != StatusOK || got.Health.Partial || got.Health.Dropped != 0 {
		t.Fatalf("health = %+v; ordinary process churn must not make the run partial", got.Health)
	}
}

func TestCollectorReportsTrackedProcessExit(t *testing.T) {
	files := fixtureFS(1000, 2, map[int]procFixture{
		1: {comm: "application", utime: 100, starttime: 10, rssPages: 4},
		2: {comm: "tracked worker", utime: 50, starttime: 20, rssPages: 2},
	})
	c := New(WithFS(files), WithTrackedPIDs(2))
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	setSystemStat(files, 1200, 2)
	setProcess(files, 1, procFixture{comm: "application", utime: 120, starttime: 10, rssPages: 4})
	deleteProcess(files, 2)

	got := c.Snapshot()
	if got.Health.Status != StatusPartial || !got.Health.Partial || got.Health.Dropped != 1 {
		t.Fatalf("health = %+v; a lost explicitly tracked process must be partial", got.Health)
	}
	if message := strings.Join(got.Health.Errors, "\n"); !strings.Contains(message, "tracked pid 2 disappeared") {
		t.Fatalf("health errors = %q; want the tracked process identified", message)
	}
}

func TestCollectorReportsReadAndParseFailuresWithoutPanicking(t *testing.T) {
	files := fixtureFS(100, 1, map[int]procFixture{
		1: {comm: "ok", utime: 10, starttime: 10, rssPages: 1},
		2: {comm: "denied", utime: 10, starttime: 20, rssPages: 1},
	})
	c := New(WithFS(files))
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	setSystemStat(files, 200, 1)
	setProcess(files, 1, procFixture{comm: "ok", utime: 20, starttime: 10, rssPages: 2})
	files["2/stat"] = &fstest.MapFile{Data: []byte("malformed")}
	files["1/statm"] = &fstest.MapFile{Data: []byte("bad rss")}

	got := c.Snapshot()
	if len(got.TopCPU) != 1 || got.TopCPU[0].PID != 1 || got.TopCPU[0].RSSBytes != 0 {
		t.Fatalf("TopCPU = %+v; want pid 1 retained with unknown RSS", got.TopCPU)
	}
	if got.Health.Status != StatusPartial || got.Health.Dropped != 1 || len(got.Health.Errors) < 2 {
		t.Fatalf("health = %+v; want one dropped process plus the RSS failure", got.Health)
	}
}

func TestCollectorReportsPermissionDenied(t *testing.T) {
	base := fixtureFS(100, 1, map[int]procFixture{
		1: {comm: "worker", utime: 10, starttime: 10, rssPages: 1},
	})
	c := New(WithFS(permissionFS{FS: base, deny: "1/stat"}))
	if err := c.Reset(); err != nil {
		t.Fatalf("Reset must fail open for a process error: %v", err)
	}
	h := c.Health()
	if h.Status != StatusPartial || h.Dropped != 1 || !strings.Contains(strings.Join(h.Errors, "\n"), "permission denied") {
		t.Fatalf("health = %+v; want permission failure", h)
	}
}

func TestCollectorReportsUnavailableSystemStat(t *testing.T) {
	c := New(WithFS(fstest.MapFS{}))
	if err := c.Reset(); err == nil {
		t.Fatal("Reset() error = nil; want missing /proc/stat error")
	}
	got := c.Snapshot()
	if got.Health.Status != StatusUnavailable || !got.Health.Partial || len(got.TopCPU) != 0 {
		t.Fatalf("snapshot = %+v; want unavailable", got)
	}
}

func TestCollectorResetStartsANewInterval(t *testing.T) {
	files := fixtureFS(100, 1, map[int]procFixture{
		1: {comm: "worker", utime: 10, starttime: 10, rssPages: 1},
	})
	c := New(WithFS(files))
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	setSystemStat(files, 200, 1)
	setProcess(files, 1, procFixture{comm: "worker", utime: 20, starttime: 10, rssPages: 1})
	if got := c.Snapshot().TopCPU[0].DeltaJiffies; got != 10 {
		t.Fatalf("first interval delta = %d; want 10", got)
	}
	if err := c.Reset(); err != nil {
		t.Fatal(err)
	}
	setSystemStat(files, 250, 1)
	setProcess(files, 1, procFixture{comm: "worker", utime: 25, starttime: 10, rssPages: 1})
	if got := c.Snapshot().TopCPU[0].DeltaJiffies; got != 5 {
		t.Fatalf("second interval delta = %d; want 5", got)
	}
}

func TestCoordinatedRunUsesOpeningAndClosingBoundaries(t *testing.T) {
	files := fixtureFS(100, 1, map[int]procFixture{
		1: {comm: "worker", utime: 10, starttime: 10, rssPages: 1},
	})
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	c := New(WithFS(files), WithClock(func() time.Time { return now }))
	if err := c.Reset(); err != nil {
		t.Fatalf("service-start Reset: %v", err)
	}

	// The service has already been alive for an hour when ResetNow opens the
	// benchmark. CaptureBaseline must replace that old baseline.
	now = now.Add(time.Hour)
	setSystemStat(files, 150, 1)
	setProcess(files, 1, procFixture{comm: "worker", utime: 15, starttime: 10, rssPages: 1})
	base, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline: %v", err)
	}
	benchmarkStart := now

	// A retried boundary is idempotent and must not silently move the baseline.
	now = now.Add(time.Second)
	setSystemStat(files, 180, 1)
	setProcess(files, 1, procFixture{comm: "worker", utime: 18, starttime: 10, rssPages: 1})
	replayed, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil || replayed.At != base.At {
		t.Fatalf("replayed baseline = %+v, %v; want original %+v", replayed, err, base)
	}

	now = benchmarkStart.Add(time.Minute)
	setSystemStat(files, 250, 1)
	setProcess(files, 1, procFixture{comm: "worker", utime: 25, starttime: 10, rssPages: 2})
	final, err := c.CaptureFinal(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureFinal: %v", err)
	}

	// Change procfs again after the closing boundary. Collect must return the
	// frozen minute, not extend the run to report-render time.
	now = benchmarkStart.Add(10 * time.Minute)
	setSystemStat(files, 900, 1)
	setProcess(files, 1, procFixture{comm: "worker", utime: 90, starttime: 10, rssPages: 3})
	value, err := c.Collect(base.Handle, final.Handle)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	snapshot, ok := value.(Snapshot)
	if !ok {
		t.Fatalf("section type = %T, want procstats.Snapshot", value)
	}
	if snapshot.StartedAt != benchmarkStart || snapshot.EndedAt != benchmarkStart.Add(time.Minute) {
		t.Fatalf("proc interval = %s..%s, want benchmark %s..%s",
			snapshot.StartedAt, snapshot.EndedAt, benchmarkStart, benchmarkStart.Add(time.Minute))
	}
	if len(snapshot.TopCPU) != 1 || snapshot.TopCPU[0].DeltaJiffies != 10 {
		t.Fatalf("frozen process delta = %+v, want 10 benchmark jiffies", snapshot.TopCPU)
	}
}

type procFixture struct {
	comm      string
	utime     uint64
	stime     uint64
	starttime uint64
	rssPages  uint64
}

func fixtureFS(total uint64, cpus int, processes map[int]procFixture) fstest.MapFS {
	files := fstest.MapFS{}
	setSystemStat(files, total, cpus)
	for pid, process := range processes {
		setProcess(files, pid, process)
	}
	return files
}

func setSystemStat(files fstest.MapFS, total uint64, cpus int) {
	var b strings.Builder
	fmt.Fprintf(&b, "cpu %d 0 0 0 0 0 0 0 0 0\n", total)
	for cpu := 0; cpu < cpus; cpu++ {
		fmt.Fprintf(&b, "cpu%d %d 0 0 0 0 0 0 0 0 0\n", cpu, total/uint64(cpus))
	}
	files["stat"] = &fstest.MapFile{Data: []byte(b.String())}
}

func setProcess(files fstest.MapFS, pid int, process procFixture) {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[11] = strconv.FormatUint(process.utime, 10)
	fields[12] = strconv.FormatUint(process.stime, 10)
	fields[19] = strconv.FormatUint(process.starttime, 10)
	stat := fmt.Sprintf("%d (%s) %s\n", pid, process.comm, strings.Join(fields, " "))
	prefix := strconv.Itoa(pid)
	files[prefix+"/stat"] = &fstest.MapFile{Data: []byte(stat)}
	files[prefix+"/statm"] = &fstest.MapFile{Data: []byte(fmt.Sprintf("100 %d 0 0 0 0 0\n", process.rssPages))}
}

func deleteProcess(files fstest.MapFS, pid int) {
	prefix := strconv.Itoa(pid)
	delete(files, prefix+"/stat")
	delete(files, prefix+"/statm")
}

func processIDs(processes []Process) []int {
	ids := make([]int, len(processes))
	for i, process := range processes {
		ids[i] = process.PID
	}
	return ids
}

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 0.0001
}

type permissionFS struct {
	fs.FS
	deny string
}

func (p permissionFS) Open(name string) (fs.File, error) {
	if name == p.deny {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return p.FS.Open(name)
}

var _ fs.FS = permissionFS{}

func TestPermissionFixtureUsesPermissionError(t *testing.T) {
	_, err := fs.ReadFile(permissionFS{FS: fstest.MapFS{"x": {Data: []byte("x")}}, deny: "x"}, "x")
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("error = %v; want fs.ErrPermission", err)
	}
}
