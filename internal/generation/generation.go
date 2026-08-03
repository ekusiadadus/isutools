// Package generation provides atomic collector generation swaps. Work leases
// stay attached to the generation in which they started, and reset waits for
// those leases before freezing the old value.
package generation

import "sync"

// Frozen is an immutable-by-contract snapshot of one generation.
type Frozen[S any] struct {
	Generation int64
	Value      S
}

type slot[T any] struct {
	generation int64
	value      T
	inflight   int
}

// Manager owns a current generation and serializes resets.
type Manager[T any, S any] struct {
	mu       sync.Mutex
	cond     *sync.Cond
	resetMu  sync.Mutex
	current  *slot[T]
	factory  func() T
	snapshot func(T) S
}

// New constructs a manager whose first generation is 1.
func New[T any, S any](factory func() T, snapshot func(T) S) *Manager[T, S] {
	m := &Manager[T, S]{factory: factory, snapshot: snapshot}
	m.cond = sync.NewCond(&m.mu)
	m.current = &slot[T]{generation: 1, value: factory()}
	return m
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
		if l.slot.inflight == 0 {
			m.cond.Broadcast()
		}
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

// Snapshot takes a best-effort snapshot of the current generation. The value
// must provide its own synchronization against active leases.
func (m *Manager[T, S]) Snapshot() Frozen[S] {
	m.mu.Lock()
	s := m.current
	m.mu.Unlock()
	return Frozen[S]{Generation: s.generation, Value: m.snapshot(s.value)}
}

// SwapAndSnapshot publishes a new empty generation, waits for work pinned to
// the old generation, and then freezes the old value. Concurrent swaps are
// serialized so frozen generations are returned in order.
func (m *Manager[T, S]) SwapAndSnapshot() Frozen[S] {
	m.resetMu.Lock()
	defer m.resetMu.Unlock()

	m.mu.Lock()
	old := m.current
	m.current = &slot[T]{generation: old.generation + 1, value: m.factory()}
	for old.inflight != 0 {
		m.cond.Wait()
	}
	m.mu.Unlock()

	return Frozen[S]{Generation: old.generation, Value: m.snapshot(old.value)}
}
