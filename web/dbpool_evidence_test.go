package web

import (
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/procstats"
)

func TestNegligiblePoolWaitDoesNotOverrideCPU(t *testing.T) {
	s := Snapshot{DBPool: []dbpool.Entry{{WaitCount: 1, WaitDuration: time.Microsecond}}, Proc: &procstats.Snapshot{CPUTotal: &procstats.CPUTotal{BusyPercent: 99}}}
	if got := diagnoseBottleneck(s); got.PrimaryAnchor != "profiles" {
		t.Fatalf("diagnosis: %+v", got)
	}
	got, _ := dbPoolSignal(s)
	if got.Level == "hot" || strings.Contains(got.NextAction, "決めています") {
		t.Fatalf("overstated diagnosis: %+v", got)
	}
}

func TestPoolBoundaryWaitAndInvalidCounters(t *testing.T) {
	for _, entry := range []dbpool.Entry{{WaitDuration: time.Second}, {WaitCount: 1}} {
		got, _ := dbPoolSignal(Snapshot{DBPool: []dbpool.Entry{entry}})
		if got.Level != "warn" || poolWaitRatio(entry) != "—" || strings.Contains(got.NextAction, "wait はありません") {
			t.Fatalf("boundary: %+v", got)
		}
	}
	got, _ := dbPoolSignal(Snapshot{DBPool: []dbpool.Entry{
		{TargetID: "bad", Partial: true, Code: dbpool.CodeCounterRewind, WaitCount: 999999, WaitDuration: time.Hour},
		{TargetID: "code-only", Code: dbpool.CodeCounterRewind, WaitCount: 999999},
		{TargetID: "good", WaitCount: 2, WaitDuration: time.Millisecond},
	}})
	if strings.Contains(got.Evidence, "999999") || !strings.Contains(got.Evidence, "excluded 2") || !strings.Contains(got.Evidence, "wait starts 2") {
		t.Fatal(got)
	}
	onlyPartial, _ := dbPoolSignal(Snapshot{DBPool: []dbpool.Entry{{Partial: true, WaitCount: 100}}})
	if !strings.Contains(onlyPartial.NextAction, "有効な区間データがありません") {
		t.Fatal(onlyPartial)
	}
}

func TestPoolCapacityRemainsPerTarget(t *testing.T) {
	got, _ := dbPoolSignal(Snapshot{DBPool: []dbpool.Entry{{TargetID: "limited", MaxOpen: 4, Open: 3}, {TargetID: "unbounded", Open: 10}}})
	for _, want := range []string{"limited open 3 / max 4", "unbounded open 10 / max unlimited"} {
		if !strings.Contains(got.Evidence, want) {
			t.Fatal(got)
		}
	}
	if strings.Contains(got.Evidence, "open 13 / max 4") {
		t.Fatal(got)
	}
}

func TestPoolRatioDisplayQualifiesEstimate(t *testing.T) {
	if got := poolWaitRatio(dbpool.Entry{WaitCount: 2, WaitDuration: time.Second}); !strings.Contains(got, "参考") {
		t.Fatal(got)
	}
	if got := poolWaitRatio(dbpool.Entry{WaitCount: 2, WaitDuration: time.Second, Partial: true}); got != "—" {
		t.Fatal(got)
	}
	body := renderReport(t, Snapshot{DBPool: []dbpool.Entry{{TargetID: "cross-boundary", WaitDuration: time.Second}}})
	if !strings.Contains(body, "厳密な平均 waitではありません") || strings.Contains(body, "pool wait はありません") {
		t.Fatal("misleading wait presentation")
	}
}

func TestPoolClosureChurnWithoutWaitsIsVisiblePerTarget(t *testing.T) {
	start := time.Unix(100, 0)
	s := Snapshot{DBPool: []dbpool.Entry{
		{TargetID: "app", BaselineAt: start, FinalAt: start.Add(time.Minute), MaxIdleClosed: 4476},
		{TargetID: "low", BaselineAt: start, FinalAt: start.Add(time.Minute), MaxIdleClosed: 18},
		{TargetID: "partial", BaselineAt: start, FinalAt: start.Add(time.Minute), Partial: true, MaxIdleClosed: 9000},
	}}
	got, ok := dbPoolSignal(s)
	if !ok || got.Level != "warn" || !strings.Contains(got.Evidence, "app max-idle 4476") ||
		!strings.Contains(got.Evidence, "74.6/s") || !strings.Contains(got.Evidence, "wait starts 0") ||
		strings.Contains(got.Evidence, "low max-idle") || strings.Contains(got.Evidence, "partial max-idle") ||
		!strings.Contains(got.NextAction, "1条件ずつ") || !strings.Contains(got.NextAction, "証明できず") {
		t.Fatalf("churn signal = %+v", got)
	}
	if body := renderReport(t, s); !strings.Contains(body, "app max-idle 4476") {
		t.Fatal("report did not render the churn candidate")
	}
}

func TestPoolClosureChurnRequiresCountAndIntervalRate(t *testing.T) {
	start := time.Unix(100, 0)
	for _, entry := range []dbpool.Entry{
		{MaxIdleClosed: 5000}, // no interval
		{BaselineAt: start, FinalAt: start.Add(time.Minute), MaxIdleClosed: 99},
		{BaselineAt: start, FinalAt: start.Add(24 * time.Hour), MaxIdleClosed: 100},
		{BaselineAt: start, FinalAt: start.Add(time.Minute), Code: dbpool.CodeCounterRewind, MaxIdleClosed: 5000},
	} {
		got, _ := dbPoolSignal(Snapshot{DBPool: []dbpool.Entry{entry}})
		if strings.Contains(got.Evidence, "closure candidates") {
			t.Fatalf("unexpected churn candidate: %+v", got)
		}
	}
	got, _ := dbPoolSignal(Snapshot{DBPool: []dbpool.Entry{{
		TargetID: "ttl", BaselineAt: start, FinalAt: start.Add(time.Minute),
		MaxIdleTimeClosed: 30, MaxLifetimeClosed: 70,
	}}})
	if got.Level != "warn" || !strings.Contains(got.Evidence, "ttl max-idle 0 / idle-time 30 / lifetime 70") {
		t.Fatalf("closure reasons = %+v", got)
	}
}
