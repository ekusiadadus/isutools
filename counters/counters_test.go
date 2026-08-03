package counters

import (
	"sync"
	"testing"
)

func TestAddSnapshotSorted(t *testing.T) {
	r := NewRegistry()
	r.Add("cache_miss", 3)
	r.Add("cache_hit", 10)
	r.Add("cache_hit", 5)

	got := r.Snapshot()
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0].Name != "cache_hit" || got[0].Count != 15 {
		t.Errorf("first = %+v, want cache_hit 15", got[0])
	}
	if got[1].Name != "cache_miss" || got[1].Count != 3 {
		t.Errorf("second = %+v", got[1])
	}
}

func TestReset(t *testing.T) {
	r := NewRegistry()
	r.Add("x", 1)
	r.Reset()
	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("after reset = %v", got)
	}
}

func TestConcurrentAdd(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				r.Add("n", 1)
			}
		}()
	}
	wg.Wait()
	if got := r.Snapshot()[0].Count; got != 8000 {
		t.Errorf("count = %d, want 8000", got)
	}
}
