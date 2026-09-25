package requestsql

import (
	"context"
	"sync"
	"testing"
)

func TestCounterClosesAgainstConcurrentCompletions(t *testing.T) {
	ctx, counter := WithCounter(context.Background())
	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Completed(ctx)
		}()
	}
	wg.Wait()
	if got := counter.Close(); got != workers {
		t.Fatalf("Close = %d, want %d", got, workers)
	}
	Completed(ctx)
	Completed(context.Background())
	if got := counter.Close(); got != workers {
		t.Fatalf("late completion changed closed count to %d", got)
	}
}
