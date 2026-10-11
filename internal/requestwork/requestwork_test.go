package requestwork

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWallPartitionAndClose(t *testing.T) {
	base := time.Now()
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	ctx, c := WithCounter(context.Background())
	CompletedSQL(ctx, at(-10), at(40))
	CompletedSQL(ctx, at(10), at(30))
	CompletedExternal(ctx, External{Label: "api", Start: at(20), End: at(70), Failed: true})
	s := c.Close(at(0), at(100))
	if s.SQLTotal != 70*time.Millisecond || s.ExternalTotal != 50*time.Millisecond || s.SQLOnly != 20*time.Millisecond || s.Overlap != 20*time.Millisecond || s.ExternalOnly != 30*time.Millisecond || s.Other != 30*time.Millisecond {
		t.Fatalf("bad summary: %+v", s)
	}
	if s.ExternalCount != 1 || s.ExternalErrors != 1 || len(s.Calls) != 1 || s.Calls[0].Label != "api" {
		t.Fatalf("bad call: %+v", s)
	}
	CompletedSQL(ctx, at(0), at(100))
	if again := c.Close(at(0), at(200)); again.SQLTotal != s.SQLTotal || again.Other != s.Other {
		t.Fatalf("late work entered closed counter: %+v", again)
	}
	s.Calls[0].Count = 42
	if c.Close(at(0), at(100)).Calls[0].Count != 1 {
		t.Fatal("Close returned mutable backing slice")
	}
}

func TestBoundsAndConcurrentCompletions(t *testing.T) {
	ctx, c := WithCounter(context.Background())
	base := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < MaxIntervals+1; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			CompletedExternal(ctx, External{Label: fmt.Sprintf("label-%d", i), Start: base, End: base.Add(time.Millisecond)})
		}(i)
	}
	wg.Wait()
	s := c.Close(base, base.Add(time.Second))
	if !s.Truncated || s.Other != 0 || s.ExternalOnly != 0 || s.ExternalCount != MaxIntervals+1 || s.ExternalTotal != (MaxIntervals+1)*time.Millisecond || len(s.Calls) > MaxLabels+1 {
		t.Fatalf("bounds: %+v", s)
	}
	if len(s.Calls) != MaxLabels+1 {
		t.Fatalf("expected bounded labels plus overflow: %d", len(s.Calls))
	}
}

func TestLongLabelIsBounded(t *testing.T) {
	ctx, c := WithCounter(context.Background())
	start := time.Now()
	CompletedExternal(ctx, External{Label: strings.Repeat("a", MaxLabelLength+10), Start: start, End: start.Add(time.Millisecond)})
	s := c.Close(start, start.Add(time.Millisecond))
	if len(s.Calls) != 1 || len(s.Calls[0].Label) != MaxLabelLength {
		t.Fatalf("unbounded label: %+v", s.Calls)
	}
}
