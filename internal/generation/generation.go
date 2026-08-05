// Package generation provides atomic collector generation swaps. Work leases
// stay attached to the generation in which they started, and a swap seals the
// old generation so its value can be frozen once every lease it pinned is
// done.
//
// Waiting for a sealed generation is a channel receive rather than a
// sync.Cond: a lease that never completes — a SQL query that never returns —
// must not be able to park the waiting goroutine forever. sync.Cond.Wait
// cannot be interrupted by a context, so a cond-based wait makes the
// runctl.GenerationCollector drain contract impossible to honour.
package generation

import (
	"context"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// DefaultCompatWait bounds SwapAndSnapshot, the context-free compatibility
// entry point. Work that never completes must not park its caller for the
// lifetime of the process, so the shim eventually gives up and freezes what
// the sealed generation holds at that moment. Callers that have a context
// should use SwapAndSnapshotContext, or Swap and Sealed.Wait, which abandon
// the wait exactly when the caller says so.
//
// It is runctl.DrainBudget because the compatibility swap is exactly the drain
// step of a run boundary expressed through the pre-generation API, and the two
// must not be able to disagree about how long work that never finishes is
// worth waiting for. runctl is the single authority for these numbers;
// inventing an independent one here is what made "why did my collector get cut
// off?" have two answers. TestDefaultCompatWaitIsTheSharedDrainBudget fails if
// the two ever diverge again.
const DefaultCompatWait = runctl.DrainBudget

// Frozen is an immutable-by-contract snapshot of one generation.
type Frozen[S any] struct {
	Generation int64
	Value      S
	// CutShort reports that the snapshot was taken while work pinned to the
	// generation was still in flight, so it may be missing rows that work would
	// have contributed. It is the difference between a section that is complete
	// and one that is partial, and a caller that cannot see it reports a
	// truncated measurement as a whole one.
	//
	// It is conservative in one direction only: work that finishes during the
	// snapshot can produce CutShort with complete data, but complete data is
	// never the reason CutShort is false.
	CutShort bool
}

// slot is one generation: a value plus the bookkeeping that decides when the
// work pinned to it has finished.
type slot[T any] struct {
	generation int64
	value      T

	// inflight and sealed are guarded by the owning manager's mu.
	inflight int
	sealed   bool

	// done is closed exactly once, when the slot is both sealed and empty.
	// Waiters select on it against their own context.
	done      chan struct{}
	closeOnce sync.Once
}

// sealLocked marks s as no longer current. From this moment inflight can only
// fall, because Acquire pins new work to the manager's current slot. Callers
// hold the manager's mu.
func (s *slot[T]) sealLocked() {
	s.sealed = true
	s.settleLocked()
}

// settleLocked closes the completion channel when, and only when, the slot is
// both sealed and empty. The condition lives here rather than at the call
// sites so that no caller can close a live generation: a live one drops to
// zero in-flight between leases all the time, and closing it there would
// panic on the second close or — once guarded — make a later Wait return
// while work is still writing to the generation it claims to have settled.
// Callers hold the manager's mu.
func (s *slot[T]) settleLocked() {
	if !s.sealed || s.inflight != 0 {
		return
	}
	s.closeOnce.Do(func() { close(s.done) })
}

// Manager owns a current generation and serializes compatibility resets.
type Manager[T any, S any] struct {
	mu       sync.Mutex
	resetMu  sync.Mutex
	current  *slot[T]
	factory  func() T
	snapshot func(T) S

	// compatWait overrides DefaultCompatWait for SwapAndSnapshot. It is the
	// per-manager injection point, guarded by mu so a test may shorten it on a
	// manager that is already shared. Zero means the default, so a manager built
	// by New is already bounded.
	compatWait time.Duration
}

// New constructs a manager whose first generation is 1.
func New[T any, S any](factory func() T, snapshot func(T) S) *Manager[T, S] {
	m := &Manager[T, S]{factory: factory, snapshot: snapshot}
	m.current = m.newSlot(1)
	return m
}

// newSlot builds an empty generation. New calls it before the manager is
// reachable; every other caller holds mu.
func (m *Manager[T, S]) newSlot(generation int64) *slot[T] {
	return &slot[T]{generation: generation, value: m.factory(), done: make(chan struct{})}
}

// Lease pins work to the generation in which it started.
type Lease[T any, S any] struct {
	manager *Manager[T, S]
	slot    *slot[T]
	done    sync.Once
}

// Acquire pins and returns the current generation.
func (m *Manager[T, S]) Acquire() *Lease[T, S] {
	m.mu.Lock()
	s := m.current
	s.inflight++
	m.mu.Unlock()
	return &Lease[T, S]{manager: m, slot: s}
}

// Value returns the mutable collector owned by the pinned generation.
func (l *Lease[T, S]) Value() T { return l.slot.value }

// Generation returns the pinned generation number.
func (l *Lease[T, S]) Generation() int64 { return l.slot.generation }

// Done releases the lease. It is safe to call more than once.
func (l *Lease[T, S]) Done() {
	l.done.Do(func() {
		m := l.manager
		m.mu.Lock()
		l.slot.inflight--
		l.slot.settleLocked()
		m.mu.Unlock()
	})
}

// CurrentGeneration returns the generation accepting new work.
func (m *Manager[T, S]) CurrentGeneration() int64 {
	m.mu.Lock()
	generation := m.current.generation
	m.mu.Unlock()
	return generation
}

// SetCompatWait bounds the wait performed by SwapAndSnapshot. A non-positive
// value restores DefaultCompatWait. It exists for callers whose own operation
// budget is tighter than the run controller's, and for tests that must exercise
// the give-up path without waiting out a real drain budget; the default is
// already bounded, so leaving it alone is safe.
func (m *Manager[T, S]) SetCompatWait(wait time.Duration) {
	m.mu.Lock()
	m.compatWait = wait
	m.mu.Unlock()
}

// compatWaitOrDefault reports the bound SwapAndSnapshot should use.
func (m *Manager[T, S]) compatWaitOrDefault() time.Duration {
	m.mu.Lock()
	wait := m.compatWait
	m.mu.Unlock()
	if wait <= 0 {
		return DefaultCompatWait
	}
	return wait
}

// Snapshot takes a best-effort snapshot of the current generation. The value
// must provide its own synchronization against active leases.
//
// CutShort is false: the current generation is still accepting work, so there
// was no bounded wait for it to give up on. A live snapshot is incomplete by
// construction, which is a different thing from a drain that was truncated.
func (m *Manager[T, S]) Snapshot() Frozen[S] {
	m.mu.Lock()
	s := m.current
	m.mu.Unlock()
	return Frozen[S]{Generation: s.generation, Value: m.snapshot(s.value)}
}

// Sealed is a generation that has been swapped out of the current position. It
// accepts no new work, so its in-flight count can only fall.
type Sealed[T any, S any] struct {
	manager *Manager[T, S]
	slot    *slot[T]
}

// Generation returns the sealed generation's number.
func (s Sealed[T, S]) Generation() int64 { return s.slot.generation }

// Settled reports whether every lease pinned to the sealed generation is done,
// without waiting.
func (s Sealed[T, S]) Settled() bool {
	select {
	case <-s.slot.done:
		return true
	default:
		return false
	}
}

// Wait blocks until every lease pinned to the sealed generation is done, or
// until ctx is done, whichever comes first. A nil context waits indefinitely.
//
// Abandoning the wait leaves no goroutine behind and costs the caller nothing
// later: work that finishes after the wait was abandoned writes to its own
// sealed value and to nothing else. Completion wins over an already-expired
// context, because a generation with nothing left in flight has nothing to
// wait for and reporting a timeout for it would drop complete data.
func (s Sealed[T, S]) Wait(ctx context.Context) error {
	select {
	case <-s.slot.done:
		return nil
	default:
	}
	if ctx == nil {
		<-s.slot.done
		return nil
	}
	select {
	case <-s.slot.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Freeze snapshots the sealed generation's value. After a Wait that returned
// nil the value is fixed; called earlier it is a best-effort read that relies
// on the value's own synchronization, exactly like Snapshot. Frozen.CutShort
// tells the two apart, so a caller that froze on a give-up can mark its section
// partial instead of publishing a truncated measurement as a whole one.
func (s Sealed[T, S]) Freeze() Frozen[S] {
	// The settle state is read before the value, never after. Work that finishes
	// between the two reads may or may not appear in the snapshot; labelling a
	// snapshot that caught every row "partial" costs a warning, while labelling
	// one that missed a row "complete" costs the truth.
	settled := s.Settled()
	return Frozen[S]{
		Generation: s.slot.generation,
		Value:      s.manager.snapshot(s.slot.value),
		CutShort:   !settled,
	}
}

// Swap publishes a new empty generation and returns the previous one, sealed.
// It only moves a pointer, so it never blocks on in-flight work: the caller
// decides how long to wait for the sealed generation, and with which context.
func (m *Manager[T, S]) Swap() Sealed[T, S] {
	m.mu.Lock()
	old := m.current
	m.current = m.newSlot(old.generation + 1)
	old.sealLocked()
	m.mu.Unlock()
	return Sealed[T, S]{manager: m, slot: old}
}

// SwapAndSnapshot publishes a new empty generation, waits for work pinned to
// the old generation, and then freezes the old value. Concurrent swaps are
// serialized so frozen generations are returned in order.
//
// It is the compatibility entry point for callers with no context at all. A
// caller that has one should use SwapAndSnapshotContext: this one can only be
// bounded by the manager's own budget, so a caller whose request was cancelled
// long ago still pays for the whole of it.
func (m *Manager[T, S]) SwapAndSnapshot() Frozen[S] {
	return m.SwapAndSnapshotContext(context.Background())
}

// SwapAndSnapshotContext is SwapAndSnapshot bounded by the caller's context as
// well as by the manager's budget, whichever ends first. A nil context means
// the budget alone.
//
// The wait is bounded in both directions for the same reason: this call runs on
// the /reset path, which holds the process-wide reset lock and the operation
// slot, so work that never finishes — a query that never returns — would
// otherwise head-of-line-block every other admin endpoint for the whole bound.
// Consulting the caller's context is what lets an abandoned request stop paying
// for it, and the returned Frozen.CutShort is what lets the caller say the
// section it got is partial.
func (m *Manager[T, S]) SwapAndSnapshotContext(ctx context.Context) Frozen[S] {
	m.resetMu.Lock()
	defer m.resetMu.Unlock()

	sealed := m.Swap()
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, m.compatWaitOrDefault())
	defer cancel()
	// A wait that gives up still freezes: measurement is fail-open, so partial
	// data beats no data and beats blocking the caller forever. The error is not
	// consulted because Freeze derives CutShort from the generation's own settle
	// state, which stays accurate when work lands between the two.
	_ = sealed.Wait(waitCtx)
	return sealed.Freeze()
}
