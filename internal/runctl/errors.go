package runctl

import "errors"

// Sentinel errors. Transport layers map these one-to-one onto HTTP status
// codes, so their identity is part of the package contract: wrap them with
// %w rather than replacing them, and never invent a new sentinel for a
// condition that is already covered here.
var (
	// ErrRunActive is returned when another run occupies the Controller and
	// the caller did not ask to preempt it. Callers that must win (an
	// initialize handler, for example) retry with StartRunOptions.Preempt.
	// Maps to HTTP 409.
	ErrRunActive = errors.New("runctl: another run is active")

	// ErrRunTransitioning is returned while a run is mid-transition
	// (starting or aborting) and the requested operation cannot be ordered
	// against it. Retrying after the transition settles is safe.
	// Maps to HTTP 409.
	ErrRunTransitioning = errors.New("runctl: run is transitioning")

	// ErrRunAborted is returned for operations on a run that was abandoned.
	// The run's data is gone for good, so this is deliberately distinct from
	// ErrUnknownRun: the caller learns the run existed and failed rather than
	// that it never existed. Maps to HTTP 410.
	ErrRunAborted = errors.New("runctl: run was aborted")

	// ErrUnknownRun is returned for a run the Controller does not retain:
	// never started, evicted by RetainedRuns, or expired. Maps to HTTP 404.
	ErrUnknownRun = errors.New("runctl: unknown run")

	// ErrStaleEpoch rejects a state change or a snapshot publication coming
	// from a worker whose run is no longer current. It is an internal fence
	// signal and is not expected to reach an API caller.
	ErrStaleEpoch = errors.New("runctl: stale epoch")

	// ErrBudgetInversion reports a configuration in which a child budget is
	// not strictly smaller than its parent. Such a configuration cannot
	// honour the hierarchy, so it is rejected at construction/registration
	// time instead of producing unexplainable timeouts at runtime.
	ErrBudgetInversion = errors.New("runctl: child budget >= parent budget")

	// ErrInitializeBusy reports that SerializeInitialize could not acquire the
	// process-wide initialize guard within InitializeGuardBudget.
	ErrInitializeBusy = errors.New("isutools: initialize guard busy")

	// ErrCollectorRegistered rejects a duplicate collector name. Names index
	// snapshot sections, so collisions would silently merge two collectors.
	ErrCollectorRegistered = errors.New("runctl: collector already registered")

	// ErrInvalidRegistration reports a registration with no name or no
	// collector.
	ErrInvalidRegistration = errors.New("runctl: invalid registration")
)
