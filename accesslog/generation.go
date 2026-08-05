package accesslog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// SectionName is the snapshot section this collector fills. It is the key the
// existing snapshot and health output already use for the access log.
const SectionName = "accesslog"

// Handle errors. They are sentinels because the run controller maps a Collect
// failure onto a stable machine-readable code and must be able to tell "this
// handle is not mine" from "this generation has no data yet".
var (
	// ErrForeignHandle rejects a handle minted by another collector or by
	// another instance of this one.
	ErrForeignHandle = errors.New("accesslog: generation handle belongs to another collector")
	// ErrHandleReleased reports a handle whose data was already freed.
	ErrHandleReleased = errors.New("accesslog: generation handle was released")
	// ErrNotDrained reports a generation that was never drained, so no fixed
	// value exists to collect.
	ErrNotDrained = errors.New("accesslog: generation was not drained")
	// ErrFreezeWithoutBoundary rejects a closing boundary that would mint this
	// collector's first generation, which happens when the adapter was
	// registered after the run had already opened. See the ordering
	// requirement on GenerationCollector.
	ErrFreezeWithoutBoundary = errors.New("accesslog: freeze without a preceding boundary for this epoch; the section is skipped")
	// errNoCollector reports a collector built around no log collector at all.
	errNoCollector = errors.New("accesslog: no collector to take a boundary on")
)

// SkippedLateRegistration is the health reason recorded when a run is refused
// a section because this collector joined after the run had opened. It names
// the operator-fixable cause, which "no accesslog section" on its own does not.
const SkippedLateRegistration = "access log generation registered after the run opened; the section is skipped rather than reported with an interval that starts at process start"

// freezePoint is where a generation ends: the byte offset in the log file at
// the moment of the boundary, plus enough identity to notice that the file it
// was measured in has been replaced.
type freezePoint struct {
	offset        int64
	rotations     int64
	copyTruncates int64
	// broken marks a point that cannot be read up to, so the generation ends
	// with whatever had already been consumed.
	broken bool
}

// boundaryKey identifies one boundary operation of one run. Every operation is
// idempotent per (runID, epoch), so the phase has to be part of the key: a run
// takes both an opening and a closing boundary under the same epoch.
type boundaryKey struct {
	runID string
	epoch runctl.Epoch
	phase runctl.Phase
}

// boundaryRecord is a replayable boundary outcome, so a retried call returns
// what the first one did rather than a second, differently-timed verdict.
type boundaryRecord struct {
	result runctl.BoundaryResult
	err    error
}

// GenerationCollector adapts a log Collector to runctl.GenerationCollector.
//
// An access log is pulled, not pushed, so a generation cannot be closed by a
// pointer swap: the lines it owns are still sitting in the file. The boundary
// therefore fixes the *freeze point* — the offset the log had ended at — and
// Drain reads up to exactly that offset. That split is what keeps a Finish
// from swallowing requests that were logged after it, and it keeps the
// boundary free of file I/O beyond a single stat.
//
// The consequence of the pull model is that this adapter has to be the only
// reader: a bare Collector.Collect between a boundary and its drain reads to
// end of file, which pulls the next generation's lines into the aggregate the
// closing generation is about to be cut from. Collector.Snapshot flushes too,
// so it carries the same restriction. A reader that only wants to display the
// aggregate — the live report, which refreshes whenever somebody has the page
// open — uses Collector.Peek, which declines to read while this adapter has a
// freeze point outstanding.
//
// # Ordering requirement
//
// Every run must reach BeginBoundary on this collector before it reaches
// Freeze, and therefore the adapter must be registered before the first run
// opens. A generation's lower edge is its predecessor's freeze point, so the
// very first generation of a collector begins at the collector's start of
// life. If the first generation were minted by a Freeze — the shape of an
// adapter registered in the middle of a run — the run's section would contain
// every line logged since the process started and would be reported as that
// run's interval. Such a Freeze is refused with ErrFreezeWithoutBoundary and
// the section is skipped: a missing interval is recoverable, a silently
// inflated one is not. The refusal mints nothing, so the next run that takes
// both boundaries measures correctly.
type GenerationCollector struct {
	collector *Collector
	// sem serializes drains. It is a channel rather than a sync.Mutex because
	// a mutex cannot be given up when the drain budget expires.
	sem chan struct{}

	mu       sync.Mutex
	epoch    runctl.Epoch
	gen      uint64
	prevDone chan struct{}
	results  map[boundaryKey]boundaryRecord
}

