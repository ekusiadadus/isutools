package hoststats

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// CodeStatfsWedged marks the filesystem source as permanently given up on: the
// breaker below tripped, and no further statfs will be issued in this process.
//
// It is deliberately distinct from the ordinary "not-captured:statfs" skip.
// The two call for different readings — one boundary that ran out of budget is
// bad luck, a mount that never answers again is a broken host — and a reader
// who cannot tell them apart will keep waiting for numbers that are never
// coming back.
const CodeStatfsWedged = "statfs-wedged"

// ErrStatfsWedged is the error every statfs attempt reports once the breaker
// has tripped. It is a sentinel so callers classify with errors.Is rather than
// by matching message text.
var ErrStatfsWedged = errors.New("statfs: the mount never answered within budget; " + CodeStatfsWedged)

// Circuit breaker defaults.
//
// A wedged mount is a property of the host, not of a Collector instance: one
// process has one root filesystem and one data directory, and a second
// Collector would only queue up behind the very same syscall. The breaker is
// therefore process-wide, which is also what makes its bound meaningful —
// "at most this many stranded goroutines per process", not "per collector".
const (
	// statfsTimeoutLimit is how many consecutive boundaries may walk away from
	// statfs before the source is abandoned. It is the number of goroutines a
	// genuinely wedged mount can strand: each abandoned call stays blocked in
	// the syscall for the life of the mount, so without a limit a long tuning
	// session leaks two per run forever.
	//
	// Three rather than one, because a single timeout is also what a boundary
	// that started with almost no budget left looks like, and giving up on a
	// healthy mount would cost a real measurement.
	statfsTimeoutLimit = 3

	// statfsAttemptTimeout is the deadline one attempt gets on top of the
	// caller's context. Zero means the caller's budget is the only deadline,
	// which is the production setting: runctl.PerCollectorBaselineBudget
	// already decides how long a boundary may take, and a second, tighter
	// deadline invented here would shrink the measurement window without
	// bounding anything the breaker does not already bound. Tests inject a
	// short one so they never wait out a real budget.
	statfsAttemptTimeout = 0
)

// processStatfsBreaker is the breaker the production path uses. Its state is
// mutable by construction — a circuit breaker is a state machine — and every
// transition goes through the mutex-guarded methods below.
var processStatfsBreaker = newStatfsBreaker(statfsAttemptTimeout, statfsTimeoutLimit)

// statfsBreaker bounds what one wedged mount can cost.
//
// statfs(2) is uninterruptible: on an NFS server that stopped answering or a
// fuse daemon that died, it blocks for as long as the mount is wedged, and no
// context can cut it short. The only way to keep a boundary inside its budget
// is to abandon the call on its own goroutine — and the only way to keep the
// abandoned goroutines from accumulating over a session is to stop issuing the
// call at all once the mount has proved it is not coming back.
type statfsBreaker struct {
	// timeout is the per-attempt deadline, or zero for "the caller's only".
	timeout time.Duration
	// limit is how many consecutive timeouts trip the breaker. Always >= 1.
	limit int

	mu sync.Mutex
	// consecutive counts timeouts since the last answer of any kind.
	consecutive int
	// tripped is one-way: a mount that wedged mid-session is not probed again,
	// because probing is exactly what costs a goroutine.
	tripped bool
}

// newStatfsBreaker builds a breaker with an injected deadline and limit, so a
// test can drive the whole state machine in milliseconds.
func newStatfsBreaker(timeout time.Duration, limit int) *statfsBreaker {
	if limit < 1 {
		limit = 1
	}
	return &statfsBreaker{timeout: timeout, limit: limit}
}

// wedged reports whether the source has been given up on.
func (b *statfsBreaker) wedged() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tripped
}

// recordTimeout counts one abandoned attempt and trips the breaker once the
// streak reaches the limit.
func (b *statfsBreaker) recordTimeout() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive++
	if b.consecutive >= b.limit {
		b.tripped = true
	}
}

// recordAnswer clears the streak. Any completion counts, including a failed
// one: a permission error proves the mount answered, which is the only thing
// the streak is about.
func (b *statfsBreaker) recordAnswer() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
}

// read reads filesystem usage without letting a wedged mount hold the
// boundary, and without letting a permanently wedged one strand a goroutine
// per boundary for the rest of the session.
//
// Abandoning a read leaks nothing but the syscall itself: the channel is
// buffered, so the goroutine delivers into a receiver that is already gone and
// exits instead of parking forever. Once the breaker has tripped, no goroutine
// is spawned at all — the source degrades to ErrStatfsWedged immediately,
// exactly like a permission error, while the rest of the sample is kept.
func (b *statfsBreaker) read(ctx context.Context, statfs func(string) (FSRaw, error), dataDir string) (map[string]FSRaw, error) {
	if b.wedged() {
		return nil, ErrStatfsWedged
	}
	if b.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.timeout)
		defer cancel()
	}

	type outcome struct {
		filesystems map[string]FSRaw
		err         error
	}
	done := make(chan outcome, 1)
	go func() {
		filesystems, err := readFilesystems(statfs, dataDir)
		done <- outcome{filesystems: filesystems, err: err}
	}()
	select {
	case out := <-done:
		b.recordAnswer()
		return out.filesystems, out.err
	case <-ctx.Done():
		b.recordTimeout()
		return nil, fmt.Errorf("statfs: %w", ctx.Err())
	}
}

// readFilesystemsWithin is the boundary's entry point, on the process-wide
// breaker. It exists as a free function so that capture stays a straight line
// and the breaker's ownership is stated in exactly one place.
func readFilesystemsWithin(ctx context.Context, statfs func(string) (FSRaw, error), dataDir string) (map[string]FSRaw, error) {
	return processStatfsBreaker.read(ctx, statfs, dataDir)
}

// readFilesystems runs statfs against the root filesystem and, when
// configured, the data directory. Those two answer different questions — the
// root filling up kills the machine, the data volume filling up kills the
// database — so both are reported and neither is inferred from the other.
//
// One unreadable path is skipped; an error is returned only when no path could
// be read at all.
func readFilesystems(statfs func(string) (FSRaw, error), dataDir string) (map[string]FSRaw, error) {
	if statfs == nil {
		return nil, errors.New("statfs: no implementation")
	}
	out := make(map[string]FSRaw, 2)
	for _, target := range []string{"/", dataDir} {
		if target == "" {
			continue
		}
		if _, done := out[target]; done {
			continue
		}
		raw, err := statfs(target)
		if err != nil {
			continue
		}
		out[target] = raw
	}
	if len(out) == 0 {
		return nil, errors.New("statfs: no path readable")
	}
	return out, nil
}

// mulSaturate multiplies without wrapping. A wrapped byte count reads as a
// plausible small number, which is worse than an obviously pinned one.
func mulSaturate(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxUint64/b {
		return math.MaxUint64
	}
	return a * b
}
