package runctl

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// captureOrder records the order baseline captures ran in, and lets a
// SerialOnly capture notice a parallel one starting underneath it.
type captureOrder struct {
	mu     sync.Mutex
	events []string

	parallelEntered chan struct{}
	parallelOnce    sync.Once
	overlapped      atomic.Bool
}

func newCaptureOrder() *captureOrder {
	return &captureOrder{parallelEntered: make(chan struct{})}
}

func (o *captureOrder) note(event string) {
	o.mu.Lock()
	o.events = append(o.events, event)
	o.mu.Unlock()
}

func (o *captureOrder) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

// orderedBaseline reports when its capture ran. The serial one then waits a
// window for a parallel capture to appear: the correct phase cannot produce
// that signal, because it does not launch the parallel group until every
// SerialOnly collector has returned. A phase that ignored the flag would launch
// both together and be caught here.
type orderedBaseline struct {
	*fakeBaseline
	order  *captureOrder
	serial bool
}

func (b orderedBaseline) CaptureBaseline(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	b.order.note(b.Name() + ":enter")
	if b.serial {
		timer := time.NewTimer(40 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-b.order.parallelEntered:
			b.order.overlapped.Store(true)
		case <-timer.C:
		}
	} else {
		b.order.parallelOnce.Do(func() { close(b.order.parallelEntered) })
	}
	res, err := b.fakeBaseline.CaptureBaseline(ctx, runID, ep)
	b.order.note(b.Name() + ":exit")
	return res, err
}

// TestSerialOnlyBaselineRunsBeforeTheParallelGroup covers the registration
// flag that trades boundary width for sampling safety: a SerialOnly collector
// samples alone, and the parallel group only starts once it is done.
func TestSerialOnlyBaselineRunsBeforeTheParallelGroup(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, nil)
	order := newCaptureOrder()
	serial := orderedBaseline{fakeBaseline: newFakeBaseline("serial"), order: order, serial: true}
	parallel := orderedBaseline{fakeBaseline: newFakeBaseline("parallel"), order: order}
	if err := c.RegisterBaseline(Registration{Name: "serial", SerialOnly: true}, serial); err != nil {
		t.Fatalf("RegisterBaseline: %v", err)
	}
	if err := c.RegisterBaseline(Registration{Name: "parallel"}, parallel); err != nil {
		t.Fatalf("RegisterBaseline: %v", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.Validity != ValidityValid || len(start.Collectors) != 2 {
		t.Fatalf("start = %#v", start)
	}
	for _, name := range []string{"serial", "parallel"} {
		if b, ok := findBoundary(start.Collectors, name, PhaseStartBaseline); !ok || !b.Committed {
			t.Fatalf("%s was not sampled: %#v", name, b)
		}
	}
	if order.overlapped.Load() {
		t.Fatal("a parallel capture started while the SerialOnly collector was still sampling")
	}
	want := []string{"serial:enter", "serial:exit", "parallel:enter", "parallel:exit"}
	if got := order.seen(); !reflect.DeepEqual(got, want) {
		t.Fatalf("capture order = %v, want %v", got, want)
	}
	// Registration order is preserved in the record regardless of execution order.
	if start.Collectors[0].Name != "serial" {
		t.Fatalf("collector order = %q, want registration order", start.Collectors[0].Name)
	}
}

// concurrencyProbe observes how many baseline captures were in flight at once.
// Captures park until the cap is reached rather than sleeping, so the
// high-water mark is a fact the phase produced and not an inference from
// timing: with the cap honoured it is exactly BaselineConcurrency, and a phase
// that fanned out unbounded would drive it to the number of collectors.
type concurrencyProbe struct {
	mu     sync.Mutex
	inside int
	peak   int

	full     chan struct{}
	fullOnce sync.Once
}

func newConcurrencyProbe() *concurrencyProbe {
	return &concurrencyProbe{full: make(chan struct{})}
}

func (p *concurrencyProbe) enter() {
	p.mu.Lock()
	p.inside++
	if p.inside > p.peak {
		p.peak = p.inside
	}
	reached := p.inside >= BaselineConcurrency
	p.mu.Unlock()
	if reached {
		p.fullOnce.Do(func() { close(p.full) })
	}
}

func (p *concurrencyProbe) leave() {
	p.mu.Lock()
	p.inside--
	p.mu.Unlock()
}

func (p *concurrencyProbe) highWater() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// probingBaseline holds its capture open until the concurrency cap is reached,
// so every slot the phase is willing to hand out is occupied at the same
// instant, and then holds it open a moment longer: a phase that fanned out
// without a cap keeps admitting captures during that window and drives the
// high-water mark past it. The fallback timer keeps a cap the phase never
// reaches from wedging the test; the phase budget then reports the shortfall.
type probingBaseline struct {
	*fakeBaseline
	probe *concurrencyProbe
}

func (b probingBaseline) CaptureBaseline(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	b.probe.enter()
	defer b.probe.leave()

	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-b.probe.full:
	case <-timer.C:
	case <-ctx.Done():
	}

	grace := time.NewTimer(30 * time.Millisecond)
	defer grace.Stop()
	select {
	case <-grace.C:
	case <-ctx.Done():
	}
	return b.fakeBaseline.CaptureBaseline(ctx, runID, ep)
}

func TestManyBaselineCollectorsRespectTheConcurrencyCap(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestController(t, nil)

	const count = BaselineConcurrency * 2
	probe := newConcurrencyProbe()
	for i := 0; i < count; i++ {
		name := "base-" + string(rune('a'+i))
		coll := probingBaseline{fakeBaseline: newFakeBaseline(name), probe: probe}
		if err := c.RegisterBaseline(Registration{Name: name}, coll); err != nil {
			t.Fatalf("RegisterBaseline: %v", err)
		}
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if len(start.Collectors) != count {
		t.Fatalf("recorded %d collectors, want %d", len(start.Collectors), count)
	}
	if start.Validity != ValidityValid {
		t.Fatalf("validity = %s, want valid", start.Validity)
	}
	// Exactly the cap: anything higher means the phase let a thundering herd of
	// collectors at the measured application, anything lower means the cap was
	// never actually reached and this test proved nothing about it.
	if got := probe.highWater(); got != BaselineConcurrency {
		t.Fatalf("%d captures were in flight at once, want exactly the cap of %d", got, BaselineConcurrency)
	}
}
