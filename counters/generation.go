package counters

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

// SectionName is the snapshot section this collector fills. It is the key the
// existing snapshot and health output already use for counters.
const SectionName = "counters"

// Handle errors. They are sentinels because the run controller maps a Collect
// failure onto a stable machine-readable code and must be able to tell "this
// handle is not mine" from "this generation has no data yet".
var (
	// ErrForeignHandle rejects a handle minted by another collector or by
	// another instance of this one.
	ErrForeignHandle = errors.New("counters: generation handle belongs to another collector")
	// ErrHandleReleased reports a handle whose data was already freed.
	ErrHandleReleased = errors.New("counters: generation handle was released")
	// ErrNotDrained reports a generation that has not been materialised yet,
	// so no fixed value exists to collect.
	ErrNotDrained = errors.New("counters: generation was not drained")
)

// Frozen is one closed counter generation.
type Frozen struct {
	// Entries are sorted by count descending, ties by name.
	Entries []Entry `json:"entries"`
	// Dropped counts observations merged into OverflowName.
	Dropped uint64 `json:"dropped"`
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

// GenerationCollector adapts a counter registry to
// runctl.GenerationCollector.
//
// A boundary swaps the counter table out and nothing more, so it cannot block:
// counters have no in-flight work to settle, only a map to hand over.
// Materialising and sorting that map is deferred to Drain, which keeps the
// boundary itself a pointer swap and gives the context somewhere to apply.
type GenerationCollector struct {
	registry *Registry

	mu      sync.Mutex
	epoch   runctl.Epoch
	gen     uint64
	results map[boundaryKey]boundaryRecord
}

var _ runctl.GenerationCollector = (*GenerationCollector)(nil)

// NewGenerationCollector wraps a registry. A nil registry means the package
// default, which is where the facade helpers count.
func NewGenerationCollector(registry *Registry) *GenerationCollector {
	if registry == nil {
		registry = Default
	}
	return &GenerationCollector{registry: registry, results: map[boundaryKey]boundaryRecord{}}
}

// Name identifies the snapshot section this collector fills.
func (c *GenerationCollector) Name() string { return SectionName }

// BeginBoundary swaps in an empty counter table and returns a handle to the
// table it just replaced.
func (c *GenerationCollector) BeginBoundary(ctx context.Context, runID string, ep runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, ep, runctl.PhaseStartBoundary)
}

// Freeze seals the running counter table. Counts recorded after it belong to
// the next generation, outside the run.
func (c *GenerationCollector) Freeze(ctx context.Context, runID string, ep runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, ep, runctl.PhaseFinishFreeze)
}

// boundary is the shared implementation of both boundaries: they differ only
// in which generation the returned handle happens to name. It ignores ctx on
// purpose: the swap is a single map assignment that cannot block, so
// abandoning it on an expired deadline would lose counts for no benefit.
func (c *GenerationCollector) boundary(_ context.Context, runID string, ep runctl.Epoch, phase runctl.Phase) (runctl.BoundaryResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

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

	counts, dropped := c.registry.rotate()
	g := &countersGeneration{owner: c, gen: c.gen, done: make(chan struct{}), counts: counts, dropped: dropped}
	c.gen++

	record := boundaryRecord{result: runctl.BoundaryResult{
		Handle:    runctl.NewGenerationHandle(runID, ep, SectionName, g.gen, g),
		At:        time.Now(),
		Committed: true,
	}}
	c.results[key] = record
	return record.result, record.err
}

// Drain materialises the swapped-out table.
//
// The boundary already detached it, so there is no in-flight work to wait for
// and a settled generation is reported as complete even when ctx is already
// done: failing it would drop a section that is not actually missing anything.
func (c *GenerationCollector) Drain(ctx context.Context, h runctl.GenerationHandle) error {
	g, err := c.generationFor(h)
	if err != nil {
		return err
	}
	select {
	case <-g.done:
		return nil
	default:
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("counters: generation %d was not materialised: %w", g.gen, err)
	}
	g.settle()
	return nil
}

// Collect returns the frozen counter table. It reads only what Drain fixed and
// never touches the registry's current table.
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
	if !g.settled {
		return nil, fmt.Errorf("%w: generation %d", ErrNotDrained, g.gen)
	}
	return Frozen{
		Entries: append([]Entry(nil), g.frozen.Entries...),
		Dropped: g.frozen.Dropped,
	}, nil
}

// Release drops the frozen table. It is idempotent, and a handle this
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
	g.counts, g.frozen = nil, Frozen{}
	g.mu.Unlock()
	// Sealing here stops a later Drain from materialising data that has
	// already been handed back.
	g.seal.Do(func() { close(g.done) })
}

// generationFor resolves a handle to the generation it names, rejecting
// handles from other collectors instead of type-asserting blindly.
func (c *GenerationCollector) generationFor(h runctl.GenerationHandle) (*countersGeneration, error) {
	g, ok := h.Token().(*countersGeneration)
	if !ok || g == nil || g.owner != c {
		return nil, fmt.Errorf("%w: collector %q generation %d", ErrForeignHandle, h.Collector, h.Gen)
	}
	return g, nil
}

// countersGeneration is the closed counter table a handle points at.
type countersGeneration struct {
	owner *GenerationCollector
	gen   uint64
	// done is closed once this generation is materialised or released, so a
	// repeated Drain is a cheap no-op instead of a second sort.
	done chan struct{}
	seal sync.Once

	mu       sync.Mutex
	counts   map[string]int64
	dropped  uint64
	frozen   Frozen
	settled  bool
	released bool
}

// settle turns the detached map into the sorted, immutable frozen value.
func (g *countersGeneration) settle() {
	g.mu.Lock()
	if !g.settled && !g.released {
		g.frozen = Frozen{Entries: sortedEntries(g.counts), Dropped: g.dropped}
		g.counts = nil
		g.settled = true
	}
	g.mu.Unlock()
	g.seal.Do(func() { close(g.done) })
}

// rotate atomically swaps in an empty counter table and returns the one it
// replaced. Snapshot followed by Reset would lose every observation made
// between the two calls, which is exactly what a generation boundary must not
// do.
func (r *Registry) rotate() (counts map[string]int64, dropped uint64) {
	r.mu.Lock()
	counts, dropped = r.counts, r.dropped
	r.counts, r.dropped = map[string]int64{}, 0
	r.mu.Unlock()
	return counts, dropped
}

// sortedEntries renders a detached table in the reporting order: count
// descending, ties by name so a snapshot is byte-stable.
func sortedEntries(counts map[string]int64) []Entry {
	entries := make([]Entry, 0, len(counts))
	for name, count := range counts {
		entries = append(entries, Entry{Name: name, Count: count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count != entries[j].Count {
			return entries[i].Count > entries[j].Count
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}
