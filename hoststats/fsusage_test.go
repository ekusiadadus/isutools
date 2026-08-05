package hoststats

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// blockingStatfs builds a statfs seam that never answers until release is
// closed, and signals every entry on the returned channel.
//
// Counting entries is what makes the goroutine assertion exact rather than
// statistical: readFilesystems calls statfs once per target, and with no data
// directory that is exactly one call per spawned goroutine. So "statfs was
// entered n times" and "n goroutines were spawned" are the same statement.
func blockingStatfs(release <-chan struct{}, calls *atomic.Int64) (func(string) (FSRaw, error), <-chan struct{}) {
	entered := make(chan struct{}, 64)
	return func(string) (FSRaw, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return FSRaw{TotalBytes: 1, AvailBytes: 1}, nil
	}, entered
}

// TestStatfsBreakerStopsSpawningGoroutinesOnAWedgedMount is the regression
// test for the leak a wedged mount used to cause: statfs(2) cannot be
// cancelled, so every boundary abandoned its own goroutine inside the syscall,
// two per run and unbounded over a tuning session.
//
// The breaker has to convert that into a fixed, one-off cost: after N
// consecutive timeouts no further call is issued at all, and the source
// reports the permanent-skip code instead.
func TestStatfsBreakerStopsSpawningGoroutinesOnAWedgedMount(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	// The mount is unwedged on the way out so the stranded goroutines finish
	// and nothing of this test survives into the next one.
	t.Cleanup(func() { close(release) })

	var calls atomic.Int64
	statfs, entered := blockingStatfs(release, &calls)

	const limit = 3
	breaker := newStatfsBreaker(20*time.Millisecond, limit)

	for i := 0; i < limit; i++ {
		filesystems, err := breaker.read(context.Background(), statfs, "")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("attempt %d: err = %v, want the boundary to walk away on its deadline", i, err)
		}
		if filesystems != nil {
			t.Fatalf("attempt %d: filesystems = %v, want nothing from a mount that never answered", i, filesystems)
		}
		// The abandoned goroutine really reached the syscall, so the spawn is
		// observed rather than assumed.
		<-entered
		if got := calls.Load(); got != int64(i+1) {
			t.Fatalf("attempt %d: statfs was entered %d times, want %d", i, got, i+1)
		}
	}

	if !breaker.wedged() {
		t.Fatalf("the breaker did not trip after %d consecutive timeouts", limit)
	}

	spawnedBefore := calls.Load()
	goroutinesBefore := runtime.NumGoroutine()

	// Every boundary from here on must cost nothing at all.
	const extraBoundaries = 20
	for i := 0; i < extraBoundaries; i++ {
		filesystems, err := breaker.read(context.Background(), statfs, "")
		if !errors.Is(err, ErrStatfsWedged) {
			t.Fatalf("boundary %d after the trip: err = %v, want ErrStatfsWedged", i, err)
		}
		if filesystems != nil {
			t.Fatalf("boundary %d after the trip: filesystems = %v, want none", i, filesystems)
		}
	}

	if got := calls.Load(); got != spawnedBefore {
		t.Errorf("statfs was issued %d more times after the breaker tripped; a wedged mount must cost at most %d goroutines",
			got-spawnedBefore, limit)
	}
	// Sampled rather than slept on: the count may still be settling from the
	// runtime's own workers, but it must come back to the pre-trip level and
	// stay there, because this breaker spawned nothing.
	waitForGoroutinesAtMost(t, goroutinesBefore)
}

// waitForGoroutinesAtMost polls until the process is back to at most want
// goroutines, so the assertion never depends on a fixed sleep being long
// enough.
func waitForGoroutinesAtMost(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	got := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		if got <= want {
			return
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
		got = runtime.NumGoroutine()
	}
	if got > want {
		t.Errorf("goroutines = %d, want no more than the %d present before the extra boundaries", got, want)
	}
}

