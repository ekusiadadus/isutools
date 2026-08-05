package runctl

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Collector methods a fake can be scripted to panic in.
const (
	opBegin          = "BeginBoundary"
	opFreeze         = "Freeze"
	opDrain          = "Drain"
	opCollect        = "Collect"
	opRelease        = "Release"
	opCaptureBase    = "CaptureBaseline"
	opCaptureFinal   = "CaptureFinal"
	panicMessagePart = "scripted collector panic"
)

// panickingGeneration behaves like a working generation collector except in
// the one method it is scripted to blow up in.
type panickingGeneration struct {
	name string
	at   string

	gen      atomic.Uint64
	releases atomic.Int64
	collects atomic.Int64
}

func (g *panickingGeneration) boom(op string) {
	if g.at == op {
		panic(panicMessagePart + " in " + op)
	}
}

func (g *panickingGeneration) Name() string { return g.name }

func (g *panickingGeneration) BeginBoundary(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error) {
	g.boom(opBegin)
	return g.result(runID, ep), nil
}

func (g *panickingGeneration) Freeze(ctx context.Context, runID string, ep Epoch) (BoundaryResult, error) {
	g.boom(opFreeze)
	return g.result(runID, ep), nil
}

func (g *panickingGeneration) result(runID string, ep Epoch) BoundaryResult {
	n := g.gen.Add(1)
	return BoundaryResult{
		Handle:    NewGenerationHandle(runID, ep, g.name, n, n),
		At:        time.Now(),
		Committed: true,
	}
}

func (g *panickingGeneration) Drain(ctx context.Context, h GenerationHandle) error {
	g.boom(opDrain)
	return nil
}

func (g *panickingGeneration) Collect(h GenerationHandle) (any, error) {
	g.boom(opCollect)
	g.collects.Add(1)
	return g.name + "-interval", nil
}

func (g *panickingGeneration) Release(h GenerationHandle) {
	g.boom(opRelease)
	g.releases.Add(1)
}

// panickingBaseline is the baseline-side equivalent.
type panickingBaseline struct {
	name string
	at   string

	releases atomic.Int64
	collects atomic.Int64
}

func (b *panickingBaseline) boom(op string) {
	if b.at == op {
		panic(panicMessagePart + " in " + op)
	}
}

func (b *panickingBaseline) Name() string { return b.name }

func (b *panickingBaseline) CaptureBaseline(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	b.boom(opCaptureBase)
	return b.sample(runID, ep, PhaseStartBaseline), nil
}

func (b *panickingBaseline) CaptureFinal(ctx context.Context, runID string, ep Epoch) (SampleResult, error) {
	b.boom(opCaptureFinal)
	return b.sample(runID, ep, PhaseFinishFinal), nil
}

func (b *panickingBaseline) sample(runID string, ep Epoch, phase Phase) SampleResult {
	at := time.Now()
	return SampleResult{
		Handle:    NewBaselineHandle(runID, ep, b.name, phase, at, fakeSample{}),
		At:        at,
		Committed: true,
	}
}

func (b *panickingBaseline) Collect(base, final BaselineHandle) (any, error) {
	b.boom(opCollect)
	b.collects.Add(1)
	return fakeSample{}, nil
}

func (b *panickingBaseline) Release(h BaselineHandle) {
	b.boom(opRelease)
	b.releases.Add(1)
}

// requireContractViolation asserts that a collector's boundary was recorded as
// a contract violation carrying the panic text, and never as a silent success.
func requireContractViolation(t *testing.T, boundaries []CollectorBoundary, name string, phase Phase) CollectorBoundary {
	t.Helper()
	b, ok := findBoundary(boundaries, name, phase)
	if !ok {
		t.Fatalf("no %s boundary was recorded for %s: %#v", phase, name, boundaries)
	}
	if b.Code != CodeContractViolation {
		t.Fatalf("%s at %s: code = %q, want %q", name, phase, b.Code, CodeContractViolation)
	}
	if !strings.Contains(b.Err, panicMessagePart) {
		t.Fatalf("%s at %s: err = %q, want the panic value", name, phase, b.Err)
	}
	if strings.Contains(b.Err, "\n") || strings.Contains(b.Err, "goroutine ") {
		t.Fatalf("%s at %s: err = %q, want one short line without a stack", name, phase, b.Err)
	}
	return b
}

