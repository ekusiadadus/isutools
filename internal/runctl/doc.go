// Package runctl owns the measurement run lifecycle: a single process-wide
// Controller decides when a run starts, when its boundaries are frozen, when
// its immutable snapshot may be published, and when it is aborted.
//
// The package exists because measurement boundaries are the one thing an
// ISUCON toolkit cannot get "mostly right". A run whose opening moments are
// missing, whose closing moments leak into the next run, or whose data is
// published by a worker belonging to an already-abandoned run is worse than no
// measurement at all: it looks authoritative while being wrong. Everything
// here is therefore built around two invariants.
//
//  1. Epoch fencing. Every run owns an [Epoch]. The Controller's current epoch
//     advances on every successful StartRun and on every AbortRun. Background
//     workers never touch the Controller state directly; they go through
//     [Controller.commit] and [Controller.publish], which reject any epoch that
//     is no longer current with [ErrStaleEpoch]. A worker belonging to an
//     aborted run is therefore structurally incapable of publishing data or of
//     dragging the run back to "finished", even when the abort could not join
//     it in time.
//
//  2. Fail-open measurement. Collector failures never surface as Go errors from
//     StartRun/FinishRun; they downgrade the run's [Validity] instead. An error
//     return means the Controller itself refused or could not perform the
//     operation. Callers must always inspect Validity, never only err.
//
// Boundaries are treated as intervals, not instants: generation collectors are
// switched sequentially and baseline collectors are sampled in parallel, and
// the measured width of both is recorded in a [BoundaryWindow] so that a run
// whose boundary was too smeared can be marked partial or invalid rather than
// silently trusted.
//
// The Controller never blocks on collectors or workers while holding its
// mutex, because ResetNow is expected to be callable from inside an
// instrumented HTTP handler.
package runctl
