package runctl

import (
	"errors"
	"testing"
	"time"
)

// TestBudgetHierarchy pins every inequality of the hierarchical budget model.
// The numbers themselves are cited by downstream collectors, so an accidental
// inversion here would silently make a documented per-target budget
// unreachable.
func TestBudgetHierarchy(t *testing.T) {
	tests := []struct {
		name          string
		child, parent time.Duration
		childName     string
		parentName    string
		allowEqual    bool
	}{
		{"start run over baseline phase", PhaseStartBaselineBudget, StartRunBudget, "PhaseStartBaselineBudget", "StartRunBudget", false},
		{"baseline phase over per-collector", PerCollectorBaselineBudget, PhaseStartBaselineBudget, "PerCollectorBaselineBudget", "PhaseStartBaselineBudget", false},
		{"per-collector baseline over per-target", PerTargetBudget, PerCollectorBaselineBudget, "PerTargetBudget", "PerCollectorBaselineBudget", false},
		{"start run over boundary phase", PhaseStartBoundaryBudget, StartRunBudget, "PhaseStartBoundaryBudget", "StartRunBudget", false},
		{"boundary phase over per-collector", PerCollectorGenerationBudget, PhaseStartBoundaryBudget, "PerCollectorGenerationBudget", "PhaseStartBoundaryBudget", false},
		{"finish sync over final phase", PhaseFinishFinalBudget, FinishSyncBudget, "PhaseFinishFinalBudget", "FinishSyncBudget", false},
		{"finish sync over freeze phase", PhaseFinishFreezeBudget, FinishSyncBudget, "PhaseFinishFreezeBudget", "FinishSyncBudget", false},
		{"freeze phase over per-collector", PerCollectorGenerationBudget, PhaseFinishFreezeBudget, "PerCollectorGenerationBudget", "PhaseFinishFreezeBudget", false},
		{"final phase over per-collector", PerCollectorBaselineBudget, PhaseFinishFinalBudget, "PerCollectorBaselineBudget", "PhaseFinishFinalBudget", false},
		{"start phases fit in the run budget", PhaseStartBoundaryBudget + PhaseStartBaselineBudget, StartRunBudget, "PhaseStartBoundaryBudget+PhaseStartBaselineBudget", "StartRunBudget", true},
		{"finish phases fit in the sync budget", PhaseFinishFreezeBudget + PhaseFinishFinalBudget, FinishSyncBudget, "PhaseFinishFreezeBudget+PhaseFinishFinalBudget", "FinishSyncBudget", true},
		{"synchronous finish fits in the finish lease", FinishSyncBudget, FinishLease, "FinishSyncBudget", "FinishLease", false},
		{"background work fits in the finish lease", DrainBudget + SnapshotBuildBudget + EnrichBudget, FinishLease, "DrainBudget+SnapshotBuildBudget+EnrichBudget", "FinishLease", false},
		{"drain cancel grace under the drain budget", DrainCancelGrace, DrainBudget, "DrainCancelGrace", "DrainBudget", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok := tt.child < tt.parent
			if tt.allowEqual {
				ok = tt.child <= tt.parent
			}
			if !ok {
				op := "<"
				if tt.allowEqual {
					op = "<="
				}
				t.Fatalf("%s(%v) %s %s(%v) does not hold", tt.childName, tt.child, op, tt.parentName, tt.parent)
			}
		})
	}

	if PreemptTotalBudget != AbortJoinBudget+StartRunBudget {
		t.Fatalf("PreemptTotalBudget = %v, want AbortJoinBudget+StartRunBudget = %v", PreemptTotalBudget, AbortJoinBudget+StartRunBudget)
	}
	if SpreadLimitGeneration >= SpreadLimitBoundary {
		t.Fatalf("SpreadLimitGeneration(%v) must be tighter than SpreadLimitBoundary(%v)", SpreadLimitGeneration, SpreadLimitBoundary)
	}
	if RetainedRuns < 2 {
		t.Fatalf("RetainedRuns = %d, want at least the in-flight run plus one history entry", RetainedRuns)
	}
	defaults := Budgets{}.withDefaults()
	if err := defaults.Validate(); err != nil {
		t.Fatalf("the default budget table must validate: %v", err)
	}
}

func TestBudgetsWithDefaultsFillsZeroFields(t *testing.T) {
	b := Budgets{StartRun: 3 * time.Second}.withDefaults()
	if b.StartRun != 3*time.Second {
		t.Fatalf("StartRun = %v, want the caller's override", b.StartRun)
	}
	if b.PhaseBaseline != PhaseStartBaselineBudget {
		t.Fatalf("PhaseBaseline = %v, want the package default", b.PhaseBaseline)
	}
	if b.Watchdog != WatchdogInterval || b.SpreadBoundary != SpreadLimitBoundary {
		t.Fatalf("defaults not applied: %#v", b)
	}
}

