package timeline

import (
	"math"
	"testing"
	"time"
)

func TestCollectorPersistsAlignedBoundedSignalBuckets(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 6, 12, 0, 0, 250_000_000, time.UTC)
	collector, err := New(Config{Interval: time.Second, MaxBuckets: 3, MaxOperations: 4})
	if err != nil {
		t.Fatal(err)
	}
	collector.Start(RunStart{RunID: "run-1", Epoch: 7, At: start})
	httpToken := collector.HTTPStart(start.Add(100 * time.Millisecond))
	collector.HTTPFinish(start.Add(800*time.Millisecond), httpToken, "GET /poll", 700*time.Millisecond, false)
	collector.ObserveSQL(start.Add(900*time.Millisecond), "SELECT ?", 20*time.Millisecond, false)
	collector.Sample(ResourceSample{
		At:      start.Add(time.Second),
		Pools:   []PoolPoint{{TargetID: "main", MaxOpen: 10, Open: 4, InUse: 3, WaitCount: 10, WaitDuration: time.Second}},
		Process: &ProcessPoint{TotalJiffies: 1_000, BusyJiffies: 400, IOWaitJiffies: 20, ProcessJiffies: 100, CPUs: 4, RSSBytes: 1000},
		Host:    &HostPoint{ReadBytes: 1_000, WriteBytes: 2_000, IOTicks: 100 * time.Millisecond, WeightedIO: 200 * time.Millisecond},
	})
	collector.Sample(ResourceSample{
		At:      start.Add(2 * time.Second),
		Pools:   []PoolPoint{{TargetID: "main", MaxOpen: 10, Open: 10, InUse: 9, WaitCount: 14, WaitDuration: 3 * time.Second}},
		Process: &ProcessPoint{TotalJiffies: 1_400, BusyJiffies: 760, IOWaitJiffies: 60, ProcessJiffies: 180, CPUs: 4, RSSBytes: 1200},
		Host:    &HostPoint{ReadBytes: 5_000, WriteBytes: 8_000, IOTicks: 500 * time.Millisecond, WeightedIO: 900 * time.Millisecond},
	})
	// The fourth interval is folded into the final bounded bucket.
	collector.ObserveSQL(start.Add(4*time.Second), "SELECT late", time.Millisecond, true)
	collector.Terminate(RunTermination{RunID: "run-1", Epoch: 7, At: start.Add(4 * time.Second), Reason: "finish-accepted", Validity: "valid"})

	section, ok := collector.Section("run-1", 7)
	if !ok {
		t.Fatal("timeline section was not retained")
	}
	if section.Schema != SchemaV1 || section.IntervalNs != int64(time.Second) || len(section.Buckets) != 3 || !section.Truncated {
		t.Fatalf("section bounds = %#v", section)
	}
	for i, bucket := range section.Buckets {
		wantStart := start.Add(time.Duration(i) * time.Second)
		if !bucket.Start.Equal(wantStart) || !bucket.End.Equal(wantStart.Add(time.Second)) {
			t.Fatalf("bucket %d = %s..%s", i, bucket.Start, bucket.End)
		}
	}
	first := section.Buckets[0]
	if first.HTTPInFlightMax != 1 || len(first.HTTP) != 1 || first.HTTP[0].Key != "GET /poll" || first.HTTP[0].P95Ns <= 0 || len(first.SQL) != 1 {
		t.Fatalf("first bucket = %#v", first)
	}
	second := section.Buckets[1]
	if len(second.DBPools) != 1 || second.DBPools[0].WaitCount != 4 || second.DBPools[0].WaitDurationNs != int64(2*time.Second) ||
		second.Process == nil || second.Process.BusyPercent != 90 || second.Process.IOWaitPercent != 10 ||
		second.Host == nil || second.Host.ReadBytes != 4_000 || second.Host.WriteBytes != 6_000 {
		t.Fatalf("resource delta = %#v", second)
	}
}

