package runctl

import (
	"context"
	"errors"
	"testing"
	"time"
)

// errCollector is the injected collector failure used across the matrix.
var errCollector = errors.New("collector failed")

// stage says where a row's expectation is observed: at the opening boundary,
// at the closing boundary, or on the published snapshot.
type stage string

const (
	stageStart   stage = "start"
	stageFinish  stage = "finish"
	stageCollect stage = "collect"
)

// TestPhaseMatrix walks the phase x collector-kind x required result table.
// Every row states not just the resulting validity but whether the section
// survives, because "the run is partial" and "this collector's numbers are
// gone" are different facts and downstream advisors need both.
func TestPhaseMatrix(t *testing.T) {
	tests := []struct {
		row           string
		genRequired   bool
		baseRequired  bool
		setup         func(*fakeGeneration, *fakeBaseline)
		budgets       func(*Budgets)
		stage         stage
		wantValidity  Validity
		wantState     RunState
		wantName      string
		wantPhase     Phase
		wantCode      string
		wantCommitted bool
		wantDropped   bool
		wantSection   string // section that must be absent from the snapshot
	}{
		{
			row:         "1: start-boundary generation required, not committed",
			genRequired: true,
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.beginErr, g.beginCommitted = errCollector, false
			},
			stage:        stageStart,
			wantValidity: ValidityInvalid,
			wantState:    StateAborted,
			wantName:     "http",
			wantPhase:    PhaseStartBoundary,
			wantCode:     CodeBoundaryFailed,
			wantDropped:  true,
		},
		{
			row:         "2: start-boundary generation required, committed",
			genRequired: true,
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.beginErr, g.beginCommitted = errCollector, true
			},
			stage:         stageStart,
			wantValidity:  ValidityInvalid,
			wantState:     StateAborted,
			wantName:      "http",
			wantPhase:     PhaseStartBoundary,
			wantCode:      CodeBoundaryFailed,
			wantCommitted: true,
			wantDropped:   true,
		},
		{
			row: "3: start-boundary generation optional, not committed",
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.beginErr, g.beginCommitted = errCollector, false
			},
			stage:        stageStart,
			wantValidity: ValidityPartial,
			wantState:    StateStarted,
			wantName:     "http",
			wantPhase:    PhaseStartBoundary,
			wantCode:     CodeBoundaryFailed,
			wantDropped:  true,
		},
		{
			row: "4: start-boundary generation optional, committed",
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.beginErr, g.beginCommitted = errCollector, true
			},
			stage:         stageStart,
			wantValidity:  ValidityPartial,
			wantState:     StateStarted,
			wantName:      "http",
			wantPhase:     PhaseStartBoundary,
			wantCode:      CodeBoundaryFailed,
			wantCommitted: true,
			wantDropped:   true,
		},
		{
			row:          "5: start-baseline required error",
			baseRequired: true,
			setup: func(_ *fakeGeneration, b *fakeBaseline) {
				b.baseErr, b.baseCommitted = errCollector, false
			},
			stage:        stageStart,
			wantValidity: ValidityInvalid,
			wantState:    StateAborted,
			wantName:     "proc",
			wantPhase:    PhaseStartBaseline,
			wantCode:     CodeBoundaryFailed,
			wantDropped:  true,
		},
		{
			row: "6: start-baseline optional error",
			setup: func(_ *fakeGeneration, b *fakeBaseline) {
				b.baseErr, b.baseCommitted = errCollector, false
			},
			stage:        stageStart,
			wantValidity: ValidityPartial,
			wantState:    StateStarted,
			wantName:     "proc",
			wantPhase:    PhaseStartBaseline,
			wantCode:     CodeBoundaryFailed,
			wantDropped:  true,
		},
		{
			row:          "7a: start-baseline required, budget exhausted",
			baseRequired: true,
			setup: func(_ *fakeGeneration, b *fakeBaseline) {
				b.captureDelay = 200 * time.Millisecond
			},
			budgets: func(b *Budgets) {
				b.PhaseBaseline = 60 * time.Millisecond
				b.PerCollectorBaseline = 30 * time.Millisecond
			},
			stage:        stageStart,
			wantValidity: ValidityInvalid,
			wantState:    StateAborted,
			wantName:     "proc",
			wantPhase:    PhaseStartBaseline,
			wantCode:     CodeNotCaptured,
			wantDropped:  true,
		},
		{
			row: "7b: start-baseline optional, budget exhausted",
			setup: func(_ *fakeGeneration, b *fakeBaseline) {
				b.captureDelay = 200 * time.Millisecond
			},
			budgets: func(b *Budgets) {
				b.PhaseBaseline = 60 * time.Millisecond
				b.PerCollectorBaseline = 30 * time.Millisecond
			},
			stage:        stageStart,
			wantValidity: ValidityPartial,
			wantState:    StateStarted,
			wantName:     "proc",
			wantPhase:    PhaseStartBaseline,
			wantCode:     CodeNotCaptured,
			wantDropped:  true,
		},
		{
			row:         "8: finish-freeze generation required, not committed",
			genRequired: true,
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.freezeErr, g.freezeCommitted = errCollector, false
			},
			stage:        stageFinish,
			wantValidity: ValidityInvalid,
			wantName:     "http",
			wantPhase:    PhaseFinishFreeze,
			wantCode:     CodeBoundaryFailed,
			wantDropped:  true,
			wantSection:  "http",
		},
		{
			row:         "9: finish-freeze generation required, committed keeps its data",
			genRequired: true,
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.freezeErr, g.freezeCommitted = errCollector, true
			},
			stage:         stageFinish,
			wantValidity:  ValidityInvalid,
			wantName:      "http",
			wantPhase:     PhaseFinishFreeze,
			wantCode:      CodeBoundaryFailed,
			wantCommitted: true,
			wantDropped:   false,
		},
		{
			row: "10a: finish-freeze generation optional, not committed",
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.freezeErr, g.freezeCommitted = errCollector, false
			},
			stage:        stageFinish,
			wantValidity: ValidityPartial,
			wantName:     "http",
			wantPhase:    PhaseFinishFreeze,
			wantCode:     CodeBoundaryFailed,
			wantDropped:  true,
			wantSection:  "http",
		},
		{
			row: "10b: finish-freeze generation optional, committed",
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.freezeErr, g.freezeCommitted = errCollector, true
			},
			stage:         stageFinish,
			wantValidity:  ValidityPartial,
			wantName:      "http",
			wantPhase:     PhaseFinishFreeze,
			wantCode:      CodeBoundaryFailed,
			wantCommitted: true,
			wantDropped:   false,
		},
		{
			row:          "11: finish-final baseline required error",
			baseRequired: true,
			setup: func(_ *fakeGeneration, b *fakeBaseline) {
				b.finalErr, b.finalCommitted = errCollector, false
			},
			stage:        stageFinish,
			wantValidity: ValidityInvalid,
			wantName:     "proc",
			wantPhase:    PhaseFinishFinal,
			wantCode:     CodeBoundaryFailed,
			wantDropped:  true,
			wantSection:  "proc",
		},
		{
			row: "12: finish-final baseline optional error",
			setup: func(_ *fakeGeneration, b *fakeBaseline) {
				b.finalErr, b.finalCommitted = errCollector, false
			},
			stage:        stageFinish,
			wantValidity: ValidityPartial,
			wantName:     "proc",
			wantPhase:    PhaseFinishFinal,
			wantCode:     CodeBoundaryFailed,
			wantDropped:  true,
			wantSection:  "proc",
		},
		{
			row:         "13: collect generation required, drain timeout keeps partial data",
			genRequired: true,
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.drainBlock = make(chan struct{})
			},
			budgets:       func(b *Budgets) { b.Drain = 40 * time.Millisecond },
			stage:         stageCollect,
			wantValidity:  ValidityPartial,
			wantName:      "http",
			wantPhase:     PhaseCollect,
			wantCode:      CodeDrainTimeout,
			wantCommitted: true,
		},
		{
			row: "14: collect generation optional, drain timeout keeps partial data",
			setup: func(g *fakeGeneration, _ *fakeBaseline) {
				g.drainBlock = make(chan struct{})
			},
			budgets:       func(b *Budgets) { b.Drain = 40 * time.Millisecond },
			stage:         stageCollect,
			wantValidity:  ValidityPartial,
			wantName:      "http",
			wantPhase:     PhaseCollect,
			wantCode:      CodeDrainTimeout,
			wantCommitted: true,
		},
		{
			row:          "15a: collect generation required error",
			genRequired:  true,
			setup:        func(g *fakeGeneration, _ *fakeBaseline) { g.collectErr = errCollector },
			stage:        stageCollect,
			wantValidity: ValidityInvalid,
			wantName:     "http",
			wantPhase:    PhaseCollect,
			wantCode:     CodeCollectFailed,
			wantDropped:  true,
			wantSection:  "http",
		},
		{
			row:          "15b: collect baseline required error",
			baseRequired: true,
			setup:        func(_ *fakeGeneration, b *fakeBaseline) { b.collectErr = errCollector },
			stage:        stageCollect,
			wantValidity: ValidityInvalid,
			wantName:     "proc",
			wantPhase:    PhaseCollect,
			wantCode:     CodeCollectFailed,
			wantDropped:  true,
			wantSection:  "proc",
		},
		{
			row:          "16a: collect generation optional error",
			setup:        func(g *fakeGeneration, _ *fakeBaseline) { g.collectErr = errCollector },
			stage:        stageCollect,
			wantValidity: ValidityPartial,
			wantName:     "http",
			wantPhase:    PhaseCollect,
			wantCode:     CodeCollectFailed,
			wantDropped:  true,
			wantSection:  "http",
		},
		{
			row:          "16b: collect baseline optional error",
			setup:        func(_ *fakeGeneration, b *fakeBaseline) { b.collectErr = errCollector },
			stage:        stageCollect,
			wantValidity: ValidityPartial,
			wantName:     "proc",
			wantPhase:    PhaseCollect,
			wantCode:     CodeCollectFailed,
			wantDropped:  true,
			wantSection:  "proc",
		},
		{
			row:         "17a: boundary spread over the limit for an optional collector only",
			genRequired: true,
			setup: func(_ *fakeGeneration, b *fakeBaseline) {
				b.baseAtShift = 3 * time.Second
			},
			stage:         stageStart,
			wantValidity:  ValidityPartial,
			wantState:     StateStarted,
			wantName:      "proc",
			wantPhase:     PhaseStartBaseline,
			wantCode:      CodeSpreadExceeded,
			wantCommitted: true,
		},
		{
			row:          "17b: boundary spread over the limit for a required collector",
			genRequired:  true,
			baseRequired: true,
			setup: func(_ *fakeGeneration, b *fakeBaseline) {
				b.baseAtShift = 3 * time.Second
			},
			stage:         stageStart,
			wantValidity:  ValidityInvalid,
			wantState:     StateAborted,
			wantName:      "proc",
			wantPhase:     PhaseStartBaseline,
			wantCode:      CodeSpreadExceeded,
			wantCommitted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.row, func(t *testing.T) {
			ctx := context.Background()
			c, _ := newTestController(t, func(o *Options) {
				if tt.budgets != nil {
					tt.budgets(&o.Budgets)
				}
			})
			g, b := newFakeGeneration("http"), newFakeBaseline("proc")
			tt.setup(g, b)
			registerPair(t, c,
				g, Registration{Name: "http", Required: tt.genRequired},
				b, Registration{Name: "proc", Required: tt.baseRequired})

			start, err := c.StartRun(ctx, StartRunOptions{})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}

			if tt.stage == stageStart {
				if start.Validity != tt.wantValidity {
					t.Fatalf("start validity = %s, want %s", start.Validity, tt.wantValidity)
				}
				if start.State != tt.wantState {
					t.Fatalf("start state = %s, want %s", start.State, tt.wantState)
				}
				assertBoundary(t, start.Collectors, tt.wantName, tt.wantPhase, tt.wantCode, tt.wantCommitted, tt.wantDropped)
				if tt.wantState == StateAborted {
					status, _ := c.Status(start.RunID)
					if status.State != StateAborted || status.Reason != ReasonRequiredFailed {
						t.Fatalf("aborted run record = %#v", status)
					}
					if _, err := c.SnapshotOf(start.RunID); err == nil {
						t.Fatal("a run aborted at its opening boundary must never publish a snapshot")
					}
				}
				return
			}

			if start.Validity != ValidityValid {
				t.Fatalf("the opening boundary must be clean for this row, got %s", start.Validity)
			}
			accepted, err := c.FinishRun(ctx, start.RunID)
			if err != nil {
				t.Fatalf("FinishRun: %v", err)
			}

			if tt.stage == stageFinish {
				if accepted.Validity != tt.wantValidity {
					t.Fatalf("accepted validity = %s, want %s", accepted.Validity, tt.wantValidity)
				}
				assertBoundary(t, accepted.Collectors, tt.wantName, tt.wantPhase, tt.wantCode, tt.wantCommitted, tt.wantDropped)
			}

			status, err := c.Await(ctx, start.RunID)
			if err != nil {
				t.Fatalf("Await: %v", err)
			}
			if status.State != StateFinished {
				t.Fatalf("state = %s, want finished: the closing side keeps its data even when invalid", status.State)
			}
			if status.Validity != tt.wantValidity {
				t.Fatalf("final validity = %s, want %s", status.Validity, tt.wantValidity)
			}

			snap, err := c.SnapshotOf(start.RunID)
			if err != nil {
				t.Fatalf("SnapshotOf: %v", err)
			}
			if tt.stage == stageCollect {
				assertBoundary(t, snap.Collectors, tt.wantName, tt.wantPhase, tt.wantCode, tt.wantCommitted, tt.wantDropped)
			}
			if tt.wantSection != "" {
				if _, ok := snap.Sections[tt.wantSection]; ok {
					t.Fatalf("section %q must be excluded from the snapshot", tt.wantSection)
				}
			}
		})
	}
}

