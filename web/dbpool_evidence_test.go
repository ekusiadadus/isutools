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
