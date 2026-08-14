package redisstats

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/runctl"
)

const SectionName = "redis"

var (
	ErrForeignHandle  = errors.New("redisstats: foreign generation handle")
	ErrNotDrained     = errors.New("redisstats: generation was not drained")
	ErrHandleReleased = errors.New("redisstats: generation handle was released")
)

type Frozen struct {
	Entries []Entry `json:"entries"`
	Dropped uint64  `json:"dropped"`
}

type generation struct {
	owner    *GenerationCollector
	table    *agg.Table
	dropped  uint64
	frozen   Frozen
	settled  bool
	released bool
	mu       sync.Mutex
}

type boundaryKey struct {
	runID string
	epoch runctl.Epoch
	phase runctl.Phase
}

type boundaryRecord struct {
	result runctl.BoundaryResult
	err    error
}

type GenerationCollector struct {
	registry *Registry
	mu       sync.Mutex
	epoch    runctl.Epoch
	gen      uint64
	results  map[boundaryKey]boundaryRecord
}

var _ runctl.GenerationCollector = (*GenerationCollector)(nil)

func NewGenerationCollector(registry *Registry) *GenerationCollector {
	if registry == nil {
		registry = Default
	}
	return &GenerationCollector{registry: registry, results: map[boundaryKey]boundaryRecord{}}
}

func (c *GenerationCollector) Name() string { return SectionName }

func (c *GenerationCollector) BeginBoundary(ctx context.Context, runID string, epoch runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, epoch, runctl.PhaseStartBoundary)
}

func (c *GenerationCollector) Freeze(ctx context.Context, runID string, epoch runctl.Epoch) (runctl.BoundaryResult, error) {
	return c.boundary(ctx, runID, epoch, runctl.PhaseFinishFreeze)
}

func (c *GenerationCollector) boundary(_ context.Context, runID string, epoch runctl.Epoch, phase runctl.Phase) (runctl.BoundaryResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch < c.epoch {
		return runctl.BoundaryResult{At: time.Now()}, runctl.ErrStaleEpoch
	}
	key := boundaryKey{runID: runID, epoch: epoch, phase: phase}
	if prior, ok := c.results[key]; ok {
		return prior.result, prior.err
	}
	if epoch > c.epoch {
		c.epoch = epoch
		c.results = map[boundaryKey]boundaryRecord{}
	}
	table, dropped := c.registry.rotate()
	g := &generation{owner: c, table: table, dropped: dropped}
	result := runctl.BoundaryResult{Handle: runctl.NewGenerationHandle(runID, epoch, SectionName, c.gen, g), At: time.Now(), Committed: true}
	c.gen++
	c.results[key] = boundaryRecord{result: result}
	return result, nil
}

func (c *GenerationCollector) Drain(ctx context.Context, handle runctl.GenerationHandle) error {
	g, err := c.resolve(handle)
	if err != nil {
		return err
	}
	g.mu.Lock()
	if g.settled || g.released {
		g.mu.Unlock()
		return nil
	}
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return err
	}
	if !g.settled && !g.released {
		g.frozen = Frozen{Entries: entriesFromAgg(g.table.Snapshot()), Dropped: g.dropped}
		g.table = nil
		g.settled = true
	}
	g.mu.Unlock()
	return nil
}

func (c *GenerationCollector) Collect(handle runctl.GenerationHandle) (any, error) {
	g, err := c.resolve(handle)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return nil, ErrHandleReleased
	}
	if !g.settled {
		return nil, ErrNotDrained
	}
	return Frozen{Entries: append([]Entry(nil), g.frozen.Entries...), Dropped: g.frozen.Dropped}, nil
}

func (c *GenerationCollector) Release(handle runctl.GenerationHandle) {
	g, err := c.resolve(handle)
	if err != nil {
		return
	}
	g.mu.Lock()
	g.table = nil
	g.frozen = Frozen{}
	g.released = true
	g.mu.Unlock()
}

func (c *GenerationCollector) resolve(handle runctl.GenerationHandle) (*generation, error) {
	g, ok := handle.Token().(*generation)
	if !ok || g == nil || g.owner != c {
		return nil, fmt.Errorf("%w: %s", ErrForeignHandle, handle.Collector)
	}
	return g, nil
}