var _ runctl.GenerationCollector = (*GenerationCollector)(nil)

// NewGenerationCollector wraps a log collector. A nil collector yields an
// adapter whose boundaries fail instead of one that panics on first use: an
// unconfigured access log must not be able to break a run.
func NewGenerationCollector(collector *Collector) *GenerationCollector {
	sem := make(chan struct{}, 1)
	sem <- struct{}{}
	return &GenerationCollector{
		collector: collector,
		sem:       sem,
		results:   map[boundaryKey]boundaryRecord{},
	}
}

// Name identifies the snapshot section this collector fills.
func (c *GenerationCollector) Name() string { return SectionName }

// BeginBoundary fixes the freeze point of the generation it closes and starts
// the next one at that same offset.
func (c *GenerationCollector) BeginBoundary(ctx context.Context, runID string, ep runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, ep, runctl.PhaseStartBoundary)
}

// Freeze fixes the run's closing offset. Lines appended after it belong to the
// next generation, outside the run, even though they are read later.
//
// It fails with ErrFreezeWithoutBoundary when it would mint this collector's
// first generation, because that generation would start at the collector's
// start of life rather than at the run's opening boundary. See the ordering
// requirement on GenerationCollector.
func (c *GenerationCollector) Freeze(ctx context.Context, runID string, ep runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, ep, runctl.PhaseFinishFreeze)
}

// boundary is the shared implementation of both boundaries: they differ only
// in which generation the returned handle happens to name. It ignores ctx on
// purpose: fixing an offset is a single stat that cannot be usefully abandoned
// half way, and giving up would leave the generation without an end.
func (c *GenerationCollector) boundary(_ context.Context, runID string, ep runctl.Epoch, phase runctl.Phase) (runctl.BoundaryResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.collector == nil {
		return runctl.BoundaryResult{At: time.Now()}, errNoCollector
	}
	if ep < c.epoch {
		return runctl.BoundaryResult{At: time.Now()}, fmt.Errorf("%w: %s is at epoch %d, got %d",
			runctl.ErrStaleEpoch, SectionName, c.epoch, ep)
	}
	key := boundaryKey{runID: runID, epoch: ep, phase: phase}
	if prev, ok := c.results[key]; ok {
		return prev.result, prev.err
	}
	if ep > c.epoch {
		// Only the current epoch is replayable; anything older is answered
		// with ErrStaleEpoch, so keeping its records would just grow the map
		// for the lifetime of the process.
		c.epoch = ep
		c.results = make(map[boundaryKey]boundaryRecord, 2)
	}
	// c.gen == 0 means no boundary of any kind has been taken yet, so this run
	// certainly did not open on this collector: the generation this Freeze
	// would mint starts at the collector's start of life, not at the run's
	// opening boundary. Refusing skips the section instead of reporting an
	// interval that is silently too wide, and mints nothing, so the next run
	// that takes both boundaries is measured correctly.
	if phase == runctl.PhaseFinishFreeze && c.gen == 0 {
		c.collector.recordSkipped(SkippedLateRegistration)
		record := boundaryRecord{
			result: runctl.BoundaryResult{At: time.Now()},
			err: fmt.Errorf("%w: run %s at epoch %d found %s with no opening boundary",
				ErrFreezeWithoutBoundary, runID, ep, SectionName),
		}
		c.results[key] = record
		return record.result, record.err
	}

	g := &logGeneration{
		owner:    c,
		gen:      c.gen,
		point:    c.collector.freezePoint(),
		prevDone: c.prevDone,
		done:     make(chan struct{}),
	}
	c.gen++
	c.prevDone = g.done

	record := boundaryRecord{result: runctl.BoundaryResult{
		Handle:    runctl.NewGenerationHandle(runID, ep, SectionName, g.gen, g),
		At:        time.Now(),
		Committed: true,
	}}
	c.results[key] = record
	return record.result, record.err
}

