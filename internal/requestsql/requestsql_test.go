package requestsql

import (
	"context"
	"sync"
	"testing"
	"time"
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

func TestShapesRemainBoundedAndPreserveCounts(t *testing.T) {
	ctx, counter := WithShapeCounter(context.Background())
	for i := 0; i < maxRequestShapes+3; i++ {
		CompletedShape(ctx, string(rune('a'+i)), time.Millisecond, i == 0)
	}
	count, shapes := counter.CloseShapes()
	if count != maxRequestShapes+3 || len(shapes) != maxRequestShapes+1 {
		t.Fatalf("count=%d shapes=%d", count, len(shapes))
	}
	if got := shapes[OverflowShape]; got.Count != 3 || got.Total != 3*time.Millisecond {
		t.Fatalf("overflow = %+v", got)
	}
	var summed int64
	for _, shape := range shapes {
		summed += shape.Count
	}
	if summed != count || shapes["a"].Errors != 1 {
		t.Fatalf("shape sum=%d count=%d errors=%d", summed, count, shapes["a"].Errors)
	}
	CompletedShape(ctx, "late", time.Second, false)
	if after, _ := counter.CloseShapes(); after != count {
		t.Fatalf("late SQL changed count: %d", after)
	}
}

func TestShapeCounterConcurrentCompletionsAndLateCall(t *testing.T) {
	ctx, counter := WithShapeCounter(context.Background())
	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			CompletedShape(ctx, "SELECT ?", time.Microsecond, false)
		}()
	}
	wg.Wait()
	count, shapes := counter.CloseShapes()
	if count != workers || shapes["SELECT ?"].Count != workers {
		t.Fatalf("count=%d shapes=%+v", count, shapes)
	}
	CompletedShape(ctx, "SELECT ?", time.Microsecond, false)
	if after, later := counter.CloseShapes(); after != count || later["SELECT ?"].Count != count {
		t.Fatalf("late query changed shapes: count=%d shapes=%+v", after, later)
	}
}
