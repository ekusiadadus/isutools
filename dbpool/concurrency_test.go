package dbpool

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentWatchAndCapture runs the watch-set mutations against a
// boundary, which is the real ordering: WatchDBPool is called from application
// start-up code while a reset may already be in flight. One mutex covers the
// watch set, the farewell samples and the run cache, so the boundary sees a
// watch set that is either wholly before or wholly after each mutation — never
// a pool that is half joined. Under -race this also proves there is no
// unsynchronised access.
func TestConcurrentWatchAndCapture(t *testing.T) {
	c, _ := newTestCollector()
	const pools = 8

	// Half the pools are watched before the run so there is always something
	// for the boundary to find.
	for i := 0; i < pools/2; i++ {
		id := fmt.Sprintf("pool-%02d", i)
		if err := c.watchStats(id, appDisplay, newScript(sql.DBStats{WaitCount: 1}, sql.DBStats{WaitCount: 4}).stats); err != nil {
			t.Fatalf("watchStats(%q) = %v, want nil", id, err)
		}
	}

	var wg sync.WaitGroup
	for i := pools / 2; i < pools; i++ {
		id := fmt.Sprintf("pool-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.watchStats(id, appDisplay, newScript(sql.DBStats{WaitCount: 2}, sql.DBStats{WaitCount: 9}).stats)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.unwatchStats("pool-00")
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.Watched()
		_ = c.Notes()
	}()

	baseResult, err := c.CaptureBaseline(context.Background(), "run-1", 1)
	if err != nil {
		t.Fatalf("CaptureBaseline = %v, want nil", err)
	}
	wg.Wait()

	finalResult := mustCapture(t, c.CaptureFinal, "run-1", 1)
	entries := mustCollect(t, c, baseResult, finalResult)

	base, err := sampleOf(baseResult.Handle, "baseline")
	if err != nil {
		t.Fatalf("sampleOf = %v, want nil", err)
	}
	if len(entries) != len(base) {
		t.Fatalf("entries = %d, want one per baseline participant (%d)", len(entries), len(base))
	}
	for _, entry := range entries {
		if _, ok := base[entry.TargetID]; !ok {
			t.Fatalf("entry %q was reported without a baseline", entry.TargetID)
		}
		if entry.BaselineAt.IsZero() || entry.FinalAt.IsZero() {
			t.Fatalf("entry %+v has an open-ended interval", entry)
		}
	}
}