// Drain reads the log up to the handle's freeze point and not one byte
// further, then cuts the aggregate so the next generation starts empty.
//
// A cancelled context stops the catch-up between bounded reads; whatever was
// read stays in the generation and a later Drain resumes from there.
func (c *GenerationCollector) Drain(ctx context.Context, h runctl.GenerationHandle) error {
	g, err := c.generationFor(h)
	if err != nil {
		return err
	}
	if sealed(g.done) {
		return nil
	}
	// Generations are cut in order: this one may only read once its
	// predecessor has stopped, or the two would compete for the same bytes of
	// the same file and mis-attribute them.
	//
	// Each wait tries the ready case first. A select over an available channel
	// and an expired context picks between them at random, which would turn a
	// drain that had nothing to wait for into a spurious timeout.
	if g.prevDone != nil && !sealed(g.prevDone) {
		select {
		case <-g.prevDone:
		case <-g.done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("accesslog: generation %d waiting for its predecessor: %w", g.gen, ctx.Err())
		}
	}
	select {
	case <-c.sem:
	default:
		select {
		case <-c.sem:
		case <-g.done:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("accesslog: generation %d waiting for the log collector: %w", g.gen, ctx.Err())
		}
	}
	defer func() { c.sem <- struct{}{} }()
	if sealed(g.done) {
		return nil
	}

	if err := g.catchUp(ctx); err != nil {
		// The lines that were read still belong to this generation, so its
		// value is fixed now and reported as partial rather than lost.
		g.retain(c.collector.generationSnapshot())
		return err
	}
	g.seal(true)
	return nil
}

// Collect returns the frozen access-log aggregate. It reads only what Drain
// fixed and never triggers a read of its own.
func (c *GenerationCollector) Collect(h runctl.GenerationHandle) (any, error) {
	g, err := c.generationFor(h)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return nil, fmt.Errorf("%w: generation %d", ErrHandleReleased, g.gen)
	}
	if !g.haveSnap {
		return nil, fmt.Errorf("%w: generation %d", ErrNotDrained, g.gen)
	}
	return g.snap, nil
}

// Release drops the frozen aggregate. It is idempotent, and a handle this
// collector never minted is ignored rather than reported: Release has no error
// channel and must not panic into the caller.
func (c *GenerationCollector) Release(h runctl.GenerationHandle) {
	g, err := c.generationFor(h)
	if err != nil {
		return
	}
	g.mu.Lock()
	if g.released {
		g.mu.Unlock()
		return
	}
	g.released = true
	g.snap, g.haveSnap = Snapshot{}, false
	g.mu.Unlock()
	// A released generation must stop blocking its successor, and the lines it
	// still owns have to be cut off the aggregate so the next generation does
	// not inherit them.
	g.seal(false)
}

// generationFor resolves a handle to the generation it names, rejecting
// handles from other collectors instead of type-asserting blindly.
func (c *GenerationCollector) generationFor(h runctl.GenerationHandle) (*logGeneration, error) {
	g, ok := h.Token().(*logGeneration)
	if !ok || g == nil || g.owner != c {
		return nil, fmt.Errorf("%w: collector %q generation %d", ErrForeignHandle, h.Collector, h.Gen)
	}
	return g, nil
}

// logGeneration is the closed byte range a handle points at.
type logGeneration struct {
	owner *GenerationCollector
	gen   uint64
	point freezePoint
	// prevDone is the predecessor's done channel, or nil for the first
	// generation. It is what keeps the catch-up reads ordered.
	prevDone chan struct{}
	// done is closed once this generation has been cut off the aggregate.
	done chan struct{}
	once sync.Once

	mu       sync.Mutex
	snap     Snapshot
	haveSnap bool
	released bool
}

// drainChunkBytes bounds how much of the catch-up runs under one hold of the
// log collector's lock. The read is chunked so a boundary taken while a large
// backlog is being drained still gets the lock inside its own budget.
const drainChunkBytes = 1 << 20

// catchUp reads the log up to this generation's freeze point.
func (g *logGeneration) catchUp(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("accesslog: generation %d catch-up interrupted: %w", g.gen, err)
		}
		done, err := g.catchUpChunk(ctx)
		if err != nil || done {
			return err
		}
	}
}