func TestBudgetsValidateRejectsInversion(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Budgets)
	}{
		{"per-collector baseline over its phase", func(b *Budgets) { b.PerCollectorBaseline = b.PhaseBaseline }},
		{"phase over the run budget", func(b *Budgets) { b.PhaseBaseline = b.StartRun }},
		{"phases exceeding the run budget", func(b *Budgets) { b.PhaseBoundary = b.StartRun - b.PhaseBaseline + time.Millisecond }},
		{"background work exceeding the lease", func(b *Budgets) { b.FinishLease = b.Drain }},
		// The synchronous freeze runs under the lease FinishRun arms, so a
		// table that lets it outlive that lease schedules its own abort.
		{"synchronous finish exceeding the lease", func(b *Budgets) { b.FinishSync = b.FinishLease + time.Millisecond }},
		{"per-collector generation over its phase", func(b *Budgets) { b.PerCollectorGeneration = b.PhaseBoundary + time.Millisecond }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := testBudgets().withDefaults()
			tt.mutate(&b)
			err := b.Validate()
			if !errors.Is(err, ErrBudgetInversion) {
				t.Fatalf("Validate() = %v, want ErrBudgetInversion", err)
			}
		})
	}
}

func TestNewRejectsInvertedBudgets(t *testing.T) {
	b := testBudgets()
	b.PerCollectorBaseline = b.PhaseBaseline * 2
	if _, err := New(Options{Budgets: b, DisableWatchdog: true}); !errors.Is(err, ErrBudgetInversion) {
		t.Fatalf("New = %v, want ErrBudgetInversion", err)
	}
}

// greedyGeneration declares a per-operation budget larger than its parent.
type greedyGeneration struct {
	*fakeGeneration
	want time.Duration
}

func (g greedyGeneration) Budget() time.Duration { return g.want }

// greedyBaseline declares a per-operation budget larger than its parent.
type greedyBaseline struct {
	*fakeBaseline
	want time.Duration
}

func (g greedyBaseline) Budget() time.Duration { return g.want }

func TestRegisterRejectsBudgetInversion(t *testing.T) {
	c, _ := newTestController(t, nil)

	gen := greedyGeneration{fakeGeneration: newFakeGeneration("http"), want: time.Hour}
	if err := c.RegisterGeneration(Registration{Name: "http"}, gen); !errors.Is(err, ErrBudgetInversion) {
		t.Fatalf("RegisterGeneration = %v, want ErrBudgetInversion", err)
	}

	base := greedyBaseline{fakeBaseline: newFakeBaseline("proc"), want: time.Hour}
	if err := c.RegisterBaseline(Registration{Name: "proc"}, base); !errors.Is(err, ErrBudgetInversion) {
		t.Fatalf("RegisterBaseline = %v, want ErrBudgetInversion", err)
	}

	fitting := greedyGeneration{fakeGeneration: newFakeGeneration("http"), want: time.Millisecond}
	if err := c.RegisterGeneration(Registration{Name: "http"}, fitting); err != nil {
		t.Fatalf("a collector inside its budget must register: %v", err)
	}
}

func TestRegisterRejectsDuplicatesAndEmptyNames(t *testing.T) {
	c, _ := newTestController(t, nil)

	if err := c.RegisterGeneration(Registration{Name: ""}, newFakeGeneration("x")); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("empty name = %v, want ErrInvalidRegistration", err)
	}
	if err := c.RegisterGeneration(Registration{Name: "http"}, nil); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("nil collector = %v, want ErrInvalidRegistration", err)
	}
	if err := c.RegisterBaseline(Registration{Name: "proc"}, nil); !errors.Is(err, ErrInvalidRegistration) {
		t.Fatalf("nil baseline = %v, want ErrInvalidRegistration", err)
	}

	if err := c.RegisterGeneration(Registration{Name: "http"}, newFakeGeneration("http")); err != nil {
		t.Fatalf("RegisterGeneration: %v", err)
	}
	if err := c.RegisterGeneration(Registration{Name: "http"}, newFakeGeneration("http")); !errors.Is(err, ErrCollectorRegistered) {
		t.Fatalf("duplicate generation = %v, want ErrCollectorRegistered", err)
	}
	if err := c.RegisterBaseline(Registration{Name: "proc"}, newFakeBaseline("proc")); err != nil {
		t.Fatalf("RegisterBaseline: %v", err)
	}
	if err := c.RegisterBaseline(Registration{Name: "proc"}, newFakeBaseline("proc")); !errors.Is(err, ErrCollectorRegistered) {
		t.Fatalf("duplicate baseline = %v, want ErrCollectorRegistered", err)
	}
}
