package agg

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestObserveAccumulates(t *testing.T) {
	tbl := NewTable(DefaultMaxKeys)
	tbl.Observe("q1", 10*time.Millisecond)
	tbl.Observe("q1", 30*time.Millisecond)

	entries := tbl.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Key != "q1" {
		t.Errorf("key = %q, want q1", e.Key)
	}
	if e.Count != 2 {
		t.Errorf("count = %d, want 2", e.Count)
	}
	if e.Total != 40*time.Millisecond {
		t.Errorf("total = %v, want 40ms", e.Total)
	}
	if e.Avg != 20*time.Millisecond {
		t.Errorf("avg = %v, want 20ms", e.Avg)
	}
	if e.Max != 30*time.Millisecond {
		t.Errorf("max = %v, want 30ms", e.Max)
	}
}

func TestSnapshotSortedByTotalDesc(t *testing.T) {
	tbl := NewTable(DefaultMaxKeys)
	tbl.Observe("small", 1*time.Millisecond)
	tbl.Observe("big", 100*time.Millisecond)
	tbl.Observe("mid", 10*time.Millisecond)

	entries := tbl.Snapshot()
	want := []string{"big", "mid", "small"}
	if len(entries) != len(want) {
		t.Fatalf("want %d entries, got %d", len(want), len(entries))
	}
	for i, k := range want {
		if entries[i].Key != k {
			t.Errorf("entries[%d].Key = %q, want %q", i, entries[i].Key, k)
		}
	}
}

func TestKeyCapOverflowsToOther(t *testing.T) {
	tbl := NewTable(2)
	tbl.Observe("a", time.Millisecond)
	tbl.Observe("b", time.Millisecond)
	tbl.Observe("c", time.Millisecond) // exceeds cap -> (other)
	tbl.Observe("d", time.Millisecond) // exceeds cap -> (other)
	tbl.Observe("a", time.Millisecond) // existing key still updates

	entries := tbl.Snapshot()
	byKey := map[string]Entry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}
	if byKey["a"].Count != 2 {
		t.Errorf("a.count = %d, want 2", byKey["a"].Count)
	}
	other, ok := byKey[OverflowKey]
	if !ok {
		t.Fatalf("missing %q entry: %v", OverflowKey, entries)
	}
	if other.Count != 2 {
		t.Errorf("other.count = %d, want 2", other.Count)
	}
}

func TestP95Approximation(t *testing.T) {
	tbl := NewTable(DefaultMaxKeys)
	for i := 0; i < 95; i++ {
		tbl.Observe("q", 1*time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		tbl.Observe("q", 100*time.Millisecond)
	}
	e := tbl.Snapshot()[0]
	if e.P95 < 1*time.Millisecond || e.P95 > 4*time.Millisecond {
		t.Errorf("p95 = %v, want log2-bucket approximation of ~1ms (1ms..4ms)", e.P95)
	}
	if e.Max < 100*time.Millisecond {
		t.Errorf("max = %v, want >= 100ms", e.Max)
	}
}

func TestReset(t *testing.T) {
	tbl := NewTable(2)
	tbl.Observe("a", time.Millisecond)
	tbl.Observe("b", time.Millisecond)
	tbl.Observe("c", time.Millisecond) // overflow
	tbl.Reset()
	if got := tbl.Snapshot(); len(got) != 0 {
		t.Fatalf("after reset want 0 entries, got %v", got)
	}
	// key budget must be restored after reset
	tbl.Observe("x", time.Millisecond)
	tbl.Observe("y", time.Millisecond)
	entries := tbl.Snapshot()
	for _, e := range entries {
		if e.Key == OverflowKey {
			t.Errorf("fresh keys after reset must not overflow: %v", entries)
		}
	}
}

func TestConcurrentObserve(t *testing.T) {
	tbl := NewTable(DefaultMaxKeys)
	var wg sync.WaitGroup
	const workers, perWorker = 8, 1000
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				tbl.Observe(fmt.Sprintf("q%d", i%10), time.Microsecond)
			}
		}(w)
	}
	wg.Wait()
	var total int64
	for _, e := range tbl.Snapshot() {
		total += e.Count
	}
	if total != workers*perWorker {
		t.Errorf("total count = %d, want %d", total, workers*perWorker)
	}
}

func BenchmarkObserve(b *testing.B) {
	tbl := NewTable(DefaultMaxKeys)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tbl.Observe("SELECT * FROM comments WHERE post_id = ? ORDER BY created_at DESC LIMIT 3", time.Millisecond)
		}
	})
}
