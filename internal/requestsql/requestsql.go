// Package requestsql tracks completed SQL calls in one HTTP request context.
package requestsql

import (
	"context"
	"math"
	"sync/atomic"
)

type contextKey struct{}

const closedBit uint64 = 1 << 63

// Counter is a bounded, concurrent per-request count. Close freezes the value;
// SQL calls completing after the request has ended cannot change it.
type Counter struct {
	state atomic.Uint64
}

// WithCounter attaches a fresh counter to ctx. The caller must Close it at
// request completion, including when the handler panics.
func WithCounter(ctx context.Context) (context.Context, *Counter) {
	counter := &Counter{}
	return context.WithValue(ctx, contextKey{}, counter), counter
}

// Completed records one completed exec or query, if ctx belongs to a live
// tracked request. Calls without a request context have no effect.
func Completed(ctx context.Context) {
	if ctx == nil {
		return
	}
	counter, ok := ctx.Value(contextKey{}).(*Counter)
	if !ok || counter == nil {
		return
	}
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

// Close freezes the count and returns it. Repeated calls return the same value.
func (c *Counter) Close() int64 {
	if c == nil {
		return 0
	}
	for {
		state := c.state.Load()
		if state&closedBit != 0 {
			return int64(state &^ closedBit)
		}
		if c.state.CompareAndSwap(state, state|closedBit) {
			return int64(state)
		}
	}
}