// assertBoundary checks one collector's record for a phase.
func assertBoundary(t *testing.T, boundaries []CollectorBoundary, name string, phase Phase, code string, committed, dropped bool) {
	t.Helper()
	got, ok := findBoundary(boundaries, name, phase)
	if !ok {
		t.Fatalf("no boundary recorded for %s at %s; have %#v", name, phase, boundaries)
	}
	if got.Code != code {
		t.Fatalf("%s@%s code = %q, want %q", name, phase, got.Code, code)
	}
	if got.Committed != committed {
		t.Fatalf("%s@%s committed = %v, want %v", name, phase, got.Committed, committed)
	}
	if got.Dropped != dropped {
		t.Fatalf("%s@%s dropped = %v, want %v", name, phase, got.Dropped, dropped)
	}
}

// TestBoundarySpread_PartialAndInvalid states the spread rule on its own: a
// window blown open by optional collectors still yields a usable interval, one
// blown open by the required collectors does not.
func TestBoundarySpread_PartialAndInvalid(t *testing.T) {
	ctx := context.Background()

	t.Run("optional collector widens the window", func(t *testing.T) {
		c, hr := newTestController(t, nil)
		g, b := newFakeGeneration("http"), newFakeBaseline("proc")
		b.baseAtShift = 4 * time.Second
		registerPair(t, c, g, Registration{Name: "http", Required: true}, b, Registration{Name: "proc"})

		start, err := c.StartRun(ctx, StartRunOptions{})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if start.Validity != ValidityPartial {
			t.Fatalf("validity = %s, want partial", start.Validity)
		}
		if start.BoundaryWindow.Spread <= SpreadLimitBoundary {
			t.Fatalf("spread = %v, expected the injected skew to exceed %v", start.BoundaryWindow.Spread, SpreadLimitBoundary)
		}
		if !hr.has(HealthBoundarySpread) {
			t.Fatalf("health key %s was not recorded", HealthBoundarySpread)
		}
	})

	t.Run("generation collectors drift apart", func(t *testing.T) {
		c, _ := newTestController(t, nil)
		first, second := newFakeGeneration("http"), newFakeGeneration("sql")
		second.beginAtShift = 400 * time.Millisecond
		registerPair(t, c, first, Registration{Name: "http", Required: true}, nil, Registration{})
		if err := c.RegisterGeneration(Registration{Name: "sql", Required: true}, second); err != nil {
			t.Fatalf("RegisterGeneration: %v", err)
		}

		start, err := c.StartRun(ctx, StartRunOptions{})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if start.Validity != ValidityInvalid || start.State != StateAborted {
			t.Fatalf("start = %s/%s, want aborted/invalid", start.State, start.Validity)
		}
		if start.GenerationWindow.Spread <= SpreadLimitGeneration {
			t.Fatalf("generation spread = %v, want over %v", start.GenerationWindow.Spread, SpreadLimitGeneration)
		}
	})

	t.Run("a boundary inside its limit stays valid", func(t *testing.T) {
		c, hr := newTestController(t, nil)
		g, b := newFakeGeneration("http"), newFakeBaseline("proc")
		registerPair(t, c, g, Registration{Name: "http", Required: true}, b, Registration{Name: "proc", Required: true})

		start, err := c.StartRun(ctx, StartRunOptions{})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if start.Validity != ValidityValid {
			t.Fatalf("validity = %s, want valid", start.Validity)
		}
		if hr.has(HealthBoundarySpread) {
			t.Fatal("a boundary inside its limit must not report a spread problem")
		}
	})
}

