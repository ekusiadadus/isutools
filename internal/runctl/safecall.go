package runctl

import (
	"errors"
	"fmt"
	"strings"
)

// Measurement is fail-open: a bug in a collector may degrade the measurement
// and must be recorded, but it may never take the measured application down.
// A collector call therefore never runs bare — StartRun and FinishRun are
// reachable from an instrumented HTTP handler (ResetNow), so an unrecovered
// panic in BeginBoundary would surface as a 500 in the application under test
// rather than as a failed section in the snapshot.
//
// Every collector entry point is wrapped by one of the helpers below, which
// convert a panic into the plan's contract-violation result: no value, no
// commit, a stable Code and a short rendering of the panic value.

// panicTextMax bounds the recorded rendering of a panic value. The value is
// copied into snapshots and health messages, both of which are shown to
// operators, so it is kept to one short line and the stack is never included.
const panicTextMax = 160

// errCollectorPanic is the marker every recovered collector panic wraps.
// Classification switches on it rather than on the message text.
var errCollectorPanic = errors.New("runctl: collector panicked")

// panicError is a recovered panic rendered as an error.
type panicError struct{ text string }

func (e *panicError) Error() string { return e.text }
func (e *panicError) Unwrap() error { return errCollectorPanic }

// isPanic reports whether err came from a recovered collector panic. A panic
// is a collector bug rather than a runtime condition, so it is coded as a
// contract violation instead of as a timeout or an ordinary failure.
func isPanic(err error) bool { return errors.Is(err, errCollectorPanic) }

// newPanicError builds the error a recovered panic is reported as.
func newPanicError(collector, op string, v any) error {
	return &panicError{text: fmt.Sprintf("collector %s panicked in %s: %s", collector, op, shortPanic(v))}
}

// shortPanic renders a recovered value as a single short line. Rendering runs
// under its own recover because the panic value may be a type whose String or
// Error method panics in turn, and a second panic inside the barrier would
// defeat the entire point of having one.
func shortPanic(v any) (text string) {
	defer func() {
		if recover() != nil {
			text = "unprintable panic value"
		}
	}()
	text = strings.Join(strings.Fields(fmt.Sprint(v)), " ")
	if text == "" {
		return "unprintable panic value"
	}
	if r := []rune(text); len(r) > panicTextMax {
		return string(r[:panicTextMax]) + "..."
	}
	return text
}

// safeResult runs a collector call that produces a value and converts a panic
// into an error. The value is discarded on panic: a collector that died
// mid-call has proved nothing about whether its boundary committed, and the
// zero BoundaryResult/SampleResult reports Committed=false, which is the
// conservative reading.
func safeResult[T any](collector, op string, fn func() (T, error)) (out T, err error) {
	defer func() {
		if r := recover(); r != nil {
			var zero T
			out, err = zero, newPanicError(collector, op, r)
		}
	}()
	return fn()
}

// safeErr runs a collector call that only reports an error.
func safeErr(collector, op string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = newPanicError(collector, op, r)
		}
	}()
	return fn()
}

// safeHook is safeErr for a caller-supplied hook rather than a collector. The
// enrich hook is application code invoked from the Controller's own background
// goroutine, so it needs the same barrier and reads better with its own
// wording.
func safeHook(name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{text: fmt.Sprintf("the %s hook panicked: %s", name, shortPanic(r))}
		}
	}()
	return fn()
}

// safeInvoke runs a collector call on a cleanup path, where there is no
// boundary record left to attach a failure to and nothing useful left to do
// about one: draining and releasing on the way out. A panic is reported
// through health and swallowed, so that one collector's bug cannot strand the
// handles of every collector after it.
func (c *Controller) safeInvoke(collector, op string, fn func() error) {
	if err := safeErr(collector, op, fn); isPanic(err) {
		c.recordHealth(HealthContractViolation, err.Error())
	}
}