func TestAnalyzeFindsMigrationAndLowVolumeGateWithoutClaimingCausality(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	section := Section{Schema: SchemaV1, RunID: "run-2", Epoch: 8, IntervalNs: int64(time.Second), MaxBuckets: 8, Buckets: []Bucket{
		{Index: 0, Start: start, End: start.Add(time.Second), HTTP: []Operation{
			{Key: "GET /poll", Count: 100, Successes: 100, P95Ns: int64(10 * time.Millisecond)},
			{Key: "POST /gate", Count: 2, Successes: 2, P95Ns: int64(120 * time.Millisecond)},
		}},
		{Index: 1, Start: start.Add(time.Second), End: start.Add(2 * time.Second), HTTP: []Operation{
			{Key: "GET /poll", Count: 150, Successes: 150, P95Ns: int64(12 * time.Millisecond)},
			{Key: "POST /gate", Count: 2, Successes: 2, P95Ns: int64(240 * time.Millisecond)},
		}, DBPools: []PoolBucket{{TargetID: "main", MaxOpen: 100, InUse: 95}}},
		{Index: 2, Start: start.Add(2 * time.Second), End: start.Add(3 * time.Second), HTTP: []Operation{
			{Key: "GET /poll", Count: 180, Successes: 170, Errors: 10, P95Ns: int64(20 * time.Millisecond)},
			{Key: "POST /gate", Count: 1, Successes: 1, P95Ns: int64(220 * time.Millisecond)},
		}, Process: &ProcessBucket{BusyPercent: 95}},
		{Index: 3, Start: start.Add(3 * time.Second), End: start.Add(4 * time.Second), HTTP: []Operation{
			{Key: "GET /poll", Count: 160, Successes: 160, P95Ns: int64(15 * time.Millisecond)},
			{Key: "POST /gate", Count: 2, Successes: 2, P95Ns: int64(130 * time.Millisecond)},
		}, Process: &ProcessBucket{BusyPercent: 96}},
	}}
	section.Analysis = Analyze(section)

	for _, kind := range []string{PhaseTrafficGrowth, PhaseSaturation, PhaseErrorOnset, PhaseBottleneckMigration} {
		if !hasPhase(section.Analysis.Phases, kind) {
			t.Errorf("missing phase %q: %#v", kind, section.Analysis.Phases)
		}
	}
	if len(section.Analysis.Suspects) == 0 || section.Analysis.Suspects[0].Key != "POST /gate" || section.Analysis.Suspects[0].Label != "correlation-suspect" {
		t.Fatalf("suspects = %#v", section.Analysis.Suspects)
	}
	for _, suspect := range section.Analysis.Suspects {
		if len(suspect.Evidence) == 0 {
			t.Fatalf("suspect has no evidence: %#v", suspect)
		}
		for _, evidence := range suspect.Evidence {
			if evidence.Signal == "" || evidence.Metric == "" || evidence.Formula == "" || evidence.Limitation == "" || evidence.WindowStart.IsZero() || evidence.WindowEnd.IsZero() {
				t.Fatalf("incomplete evidence = %#v", evidence)
			}
		}
	}
}

func TestAnalyzeFindsPollingSymptomWithoutErrorOnset(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	section := Section{Buckets: []Bucket{
		{Index: 0, Start: start, End: start.Add(time.Second), HTTP: []Operation{
			{Key: "GET /poll", Count: 100, Successes: 100},
			{Key: "POST /work", Count: 10, Successes: 10},
		}},
		{Index: 1, Start: start.Add(time.Second), End: start.Add(2 * time.Second), HTTP: []Operation{
			{Key: "GET /poll", Count: 130, Successes: 130},
			{Key: "POST /work", Count: 10, Successes: 10},
		}},
	}}
	analysis := Analyze(section)
	if !analysis.Available {
		t.Fatalf("analysis unavailable: %#v", analysis)
	}
	found := false
	for _, suspect := range analysis.Suspects {
		if suspect.Key == "GET /poll" && suspect.Signal == "backpressure/polling symptom candidate" {
			found = true
			if suspect.Label != "correlation-suspect" || len(suspect.Evidence) != 1 || suspect.Evidence[0].Formula == "" || suspect.Evidence[0].Limitation == "" {
				t.Fatalf("untraceable polling suspect: %#v", suspect)
			}
		}
	}
	if !found {
		t.Fatalf("polling symptom was hidden without errors: %#v", analysis.Suspects)
	}
}

func TestAnalyzeKeepsAggregateFallbackWhenEvidenceIsInsufficient(t *testing.T) {
	t.Parallel()
	start := time.Now()
	section := Section{Schema: SchemaV1, RunID: "run-short", Epoch: 1, IntervalNs: int64(time.Second), Buckets: []Bucket{{Index: 0, Start: start, End: start.Add(time.Second)}}}
	analysis := Analyze(section)
	if analysis.Available || analysis.Reason != ReasonInsufficientBuckets || len(analysis.Phases) != 0 || len(analysis.Suspects) != 0 {
		t.Fatalf("analysis = %#v", analysis)
	}
}