// TestPanicInStartBoundaryBecomesARecordedFailure covers the worst case for
// fail-open: ResetNow runs StartRun on the application's own request path, so
// a collector panicking in BeginBoundary must degrade the run rather than
// escape into the handler.
func TestPanicInStartBoundaryBecomesARecordedFailure(t *testing.T) {
	tests := []struct {
		name     string
		required bool
		validity Validity
		state    RunState
	}{
		{name: "required", required: true, validity: ValidityInvalid, state: StateAborted},
		{name: "optional", required: false, validity: ValidityPartial, state: StateStarted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c, hr := newTestController(t, nil)
			g := &panickingGeneration{name: "http", at: opBegin}
			if err := c.RegisterGeneration(Registration{Name: "http", Required: tt.required}, g); err != nil {
				t.Fatalf("RegisterGeneration: %v", err)
			}

			start, err := c.StartRun(ctx, StartRunOptions{})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			if start.Validity != tt.validity || start.State != tt.state {
				t.Fatalf("start = %s/%s, want %s/%s", start.State, start.Validity, tt.state, tt.validity)
			}
			b := requireContractViolation(t, start.Collectors, "http", PhaseStartBoundary)
			if b.Committed {
				t.Fatal("a panicking BeginBoundary was recorded as committed")
			}
			if !b.Dropped {
				t.Fatal("a panicking BeginBoundary kept its section")
			}
			if !hr.has(HealthContractViolation) {
				t.Fatalf("health key %s was not recorded", HealthContractViolation)
			}
		})
	}
}

// TestPanicInStartBaselineBecomesARecordedFailure is the baseline-side twin of
// the test above.
func TestPanicInStartBaselineBecomesARecordedFailure(t *testing.T) {
	ctx := context.Background()
	c, hr := newTestController(t, nil)
	b := &panickingBaseline{name: "proc", at: opCaptureBase}
	if err := c.RegisterBaseline(Registration{Name: "proc", Required: false}, b); err != nil {
		t.Fatalf("RegisterBaseline: %v", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.Validity != ValidityPartial || start.State != StateStarted {
		t.Fatalf("start = %s/%s, want started/partial", start.State, start.Validity)
	}
	rec := requireContractViolation(t, start.Collectors, "proc", PhaseStartBaseline)
	if rec.Committed {
		t.Fatal("a panicking CaptureBaseline was recorded as committed")
	}
	if !hr.has(HealthContractViolation) {
		t.Fatalf("health key %s was not recorded", HealthContractViolation)
	}
}

// TestPanicAtTheClosingBoundaryBecomesARecordedFailure covers Freeze and
// CaptureFinal: the run still finishes, because the measurement up to that
// point is real, but the panicking collector contributes nothing.
func TestPanicAtTheClosingBoundaryBecomesARecordedFailure(t *testing.T) {
	tests := []struct {
		name     string
		genAt    string
		baseAt   string
		phase    Phase
		who      string
		required bool
		validity Validity
	}{
		{name: "freeze required", genAt: opFreeze, phase: PhaseFinishFreeze, who: "http", required: true, validity: ValidityInvalid},
		{name: "freeze optional", genAt: opFreeze, phase: PhaseFinishFreeze, who: "http", required: false, validity: ValidityPartial},
		{name: "capture final required", baseAt: opCaptureFinal, phase: PhaseFinishFinal, who: "proc", required: true, validity: ValidityInvalid},
		{name: "capture final optional", baseAt: opCaptureFinal, phase: PhaseFinishFinal, who: "proc", required: false, validity: ValidityPartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c, hr := newTestController(t, nil)
			g := &panickingGeneration{name: "http", at: tt.genAt}
			b := &panickingBaseline{name: "proc", at: tt.baseAt}
			if err := c.RegisterGeneration(Registration{Name: "http", Required: tt.required}, g); err != nil {
				t.Fatalf("RegisterGeneration: %v", err)
			}
			if err := c.RegisterBaseline(Registration{Name: "proc", Required: tt.required}, b); err != nil {
				t.Fatalf("RegisterBaseline: %v", err)
			}

			start, err := c.StartRun(ctx, StartRunOptions{})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			accepted, err := c.FinishRun(ctx, start.RunID)
			if err != nil {
				t.Fatalf("FinishRun: %v", err)
			}
			if accepted.Validity != tt.validity {
				t.Fatalf("accepted validity = %s, want %s", accepted.Validity, tt.validity)
			}
			requireContractViolation(t, accepted.Collectors, tt.who, tt.phase)
			if !hr.has(HealthContractViolation) {
				t.Fatalf("health key %s was not recorded", HealthContractViolation)
			}

			status, err := c.Await(ctx, start.RunID)
			if err != nil {
				t.Fatalf("Await: %v", err)
			}
			if status.State != StateFinished {
				t.Fatalf("state = %s, want finished", status.State)
			}
			snap, err := c.SnapshotOf(start.RunID)
			if err != nil {
				t.Fatalf("SnapshotOf: %v", err)
			}
			if _, ok := snap.Sections[tt.who]; ok {
				t.Fatalf("the panicking collector %s still produced a section", tt.who)
			}
			if snap.Validity != tt.validity {
				t.Fatalf("snapshot validity = %s, want %s", snap.Validity, tt.validity)
			}
		})
	}
}

// TestPanicInDrainOrCollectDropsOnlyThatSection proves the background half of
// the barrier: a panic there runs on the Controller's own goroutine, where an
// escape would take the whole process down with it long after the request that
// started the run has returned.
func TestPanicInDrainOrCollectDropsOnlyThatSection(t *testing.T) {
	tests := []struct {
		name     string
		genAt    string
		baseAt   string
		who      string
		required bool
		validity Validity
	}{
		{name: "generation drain required", genAt: opDrain, who: "http", required: true, validity: ValidityInvalid},
		{name: "generation drain optional", genAt: opDrain, who: "http", required: false, validity: ValidityPartial},
		{name: "generation collect required", genAt: opCollect, who: "http", required: true, validity: ValidityInvalid},
		{name: "generation collect optional", genAt: opCollect, who: "http", required: false, validity: ValidityPartial},
		{name: "baseline collect required", baseAt: opCollect, who: "proc", required: true, validity: ValidityInvalid},
		{name: "baseline collect optional", baseAt: opCollect, who: "proc", required: false, validity: ValidityPartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			c, hr := newTestController(t, nil)
			g := &panickingGeneration{name: "http", at: tt.genAt}
			b := &panickingBaseline{name: "proc", at: tt.baseAt}
			survivor := newFakeGeneration("sql")
			if err := c.RegisterGeneration(Registration{Name: "http", Required: tt.required}, g); err != nil {
				t.Fatalf("RegisterGeneration(http): %v", err)
			}
			if err := c.RegisterGeneration(Registration{Name: "sql"}, survivor); err != nil {
				t.Fatalf("RegisterGeneration(sql): %v", err)
			}
			if err := c.RegisterBaseline(Registration{Name: "proc", Required: tt.required}, b); err != nil {
				t.Fatalf("RegisterBaseline: %v", err)
			}

			start, err := c.StartRun(ctx, StartRunOptions{})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			if _, err := c.FinishRun(ctx, start.RunID); err != nil {
				t.Fatalf("FinishRun: %v", err)
			}
			if _, err := c.Await(ctx, start.RunID); err != nil {
				t.Fatalf("Await: %v", err)
			}

			snap, err := c.SnapshotOf(start.RunID)
			if err != nil {
				t.Fatalf("SnapshotOf: %v", err)
			}
			if _, ok := snap.Sections[tt.who]; ok {
				t.Fatalf("the panicking collector %s still produced a section", tt.who)
			}
			if got := snap.Sections["sql"]; got != "sql-interval" {
				t.Fatalf("the healthy collector lost its section: %#v", snap.Sections)
			}
			if snap.Validity != tt.validity {
				t.Fatalf("snapshot validity = %s, want %s", snap.Validity, tt.validity)
			}
			requireContractViolation(t, snap.Collectors, tt.who, PhaseCollect)
			if !hr.has(HealthContractViolation) {
				t.Fatalf("health key %s was not recorded", HealthContractViolation)
			}
		})
	}
}

