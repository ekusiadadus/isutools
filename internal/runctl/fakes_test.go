package runctl

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/health"
)

// fakeSample is deliberately a value type with no reference fields, so that a
// handle holder who ignores the read-only contract and mutates the result of
// Sample() still cannot reach another holder's copy. Real collectors are
// expected to follow the same shape.
type fakeSample struct {
	Value int64
}

// ioGuard records side effects that happen while it is armed. It is how the
// conformance tests prove that Collect(base, final) derives its interval from
// frozen samples instead of touching the collector, the database or procfs.
type ioGuard struct {
	mu    sync.Mutex
	armed bool
	hits  int
}

func (g *ioGuard) arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

func (g *ioGuard) touch() {
	g.mu.Lock()
	if g.armed {
		g.hits++
	}
	g.mu.Unlock()
}

func (g *ioGuard) violations() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

// fakeGeneration is a scriptable GenerationCollector.
type fakeGeneration struct {
	name string

	beginErr        error
	beginCommitted  bool
	beginAtShift    time.Duration
	beginDelay      time.Duration
	freezeErr       error
	freezeCommitted bool
	freezeAtShift   time.Duration
	drainErr        error
	drainBlock      chan struct{}
	drainIgnoresCtx bool
	collectErr      error
	value           any

	mu       sync.Mutex
	begins   int
	freezes  int
	drains   int
	collects int
	releases int
	nextGen  uint64
}

func newFakeGeneration(name string) *fakeGeneration {
	return &fakeGeneration{
		name:            name,
		beginCommitted:  true,
		freezeCommitted: true,
		value:           name + "-interval",
	}
}

func (f *fakeGeneration) Name() string { return f.name }

