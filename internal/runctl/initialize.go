package runctl

import (
	"context"
	"fmt"
	"time"
)

// initializeGuardKey marks a context as running inside the initialize guard.
// The type is unexported so nothing outside this package can forge the marker;
// callers ask HasInitializeGuard instead.
type initializeGuardKey struct{}

// initializeGuard is a process-wide, one-slot semaphore. A channel rather than
// a sync.Mutex because acquisition has to be abandonable: waiting forever for
// a stuck initialize would be worse than reporting that it is busy.
var initializeGuard = make(chan struct{}, 1)

// SerializeInitialize runs fn as the only initialize in this process.
//
// Starting a run only serializes the instant the boundary is taken. If one
// initialize takes its boundary and a second then rebuilds the database, the
// first run is polluted by the second one's load; preemption makes that
// visible by invalidating the run, but it cannot prevent it. The only real fix
// is to serialize the whole initialize handler, which is what this guard is
// for.
//
// The context passed to fn carries the guard marker, so a run started inside
// fn can be told apart from one started outside the guard.
func SerializeInitialize(ctx context.Context, fn func(context.Context) error) error {
	return SerializeInitializeWithBudget(ctx, InitializeGuardBudget, fn)
}

// SerializeInitializeWithBudget is SerializeInitialize with an explicit
// acquisition timeout. It exists so tests can exercise the busy path without
// waiting out InitializeGuardBudget.
func SerializeInitializeWithBudget(ctx context.Context, budget time.Duration, fn func(context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("%w: nil initialize function", ErrInvalidRegistration)
	}
	if budget <= 0 {
		budget = InitializeGuardBudget
	}

	timer := time.NewTimer(budget)
	defer timer.Stop()

	select {
	case initializeGuard <- struct{}{}:
	case <-timer.C:
		return ErrInitializeBusy
	case <-ctx.Done():
		return fmt.Errorf("runctl: waiting for initialize guard: %w", ctx.Err())
	}
	defer func() { <-initializeGuard }()

	return fn(context.WithValue(ctx, initializeGuardKey{}, struct{}{}))
}

// HasInitializeGuard reports whether ctx came from inside SerializeInitialize.
// The public wrapper uses it to flag an initialize-triggered run that bypassed
// the guard: such a run can still succeed, but it may have been polluted by a
// concurrent rebuild, and silently trusting it is exactly the failure mode the
// guard exists to expose.
//
// The check is context-based on purpose. Goroutine-local state would not
// survive the handler spawning its own goroutines, and would let the marker
// leak across unrelated requests.
func HasInitializeGuard(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	return ctx.Value(initializeGuardKey{}) != nil
}