// TestPanicInReleaseDoesNotStrandOtherHandles pins the resource half of the
// contract: Release has no boundary record to fail into, so the bug is
// reported through health and every other handle is still freed.
func TestPanicInReleaseDoesNotStrandOtherHandles(t *testing.T) {
	ctx := context.Background()
	c, hr := newTestController(t, nil)
	g := &panickingGeneration{name: "http", at: opRelease}
	b := &panickingBaseline{name: "proc", at: opRelease}
	genSurvivor := newFakeGeneration("sql")
	baseSurvivor := newFakeBaseline("host")
	if err := c.RegisterGeneration(Registration{Name: "http", Required: true}, g); err != nil {
		t.Fatalf("RegisterGeneration(http): %v", err)
	}
	if err := c.RegisterGeneration(Registration{Name: "sql"}, genSurvivor); err != nil {
		t.Fatalf("RegisterGeneration(sql): %v", err)
	}
	if err := c.RegisterBaseline(Registration{Name: "proc", Required: true}, b); err != nil {
		t.Fatalf("RegisterBaseline(proc): %v", err)
	}
	if err := c.RegisterBaseline(Registration{Name: "host"}, baseSurvivor); err != nil {
		t.Fatalf("RegisterBaseline(host): %v", err)
	}

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if start.Validity != ValidityValid {
		t.Fatalf("start validity = %s, want valid: Release runs after the boundary", start.Validity)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := c.Await(ctx, start.RunID); err != nil {
		t.Fatalf("Await: %v", err)
	}

	snap, err := c.SnapshotOf(start.RunID)
	if err != nil {
		t.Fatalf("SnapshotOf: %v", err)
	}
	if snap.Validity != ValidityValid {
		t.Fatalf("snapshot validity = %s, want valid: a failed Release loses no data", snap.Validity)
	}
	if _, _, _, _, releases := genSurvivor.counts(); releases == 0 {
		t.Fatal("the panicking generation Release stranded the next collector's handles")
	}
	if _, _, releases := baseSurvivor.counts(); releases == 0 {
		t.Fatal("the panicking baseline Release stranded the next collector's handles")
	}
	if !hr.has(HealthContractViolation) {
		t.Fatalf("health key %s was not recorded", HealthContractViolation)
	}
}

// TestPanicInEnrichDegradesTheRun covers the one non-collector callback the
// Controller invokes: it runs on the background worker's goroutine, where an
// escaping panic has nothing above it but the process.
func TestPanicInEnrichDegradesTheRun(t *testing.T) {
	ctx := context.Background()
	c, hr := newTestController(t, func(o *Options) {
		o.Enrich = func(context.Context, *Snapshot) error { panic(panicMessagePart + " in Enrich") }
	})
	g := newFakeGeneration("http")
	registerPair(t, c, g, Registration{Name: "http", Required: true}, nil, Registration{})

	start, err := c.StartRun(ctx, StartRunOptions{})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.FinishRun(ctx, start.RunID); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	status, err := c.Await(ctx, start.RunID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if status.State != StateFinished || status.Validity != ValidityPartial {
		t.Fatalf("status = %s/%s, want finished/partial", status.State, status.Validity)
	}
	snap, err := c.SnapshotOf(start.RunID)
	if err != nil {
		t.Fatalf("SnapshotOf: %v", err)
	}
	if got := snap.Sections["http"]; got != "http-interval" {
		t.Fatalf("a panicking enrichment lost the collected sections: %#v", snap.Sections)
	}
	if !hr.has(HealthContractViolation) {
		t.Fatalf("health key %s was not recorded", HealthContractViolation)
	}
}

// TestPanicTextIsShortAndCarriesNoStack keeps the recorded panic value fit for
// a snapshot an operator reads: one short line, never a stack dump.
func TestPanicTextIsShortAndCarriesNoStack(t *testing.T) {
	long := strings.Repeat("goroutine 1 [running]:\nruntime.gopanic()\n", 40)
	_, err := safeResult("http", opCollect, func() (any, error) { panic(long) })
	if !isPanic(err) {
		t.Fatalf("err = %v, want a recovered panic", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("recorded panic spans several lines: %q", err.Error())
	}
	if n := len([]rune(err.Error())); n > panicTextMax+64 {
		t.Fatalf("recorded panic is %d runes long, want it truncated near %d", n, panicTextMax)
	}
	if !strings.HasSuffix(err.Error(), "...") {
		t.Fatalf("a truncated panic must say so: %q", err.Error())
	}

	// An empty panic value must still name the collector and the method, or
	// the record would say nothing at all.
	empty := safeErr("http", opDrain, func() error { panic("") })
	if !isPanic(empty) {
		t.Fatalf("err = %v, want a recovered panic", empty)
	}
	if !strings.Contains(empty.Error(), "collector http panicked in Drain") {
		t.Fatalf("err = %q, want the collector and the method it died in", empty.Error())
	}
}

// TestPanicBarrierHandlesUnprintableValues covers the second-order case: the
// barrier itself has to format the recovered value, and fmt re-panics when
// rendering that value panics again. A barrier that died there would be no
// barrier at all.
func TestPanicBarrierHandlesUnprintableValues(t *testing.T) {
	err := safeErr("http", opDrain, func() error { panic(unprintable{}) })
	if !isPanic(err) {
		t.Fatalf("err = %v, want a recovered panic", err)
	}
	if !strings.Contains(err.Error(), "unprintable panic value") {
		t.Fatalf("err = %q, want the unprintable fallback", err.Error())
	}
	if !strings.Contains(err.Error(), "collector http panicked in Drain") {
		t.Fatalf("err = %q, want the collector and the method it died in", err.Error())
	}
}

// unprintable panics with itself while being rendered, so fmt's own recovery
// hits a nested panic and gives up.
type unprintable struct{}

func (unprintable) String() string { panic(unprintable{}) }

// TestPanickingBudgetFailsRegistrationNotTheProcess covers the one collector
// call that happens before any run exists.
func TestPanickingBudgetFailsRegistrationNotTheProcess(t *testing.T) {
	c, _ := newTestController(t, nil)
	coll := budgetPanicGeneration{panickingGeneration: &panickingGeneration{name: "http"}}
	if err := c.RegisterGeneration(Registration{Name: "http"}, coll); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("RegisterGeneration = %v, want ErrInvalidRegistration", err)
	}
}

// budgetPanicGeneration is a collector whose declared budget cannot be read.
type budgetPanicGeneration struct{ *panickingGeneration }

func (budgetPanicGeneration) Budget() time.Duration { panic(panicMessagePart + " in Budget") }
