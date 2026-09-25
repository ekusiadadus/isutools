// Package requestsql tracks completed SQL calls in one HTTP request context.
package requestsql

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type contextKey struct{}

// OverflowShape keeps calls counted when one request uses too many distinct
// normalized statements. It is not a SQL statement.
const OverflowShape = "(other SQL shapes)"
const UnknownShape = "(SQL shape unavailable)"

const maxRequestShapes = 32
const closedBit uint64 = 1 << 63

// Shape is one normalized statement observed before the request completed.
type Shape struct {
	Count  int64
	Total  time.Duration
	Errors int64
}

// Counter is a bounded, concurrent per-request count. Close freezes both the
// scalar and shape counts so late SQL cannot enter a finished request.
type Counter struct {
	state atomic.Uint64 // scalar-only fast path when shape capture is disabled
	shape *shapeCounter
}

type shapeCounter struct {
	mu     sync.Mutex
	closed bool
	count  int64
	shapes map[string]Shape
}

// WithCounter attaches a fresh counter to ctx. The caller must Close it at
// request completion, including when the handler panics.
func WithCounter(ctx context.Context) (context.Context, *Counter) {
	return withCounter(ctx, false)
}

// WithShapeCounter enables bounded SQL-shape capture for one request.
func WithShapeCounter(ctx context.Context) (context.Context, *Counter) {
	return withCounter(ctx, true)
}

func withCounter(ctx context.Context, capture bool) (context.Context, *Counter) {
	counter := &Counter{}
	if capture {
		counter.shape = &shapeCounter{}
	}
	return context.WithValue(ctx, contextKey{}, counter), counter
}

// Completed records a call without shape information. It remains for callers
// that only need the scalar count.
func Completed(ctx context.Context) { CompletedShape(ctx, UnknownShape, 0, false) }

// CompletedShape records a completed exec or query when ctx belongs to a live
// tracked request. The statement must already be normalized by sqlstats.
func CompletedShape(ctx context.Context, shape string, duration time.Duration, failed bool) {
	if ctx == nil {
		return
	}
	counter, ok := ctx.Value(contextKey{}).(*Counter)
	if !ok || counter == nil {
		return
	}
	if counter.shape == nil {
		for {
			state := counter.state.Load()
			if state&closedBit != 0 || state == math.MaxInt64 {
				return
			}
			if counter.state.CompareAndSwap(state, state+1) {
				return
			}
		}
	}
	counter.shape.mu.Lock()
	defer counter.shape.mu.Unlock()
	if counter.shape.closed {
		return
	}
	counter.shape.count++
	if shape == "" {
		return
	}
	if counter.shape.shapes == nil {
		counter.shape.shapes = make(map[string]Shape)
	}
	if _, exists := counter.shape.shapes[shape]; !exists && len(counter.shape.shapes) >= maxRequestShapes {
		shape = OverflowShape
	}
	value := counter.shape.shapes[shape]
	value.Count++
	if duration > 0 {
		value.Total += duration
	}
	if failed {
		value.Errors++
	}
	counter.shape.shapes[shape] = value
}

// Close freezes the count and returns it. Repeated calls return the same value.
func (c *Counter) Close() int64 {
	count, _ := c.CloseShapes()
	return count

}

// CloseShapes freezes the request and returns an independent shape snapshot.
func (c *Counter) CloseShapes() (int64, map[string]Shape) {
	if c == nil {
		return 0, nil
	}
	if c.shape == nil {
		for {
			state := c.state.Load()
			if state&closedBit != 0 {
				return int64(state &^ closedBit), nil
			}
			if c.state.CompareAndSwap(state, state|closedBit) {
				return int64(state), nil
			}
		}
	}
	c.shape.mu.Lock()
	defer c.shape.mu.Unlock()
	c.shape.closed = true
	shapes := make(map[string]Shape, len(c.shape.shapes))
	for key, value := range c.shape.shapes {
		shapes[key] = value
	}
	return c.shape.count, shapes
}