func (f *fakeGeneration) BeginBoundary(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error) {
	if f.beginDelay > 0 {
		timer := time.NewTimer(f.beginDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return BoundaryResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	f.begins++
	f.nextGen++
	gen := f.nextGen
	f.mu.Unlock()
	at := time.Now().Add(f.beginAtShift)
	return BoundaryResult{
		Handle:    NewGenerationHandle(runID, ep, f.name, gen, fmt.Sprintf("%s/%d", f.name, gen)),
		At:        at,
		Committed: f.beginCommitted,
	}, f.beginErr
}

func (f *fakeGeneration) Freeze(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error) {
	f.mu.Lock()
	f.freezes++
	gen := f.nextGen
	f.mu.Unlock()
	at := time.Now().Add(f.freezeAtShift)
	return BoundaryResult{
		Handle:    NewGenerationHandle(runID, ep, f.name, gen, fmt.Sprintf("%s/%d", f.name, gen)),
		At:        at,
		Committed: f.freezeCommitted,
	}, f.freezeErr
}

func (f *fakeGeneration) Drain(ctx context.Context, h GenerationHandle) error {
	f.mu.Lock()
	f.drains++
	block, ignore, err := f.drainBlock, f.drainIgnoresCtx, f.drainErr
	f.mu.Unlock()
	if block != nil {
		if ignore {
			<-block
		} else {
			select {
			case <-block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return err
}

func (f *fakeGeneration) Collect(h GenerationHandle) (any, error) {
	f.mu.Lock()
	f.collects++
	f.mu.Unlock()
	if f.collectErr != nil {
		return nil, f.collectErr
	}
	return f.value, nil
}

func (f *fakeGeneration) Release(h GenerationHandle) {
	f.mu.Lock()
	f.releases++
	f.mu.Unlock()
}

func (f *fakeGeneration) counts() (begins, freezes, drains, collects, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begins, f.freezes, f.drains, f.collects, f.releases
}

// fakeBaseline is a scriptable BaselineCollector.
type fakeBaseline struct {
	name string

	baseErr        error
	baseCommitted  bool
	baseAtShift    time.Duration
	finalErr       error
	finalCommitted bool
	finalAtShift   time.Duration
	captureDelay   time.Duration
	collectErr     error
	// collectTouchesLive simulates a collector that reaches into its own
	// mutable state instead of the frozen handles.
	collectTouchesLive bool

	io ioGuard

	mu       sync.Mutex
	live     int64
	captures int
	collects int
	releases int
}

func newFakeBaseline(name string) *fakeBaseline {
	return &fakeBaseline{name: name, baseCommitted: true, finalCommitted: true}
}

func (f *fakeBaseline) Name() string { return f.name }

func (f *fakeBaseline) setLive(v int64) {
	f.mu.Lock()
	f.live = v
	f.mu.Unlock()
}

func (f *fakeBaseline) readLive() int64 {
	f.io.touch()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live
}

func (f *fakeBaseline) capture(ctx context.Context, runID string, ep Epoch, phase Phase, shift time.Duration, committed bool, failure error) (SampleResult, error) {
	if f.captureDelay > 0 {
		timer := time.NewTimer(f.captureDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return SampleResult{}, ctx.Err()
		}
	}
	value := f.readLive()
	f.mu.Lock()
	f.captures++
	f.mu.Unlock()
	at := time.Now().Add(shift)
	return SampleResult{
		Handle:    NewBaselineHandle(runID, ep, f.name, phase, at, fakeSample{Value: value}),
		At:        at,
		Committed: committed,
	}, failure
}

func (f *fakeBaseline) CaptureBaseline(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	return f.capture(ctx, runID, ep, PhaseStartBaseline, f.baseAtShift, f.baseCommitted, f.baseErr)
}

func (f *fakeBaseline) CaptureFinal(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	return f.capture(ctx, runID, ep, PhaseFinishFinal, f.finalAtShift, f.finalCommitted, f.finalErr)
}

func (f *fakeBaseline) Collect(base, final BaselineHandle) (any, error) {
	f.mu.Lock()
	f.collects++
	f.mu.Unlock()
	if f.collectErr != nil {
		return nil, f.collectErr
	}
	if f.collectTouchesLive {
		// Deliberate contract violation used by the conformance test.
		f.readLive()
	}
	b, ok := base.Sample().(fakeSample)
	if !ok {
		return nil, fmt.Errorf("baseline sample of %s has type %T, want fakeSample", f.name, base.Sample())
	}
	e, ok := final.Sample().(fakeSample)
	if !ok {
		return nil, fmt.Errorf("final sample of %s has type %T, want fakeSample", f.name, final.Sample())
	}
	return fakeSample{Value: e.Value - b.Value}, nil
}

func (f *fakeBaseline) Release(h BaselineHandle) {
	f.mu.Lock()
	f.releases++
	f.mu.Unlock()
}

func (f *fakeBaseline) counts() (captures, collects, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captures, f.collects, f.releases
}

// recordingHealth captures the runctl-* keys a test provoked.
type recordingHealth struct {
	mu      sync.Mutex
	entries map[string]string
}

func newRecordingHealth() *recordingHealth {
	return &recordingHealth{entries: make(map[string]string)}
}

func (r *recordingHealth) Set(collector string, status health.Status, message string) {
	r.mu.Lock()
	r.entries[collector] = message
	r.mu.Unlock()
}

func (r *recordingHealth) has(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[key]
	return ok
}

// fakeClock is a manually advanced clock so lease and TTL behaviour can be
// tested without waiting out a twenty second lease.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testBudgets is a scaled-down but hierarchy-respecting budget table.
func testBudgets() Budgets {
	return Budgets{
		StartRun:               2 * time.Second,
		FinishSync:             2 * time.Second,
		PhaseBoundary:          400 * time.Millisecond,
		PhaseBaseline:          1200 * time.Millisecond,
		PhaseFreeze:            400 * time.Millisecond,
		PhaseFinal:             1200 * time.Millisecond,
		PerCollectorGeneration: 200 * time.Millisecond,
		PerCollectorBaseline:   600 * time.Millisecond,
		Drain:                  300 * time.Millisecond,
		SnapshotBuild:          300 * time.Millisecond,
		Enrich:                 100 * time.Millisecond,
		AbortJoin:              150 * time.Millisecond,
		DetachedReap:           2 * time.Second,
		FinishLease:            5 * time.Second,
		StartedTTL:             time.Minute,
		FinishedTTL:            time.Minute,
		TombstoneTTL:           time.Minute,
		NonceTTL:               time.Minute,
		Watchdog:               20 * time.Millisecond,
	}
}

// newTestController builds a Controller with test budgets, a recording health
// sink and the watchdog disabled so tests drive Sweep explicitly.
func newTestController(t *testing.T, mutate func(*Options)) (*Controller, *recordingHealth) {
	t.Helper()
	hr := newRecordingHealth()
	o := Options{
		Budgets:         testBudgets(),
		Health:          hr,
		DisableWatchdog: true,
	}
	if mutate != nil {
		mutate(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.Close)
	return c, hr
}

// registerPair registers one generation and one baseline collector.
func registerPair(t *testing.T, c *Controller, g *fakeGeneration, gr Registration, b *fakeBaseline, br Registration) {
	t.Helper()
	if g != nil {
		if err := c.RegisterGeneration(gr, g); err != nil {
			t.Fatalf("RegisterGeneration: %v", err)
		}
	}
	if b != nil {
		if err := c.RegisterBaseline(br, b); err != nil {
			t.Fatalf("RegisterBaseline: %v", err)
		}
	}
}

// findBoundary returns the boundary recorded for a collector in a phase.
func findBoundary(boundaries []CollectorBoundary, name string, phase Phase) (CollectorBoundary, bool) {
	for _, b := range boundaries {
		if b.Name == name && b.Phase == phase {
			return b, true
		}
	}
	return CollectorBoundary{}, false
}
