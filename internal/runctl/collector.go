package runctl

import "context"

// GenerationCollector is a collector that accumulates into a swappable
// generation: HTTP stats, SQL stats, access log offsets, counters. Boundaries
// are pointer swaps, so they are expected to be non-blocking and to finish
// inside PerCollectorGenerationBudget.
//
// Every method takes or returns a handle rather than exposing the collector's
// current state, so that a run's data is fixed the instant its boundary is
// taken and cannot drift while the snapshot is being built.
type GenerationCollector interface {
	// Name identifies the snapshot section this collector fills.
	Name() string

	// BeginBoundary swaps in a fresh generation and returns a handle to the
	// generation it just closed. Fast and non-blocking.
	BeginBoundary(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error)

	// Freeze seals the current generation and returns its handle. Observations
	// made after Freeze belong to the next generation, outside the run.
	Freeze(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error)

	// Drain settles in-flight work pinned to the handle's generation only. It
	// must return within DrainCancelGrace of ctx being done and must leave no
	// goroutine that will later modify that generation.
	//
	// Implementations must wait on a per-generation done channel. sync.Cond
	// cannot be interrupted by a context, so a cond-based wait makes this
	// contract impossible to honour when a request never returns.
	Drain(ctx context.Context, h GenerationHandle) error

	// Collect reads the drained generation's fixed data. It must not read the
	// collector's mutable current state.
	Collect(h GenerationHandle) (any, error)

	// Release frees whatever the handle pins. Idempotent: a second Release,
	// or a Release racing the owner's own cleanup, is a no-op.
	Release(h GenerationHandle)
}

// BaselineCollector is a collector that measures a delta between two samples:
// process stats, table row counts, DB pool stats, network counters, host
// stats. Sampling does bounded I/O, so it gets a much larger per-collector
// budget than a generation swap and is executed in parallel to keep the
// boundary window narrow.
type BaselineCollector interface {
	// Name identifies the snapshot section this collector fills.
	Name() string

	// CaptureBaseline samples the opening boundary and returns an immutable
	// handle carrying the sampled values.
	CaptureBaseline(ctx context.Context, runID string, ep Epoch) (SampleResult, error)

	// CaptureFinal samples the closing boundary.
	CaptureFinal(ctx context.Context, runID string, ep Epoch) (SampleResult, error)

	// Collect derives the interval value from two frozen samples. The only
	// legal inputs are base.Sample() and final.Sample(); it must not touch the
	// collector's own fields, the database, or /proc. A type mismatch must be
	// returned as an error, never a panic — measurement may not break the
	// measured application.
	Collect(base, final BaselineHandle) (any, error)

	// Release frees whatever the handle pins. Idempotent.
	Release(h BaselineHandle)
}

// registeredGeneration pairs a generation collector with its registration.
type registeredGeneration struct {
	reg  Registration
	coll GenerationCollector
}

// registeredBaseline pairs a baseline collector with its registration.
type registeredBaseline struct {
	reg  Registration
	coll BaselineCollector
}