// catchUpChunk reads at most drainChunkBytes and reports whether the freeze
// point has been reached.
func (g *logGeneration) catchUpChunk(ctx context.Context) (bool, error) {
	c := g.owner.collector
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.file == nil || g.point.broken {
		return true, nil
	}
	if c.health.Rotations != g.point.rotations || c.health.CopyTruncates != g.point.copyTruncates {
		// The file the freeze point was measured in has been replaced, so the
		// offset no longer names a position in it. Whatever was already read
		// stays in the generation; guessing at the rest would mis-attribute
		// another generation's lines to this one.
		c.recordPartialLocked("access log rotated between the boundary and the drain; the generation is incomplete")
		return true, nil
	}
	remaining := g.point.offset - c.offset
	if remaining <= 0 {
		return true, nil
	}
	if remaining > drainChunkBytes {
		remaining = drainChunkBytes
	}
	err := c.readToEOFLocked(ctx, &remaining)
	switch {
	case err == nil:
		// End of file before the freeze point: the log shrank under us, which
		// readToEOFLocked cannot distinguish from an ordinary EOF.
		return true, nil
	case errors.Is(err, ErrCollectLimit):
		// The chunk budget ran out. That is the intended stop, not a failure;
		// whether the generation is complete depends on the offset alone.
		return c.offset >= g.point.offset, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false, fmt.Errorf("accesslog: generation %d catch-up interrupted: %w", g.gen, err)
	default:
		c.recordErrorLocked(fmt.Errorf("drain %s: %w", c.path, err))
		return false, fmt.Errorf("accesslog: generation %d catch-up failed: %w", g.gen, err)
	}
}

// seal cuts this generation off the aggregate and starts the next one. keep
// decides whether the frozen value is retained for Collect or dropped.
func (g *logGeneration) seal(keep bool) {
	g.once.Do(func() {
		snap := g.owner.collector.cutGeneration()
		if keep {
			g.retain(snap)
		}
		close(g.done)
	})
}

// retain fixes the generation's value. A released generation keeps nothing.
func (g *logGeneration) retain(snap Snapshot) {
	g.mu.Lock()
	if !g.released {
		g.snap, g.haveSnap = snap, true
	}
	g.mu.Unlock()
}

// sealed reports whether a generation has already been cut.
func sealed(done chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// freezePoint fixes the offset a generation ends at. Everything already in the
// file belongs to the generation being closed; anything appended after this
// call belongs to the next one, which is what stops a Finish from swallowing
// requests logged after it.
func (c *Collector) freezePoint() freezePoint {
	c.mu.Lock()
	defer c.mu.Unlock()

	// From here until the matching seal, the collector owes a drain an exact
	// offset to stop at, so no flush may advance past it. BoundaryPending is
	// how a reader that flushes on its own schedule finds that out.
	c.pendingBoundaries++
	point := freezePoint{
		offset:        c.offset,
		rotations:     c.health.Rotations,
		copyTruncates: c.health.CopyTruncates,
	}
	if c.closed {
		point.broken = true
		return point
	}
	if c.file == nil {
		// The log was never opened, or was closed by a failed collect. Give
		// the next generation a baseline at the current end of the file; the
		// generation being closed simply has nothing left to read.
		if err := c.openLocked(true); err != nil {
			c.recordErrorLocked(err)
		}
		point.offset, point.broken = c.offset, true
		return point
	}
	info, err := c.file.Stat()
	if err != nil {
		c.recordErrorLocked(fmt.Errorf("stat open %s: %w", c.path, err))
		point.broken = true
		return point
	}
	if size := info.Size(); size > point.offset {
		point.offset = size
	}
	return point
}

// recordSkipped degrades the collector's health with the reason a run has no
// access-log section. The reason belongs on the un-cut generation because that
// is exactly the window whose extent could not be established; the next
// boundary clears it along with the aggregate.
func (c *Collector) recordSkipped(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordPartialLocked(reason)
}

// generationSnapshot copies the aggregate without ending the generation.
func (c *Collector) generationSnapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

// cutGeneration freezes the aggregate and starts an empty one without moving
// the read position, so the next generation begins exactly at the freeze
// point: no line is counted twice and none is skipped. Collector.Reset cannot
// be used for this — it re-baselines at the current end of file and would drop
// everything written since the boundary.
func (c *Collector) cutGeneration() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.snapshotLocked()
	c.agg.Reset()
	c.health = Health{Status: StatusOK}
	// The freeze point this generation was given has been honoured, so the
	// offset it named no longer constrains anybody. seal's sync.Once makes the
	// pairing with freezePoint exactly one-to-one.
	if c.pendingBoundaries > 0 {
		c.pendingBoundaries--
	}
	return snap
}

func (c *Collector) snapshotLocked() Snapshot {
	snap := c.agg.Snapshot()
	c.health.Offset = c.offset
	snap.Health = c.health
	return snap
}
