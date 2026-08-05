// Package dbpool reports database/sql connection-pool statistics for one
// measurement run.
//
// The pool is the narrowest part of most ISUCON application stacks. When
// SetMaxOpenConns is smaller than the number of concurrent handlers, requests
// queue *inside* database/sql, where neither the SQL statistics nor the HTTP
// handler timings can see them: every individual query looks fast while the
// request that issued it is slow. Reporting WaitCount and WaitDuration over
// the run interval is what makes that invisible queue visible.
//
// This version displays numbers only. There is deliberately no advisor
// threshold yet:
//
//   - WaitDuration is the sum of every goroutine's wait, so comparing it with
//     the wall-clock length of the interval says nothing (64 concurrent
//     waiters can accumulate 64x the interval without anything being wrong).
//   - "The pool was full at snapshot time" and "somebody waited earlier in the
//     run" are two unrelated observations that cannot be chained into a cause.
//   - "Raise the limit" is not a safe conclusion: a larger pool can simply
//     move the saturation into the database server.
//
// Thresholds are to be derived from real measurements and added separately.
//
// Collector implements runctl.BaselineCollector: it samples every watched pool
// at the run's opening and closing boundary and derives the interval from
// those two frozen samples alone. (*sql.DB).Stats does no I/O — it takes the
// handle's own mutex once — so a boundary sample costs microseconds even with
// MaxPools pools registered, and nothing whatsoever is measured in between.
package dbpool
