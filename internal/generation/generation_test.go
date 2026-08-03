package generation

import (
	"sync"
	"testing"
	"time"
)

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) add(n int) {
	c.mu.Lock()
	c.n += n
	c.mu.Unlock()
}

func (c *counter) snapshot() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func newCounterManager() *Manager[*counter, int] {
	return New(
		func() *counter { return &counter{} },
		func(c *counter) int { return c.snapshot() },
	)
}

func TestStartsAtGenerationOne(t *testing.T) {
	m := newCounterManager()
	got := m.Snapshot()
	if got.Generation != 1 || got.Value != 0 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestSwapWaitsForOldLeaseAndNewWorkUsesNewGeneration(t *testing.T) {
	m := newCounterManager()
	old := m.Acquire()
	old.Value().add(2)

	result := make(chan Frozen[int], 1)
	go func() { result <- m.SwapAndSnapshot() }()

	deadline := time.Now().Add(time.Second)
	for m.CurrentGeneration() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.CurrentGeneration() != 2 {
		t.Fatal("reset did not publish generation 2")
	}

	current := m.Acquire()
	if current.Generation() != 2 {
		t.Fatalf("new lease generation = %d, want 2", current.Generation())
	}
	current.Value().add(7)
	current.Done()

	select {
	case <-result:
		t.Fatal("swap returned before old in-flight measurement completed")
	default:
	}

	old.Value().add(3)
	old.Done()
	select {
	case frozen := <-result:
		if frozen.Generation != 1 || frozen.Value != 5 {
			t.Fatalf("frozen = %#v, want generation 1 value 5", frozen)
		}
	case <-time.After(time.Second):
		t.Fatal("swap did not finish after old lease completed")
	}

	got := m.Snapshot()
	if got.Generation != 2 || got.Value != 7 {
		t.Fatalf("current = %#v, want generation 2 value 7", got)
	}
}

func TestLeaseDoneIsIdempotent(t *testing.T) {
	m := newCounterManager()
	lease := m.Acquire()
	lease.Value().add(1)
	lease.Done()
	lease.Done()
	frozen := m.SwapAndSnapshot()
	if frozen.Value != 1 {
		t.Fatalf("frozen value = %d, want 1", frozen.Value)
	}
}

func TestConcurrentSwapsAreSerialized(t *testing.T) {
	m := newCounterManager()
	old := m.Acquire()

	results := make(chan Frozen[int], 2)
	go func() { results <- m.SwapAndSnapshot() }()
	deadline := time.Now().Add(time.Second)
	for m.CurrentGeneration() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	go func() { results <- m.SwapAndSnapshot() }()

	time.Sleep(10 * time.Millisecond)
	if got := m.CurrentGeneration(); got != 2 {
		t.Fatalf("second swap overtook first: current generation = %d", got)
	}
	old.Done()

	a := <-results
	b := <-results
	if a.Generation != 1 || b.Generation != 2 {
		t.Fatalf("swap results = %#v then %#v", a, b)
	}
}