// TestBoundaryResult_CommittedOnError pins the Committed semantics, including
// the one combination that can only be a collector bug.
func TestBoundaryResult_CommittedOnError(t *testing.T) {
	ctx := context.Background()

	t.Run("nil error with Committed=false is a contract violation", func(t *testing.T) {
		c, hr := newTestController(t, nil)
		g := newFakeGeneration("http")
		g.beginCommitted = false // success reported without switching
		registerPair(t, c, g, Registration{Name: "http"}, nil, Registration{})

		start, err := c.StartRun(ctx, StartRunOptions{})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if start.Validity != ValidityInvalid {
			t.Fatalf("validity = %s: a contract violation must be treated as a required failure", start.Validity)
		}
		assertBoundary(t, start.Collectors, "http", PhaseStartBoundary, CodeContractViolation, false, true)
		if !hr.has(HealthContractViolation) {
			t.Fatalf("health key %s was not recorded", HealthContractViolation)
		}
	})

	t.Run("baseline contract violation", func(t *testing.T) {
		c, hr := newTestController(t, nil)
		b := newFakeBaseline("proc")
		b.baseCommitted = false
		registerPair(t, c, nil, Registration{}, b, Registration{Name: "proc"})

		start, err := c.StartRun(ctx, StartRunOptions{})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if start.Validity != ValidityInvalid {
			t.Fatalf("validity = %s, want invalid", start.Validity)
		}
		assertBoundary(t, start.Collectors, "proc", PhaseStartBaseline, CodeContractViolation, false, true)
		if !hr.has(HealthContractViolation) {
			t.Fatalf("health key %s was not recorded", HealthContractViolation)
		}
	})

	t.Run("error with Committed=true keeps the handle alive", func(t *testing.T) {
		c, _ := newTestController(t, nil)
		g := newFakeGeneration("http")
		registerPair(t, c, g, Registration{Name: "http"}, nil, Registration{})

		start, err := c.StartRun(ctx, StartRunOptions{})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		g.mu.Lock()
		g.freezeErr, g.freezeCommitted = errCollector, true
		g.mu.Unlock()

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
		if _, ok := snap.Sections["http"]; !ok {
			t.Fatal("a committed freeze must still contribute its data")
		}
		if _, _, _, collects, releases := g.counts(); collects == 0 || releases == 0 {
			t.Fatalf("handle lifecycle: collects=%d releases=%d", collects, releases)
		}
	})
}

func TestComputeWindowIgnoresCollectorsThatNeverRan(t *testing.T) {
	base := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	boundaries := []CollectorBoundary{
		{Name: "a", Kind: KindGeneration, At: base},
		{Name: "b", Kind: KindBaseline, At: base.Add(200 * time.Millisecond)},
		{Name: "c", Kind: KindBaseline}, // never captured
	}
	all := computeWindow(boundaries, nil)
	if all.Spread != 200*time.Millisecond || !all.Min.Equal(base) {
		t.Fatalf("window = %#v", all)
	}
	gen := computeWindow(boundaries, keepGeneration)
	if gen.Spread != 0 || !gen.Min.Equal(base) {
		t.Fatalf("generation window = %#v", gen)
	}
	empty := computeWindow(nil, nil)
	if !empty.Min.IsZero() || empty.Spread != 0 {
		t.Fatalf("empty window = %#v", empty)
	}
}