func TestEventsAreFencedToRunAtStart(t *testing.T) {
	t.Parallel()
	start := time.Now().UTC()
	collector, err := New(Config{Interval: time.Second, MaxBuckets: 4, MaxOperations: 4})
	if err != nil {
		t.Fatal(err)
	}
	collector.Start(RunStart{RunID: "run-a", Epoch: 1, At: start})
	httpA := collector.HTTPStart(start.Add(100 * time.Millisecond))
	sqlA := collector.SQLStart(start.Add(100 * time.Millisecond))
	collector.Terminate(RunTermination{RunID: "run-a", Epoch: 1, At: start.Add(time.Second), Reason: "preempted", Validity: "invalid"})

	startB := start.Add(time.Second)
	collector.Start(RunStart{RunID: "run-b", Epoch: 2, At: startB})
	collector.HTTPFinish(startB.Add(100*time.Millisecond), httpA, "GET /old", time.Second, false)
	collector.SQLFinish(startB.Add(100*time.Millisecond), sqlA, "SELECT old", time.Second, false)
	httpB := collector.HTTPStart(startB.Add(200 * time.Millisecond))
	collector.HTTPFinish(startB.Add(300*time.Millisecond), httpB, "GET /new", 100*time.Millisecond, false)
	collector.Terminate(RunTermination{RunID: "run-b", Epoch: 2, At: startB.Add(time.Second), Reason: "finish-accepted", Validity: "valid"})

	section, ok := collector.Section("run-b", 2)
	if !ok || len(section.Buckets) != 1 || len(section.Buckets[0].HTTP) != 1 ||
		section.Buckets[0].HTTP[0].Key != "GET /new" || len(section.Buckets[0].SQL) != 0 ||
		section.Buckets[0].HTTPInFlightMax != 1 {
		t.Fatalf("cross-run event escaped epoch fence: %#v", section)
	}
}

func TestStartAndTerminationAreIdempotentAcrossDeliveryOrder(t *testing.T) {
	t.Parallel()
	start := time.Now().UTC()
	collector, err := New(Config{Interval: time.Second, MaxBuckets: 4, MaxOperations: 4})
	if err != nil {
		t.Fatal(err)
	}
	terminal := RunTermination{RunID: "run", Epoch: 9, At: start.Add(time.Second), Reason: "required-failed", Validity: "invalid"}
	collector.Terminate(terminal)
	if collector.Start(RunStart{RunID: "run", Epoch: 9, At: start}) {
		t.Fatal("terminal-before-start must not authorize a sampler")
	}
	collector.Terminate(terminal)
	if collector.Start(RunStart{RunID: "run", Epoch: 9, At: start}) {
		t.Fatal("completed run replay must be rejected")
	}
	section, ok := collector.Section("run", 9)
	if !ok || section.StopReason != "required-failed" || section.Validity != "invalid" {
		t.Fatalf("completed section = %#v, %v", section, ok)
	}

	if !collector.Start(RunStart{RunID: "next", Epoch: 10, At: start.Add(2 * time.Second)}) {
		t.Fatal("new run was rejected")
	}
	if collector.Start(RunStart{RunID: "next", Epoch: 10, At: start.Add(2 * time.Second)}) {
		t.Fatal("active replay must not authorize a second sampler")
	}
}

func TestEventsBeforeRunBoundaryAndImpossibleProcessDeltasAreRejected(t *testing.T) {
	t.Parallel()
	start := time.Now().UTC()
	collector, err := New(Config{Interval: time.Second, MaxBuckets: 4, MaxOperations: 4})
	if err != nil {
		t.Fatal(err)
	}
	collector.Start(RunStart{RunID: "run", Epoch: 1, At: start})
	if token := collector.HTTPStart(start.Add(-time.Nanosecond)); token != nil {
		t.Fatalf("pre-boundary HTTP token = %#v", token)
	}
	if token := collector.SQLStart(start.Add(-time.Nanosecond)); token != nil {
		t.Fatalf("pre-boundary SQL token = %#v", token)
	}
	collector.Sample(ResourceSample{At: start, Process: &ProcessPoint{TotalJiffies: 100, BusyJiffies: 50, IOWaitJiffies: 10, CPUs: 4}})
	collector.Sample(ResourceSample{At: start.Add(time.Second), Process: &ProcessPoint{TotalJiffies: 110, BusyJiffies: 70, IOWaitJiffies: 11, CPUs: 4}})
	collector.Terminate(RunTermination{RunID: "run", Epoch: 1, At: start.Add(2 * time.Second), Reason: "finish-accepted", Validity: "valid"})
	section, _ := collector.Section("run", 1)
	if len(section.Buckets) == 0 || section.Buckets[0].HTTPInFlightMax != 0 {
		t.Fatalf("pre-boundary event altered section: %#v", section)
	}
	if len(section.Buckets) > 1 && section.Buckets[1].Process != nil {
		t.Fatalf("busy delta larger than total was published: %#v", section.Buckets[1].Process)
	}
}