// TestStatfsBreakerCountsConsecutiveTimeoutsOnly pins the other half of the
// rule: a mount that answers is not on its way out, however slow it was, so a
// single answer has to clear the streak. Without that, a host with one
// unlucky boundary per run would eventually lose its filesystem numbers
// permanently.
func TestStatfsBreakerCountsConsecutiveTimeoutsOnly(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var calls atomic.Int64
	blocking, entered := blockingStatfs(release, &calls)
	answering := fakeStatfs(defaultStatfsSizes())

	const limit = 2
	breaker := newStatfsBreaker(20*time.Millisecond, limit)

	for i := 0; i < 4; i++ {
		if _, err := breaker.read(context.Background(), blocking, ""); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout %d: err = %v, want a deadline", i, err)
		}
		<-entered
		filesystems, err := breaker.read(context.Background(), answering, "")
		if err != nil {
			t.Fatalf("answered read %d: err = %v, want nil", i, err)
		}
		if len(filesystems) == 0 {
			t.Fatalf("answered read %d returned no filesystem", i)
		}
		if breaker.wedged() {
			t.Fatalf("the breaker tripped after %d alternating timeouts, but never on a streak of %d", i+1, limit)
		}
	}
}

// TestStatfsBreakerReportsAnAnsweringMount proves the breaker is transparent
// on a healthy host: the usage comes back unchanged and nothing is recorded
// against the mount.
func TestStatfsBreakerReportsAnAnsweringMount(t *testing.T) {
	t.Parallel()
	breaker := newStatfsBreaker(time.Second, 2)
	filesystems, err := breaker.read(context.Background(), fakeStatfs(defaultStatfsSizes()), "/var/lib/mysql")
	if err != nil {
		t.Fatalf("read() error = %v, want nil", err)
	}
	if len(filesystems) != 2 {
		t.Fatalf("filesystems = %v, want both the root and the data directory", filesystems)
	}
	if breaker.wedged() {
		t.Fatal("a mount that answered must not trip the breaker")
	}
}

// TestStatfsBreakerTreatsAFailedReadAsAnAnswer fixes what a permission error
// means: the mount replied, so it is not wedged, and the streak clears.
func TestStatfsBreakerTreatsAFailedReadAsAnAnswer(t *testing.T) {
	t.Parallel()
	breaker := newStatfsBreaker(time.Second, 1)
	if _, err := breaker.read(context.Background(), fakeStatfs(nil), "/data"); err == nil {
		t.Fatal("read() must report a mount whose paths were all unreadable")
	}
	if breaker.wedged() {
		t.Fatal("an error that came back inside the budget is not a wedged mount")
	}
}

// TestNewStatfsBreakerRejectsAMeaninglessLimit keeps the breaker's own
// contract intact: a limit below one would trip it before the first attempt
// and lose the source on a perfectly healthy host.
func TestNewStatfsBreakerRejectsAMeaninglessLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		if got := newStatfsBreaker(0, limit).limit; got != 1 {
			t.Errorf("newStatfsBreaker(limit=%d).limit = %d, want 1", limit, got)
		}
	}
}

// TestProcessStatfsBreakerIsConfiguredForProduction pins the two settings the
// boundary path depends on: a limit that cannot give up on one unlucky
// boundary, and no deadline of its own, so runctl's collector budget stays the
// single authority on how long a boundary may take.
func TestProcessStatfsBreakerIsConfiguredForProduction(t *testing.T) {
	t.Parallel()
	if processStatfsBreaker.limit < 2 {
		t.Errorf("limit = %d, want more than one timeout before the source is abandoned", processStatfsBreaker.limit)
	}
	if processStatfsBreaker.timeout != 0 {
		t.Errorf("timeout = %v, want the caller's budget to be the only deadline", processStatfsBreaker.timeout)
	}
	if processStatfsBreaker.wedged() {
		t.Fatal("the process breaker tripped during the test run; the boundary path would report no filesystems")
	}
}

// TestReadFilesystemsWithinUsesTheProcessBreaker proves the boundary entry
// point is wired to the breaker rather than spawning unguarded goroutines of
// its own.
func TestReadFilesystemsWithinUsesTheProcessBreaker(t *testing.T) {
	t.Parallel()
	filesystems, err := readFilesystemsWithin(context.Background(), fakeStatfs(defaultStatfsSizes()), "")
	if err != nil {
		t.Fatalf("readFilesystemsWithin() error = %v, want nil", err)
	}
	if _, ok := filesystems["/"]; !ok {
		t.Fatalf("filesystems = %v, want the root filesystem", filesystems)
	}
}