func TestAnalyzeDoesNotOverflowHostileCounters(t *testing.T) {
	t.Parallel()
	start := time.Now().UTC()
	section := Section{Buckets: []Bucket{
		{Index: 0, Start: start, End: start.Add(time.Second), HTTP: []Operation{{Key: "GET /a", Count: int64(^uint64(0) >> 1), Successes: int64(^uint64(0) >> 1), P95Ns: int64(^uint64(0) >> 1)}}},
		{Index: 1, Start: start.Add(time.Second), End: start.Add(2 * time.Second), HTTP: []Operation{{Key: "GET /a", Count: int64(^uint64(0) >> 1), Successes: int64(^uint64(0) >> 1), Errors: 1, P95Ns: int64(^uint64(0) >> 1)}}},
	}}
	analysis := Analyze(section)
	if !analysis.Available || !hasPhase(analysis.Phases, PhaseErrorOnset) {
		t.Fatalf("analysis = %#v", analysis)
	}
}

func TestCollectorBoundsOverflowCancelAndLiveSection(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{Interval: time.Millisecond},
		{Interval: 2 * time.Minute},
		{MaxBuckets: 1},
		{MaxBuckets: maxConfigBuckets + 1},
		{MaxOperations: maxConfigOperations + 1},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%#v) succeeded", cfg)
		}
	}

	start := time.Now().UTC()
	collector, err := New(Config{Interval: time.Second, MaxBuckets: 3, MaxOperations: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !collector.Start(RunStart{RunID: "run-bounds", Epoch: 1, At: start}) {
		t.Fatal("start rejected")
	}
	first := collector.HTTPStart(start.Add(10 * time.Millisecond))
	second := collector.HTTPStart(start.Add(20 * time.Millisecond))
	collector.HTTPCancel(start.Add(30*time.Millisecond), first)
	collector.SQLCancel(start.Add(30*time.Millisecond), nil)
	collector.Tick(start.Add(time.Second + time.Millisecond))
	collector.HTTPFinish(start.Add(time.Second+10*time.Millisecond), second, "GET /first", time.Millisecond, false)
	collector.ObserveSQL(start.Add(time.Second+20*time.Millisecond), "SELECT first", time.Millisecond, false)
	collector.ObserveSQL(start.Add(time.Second+30*time.Millisecond), "SELECT second", time.Millisecond, false)

	live, ok := collector.Section("run-bounds", 1)
	if !ok || len(live.Buckets) != 2 || live.Buckets[1].HTTPInFlightMax != 1 {
		t.Fatalf("live section = %#v, %v", live, ok)
	}
	collector.Terminate(RunTermination{RunID: "run-bounds", Epoch: 1, At: start.Add(2 * time.Second), Reason: "finish-accepted", Validity: "valid"})
	section, _ := collector.Section("run-bounds", 1)
	if section.OverflowedEvents != 1 || len(section.Buckets[1].SQL) != 2 {
		t.Fatalf("bounded section = %#v", section)
	}
	foundOverflow := false
	for _, operation := range section.Buckets[1].SQL {
		foundOverflow = foundOverflow || operation.Key == overflowOperationKey
	}
	if !foundOverflow {
		t.Fatalf("overflow row missing: %#v", section.Buckets[1].SQL)
	}

	dropped := uint64(math.MaxUint64)
	incrementDropped(&dropped)
	if dropped != math.MaxUint64 {
		t.Fatalf("saturated dropped counter wrapped to %d", dropped)
	}
	if _, ok := collector.Section("missing", 99); ok {
		t.Fatal("missing section reported present")
	}
}

func hasPhase(phases []Phase, kind string) bool {
	for _, phase := range phases {
		if phase.Kind == kind {
			return true
		}
	}
	return false
}
