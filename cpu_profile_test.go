package isutools

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/profilecapture"
	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/web"
)

type fakeCPUOwner struct {
	mu     sync.Mutex
	starts []profilecapture.StartRequest
	stops  []profilecapture.StopRequest
	status profilecapture.Status
	found  bool
}

type fakeFixedCPUProfiler struct {
	mu          sync.Mutex
	generations []int64
	start       bool
}

func (f *fakeFixedCPUProfiler) Capture(generation int64) bool {
	f.mu.Lock()
	f.generations = append(f.generations, generation)
	f.mu.Unlock()
	return f.start
}

func (f *fakeCPUOwner) StartRun(_ context.Context, req profilecapture.StartRequest) profilecapture.StartResult {
	f.mu.Lock()
	f.starts = append(f.starts, req)
	f.mu.Unlock()
	return profilecapture.StartResult{RunID: req.RunID, Epoch: req.Epoch, CaptureID: "capture", State: profilecapture.StateCapturing}
}
func (f *fakeCPUOwner) RequestStop(req profilecapture.StopRequest) profilecapture.StopTicket {
	f.mu.Lock()
	f.stops = append(f.stops, req)
	f.mu.Unlock()
	return profilecapture.StopTicket{RunID: req.RunID, Epoch: req.Epoch, CaptureID: "capture", BoundaryAt: req.BoundaryAt}
}
func (f *fakeCPUOwner) Await(profilecapture.StopTicket, context.Context) profilecapture.Status {
	return profilecapture.Status{State: profilecapture.StatePublished}
}
func (f *fakeCPUOwner) Status(string, uint64) (profilecapture.Status, bool) {
	return f.status, f.found
}

func TestCPUWebBridgePreservesCaptureCoverage(t *testing.T) {
	t.Parallel()

	boundary := time.Date(2026, 8, 6, 4, 0, 0, 0, time.UTC)
	owner := &fakeCPUOwner{found: true, status: profilecapture.Status{
		RunID: "run-1", Epoch: 7, CaptureID: "capture", State: profilecapture.StatePublished,
		Artifact:      profilecapture.PublishedArtifact{File: "cpu_capture.pprof", SHA256: "sum", Bytes: 42},
		BoundaryStart: boundary, BoundaryFinish: boundary.Add(time.Second),
		StartRequestedAt: boundary, StartCompletedAt: boundary.Add(10 * time.Millisecond),
		StopRequestedAt: boundary.Add(time.Second), StopCompletedAt: boundary.Add(1100 * time.Millisecond),
		StopReason: "finish-accepted", RunSpan: time.Second, CaptureSpan: 1090 * time.Millisecond,
		HeadLoss: 10 * time.Millisecond, TailExcess: 100 * time.Millisecond, Complete: true,
		Sidecar: profilecapture.CompletionAttachment{
			Phase: profilecapture.CompletionPhaseInitial, File: "cpu_capture.meta.json", SHA256: "sidecar-sum", Bytes: 123, Visible: true, Durability: "durable",
		},
		Coverage: profilecapture.CompletionAttachment{
			Phase: profilecapture.CompletionPhaseCoverage, Sequence: 2, File: "cpu_capture.coverage.json", SHA256: "coverage-sum", Bytes: 124, Visible: true, Durability: "durable",
		},
	}}
	manifest := (&cpuWebBridge{owner: owner}).Manifest("run-1", 7)
	if manifest == nil || manifest.BoundaryStart != boundary || manifest.BoundaryFinish != boundary.Add(time.Second) ||
		manifest.StartRequestedAt != boundary || manifest.StartCompletedAt != boundary.Add(10*time.Millisecond) ||
		manifest.StopRequestedAt != boundary.Add(time.Second) || manifest.StopCompletedAt != boundary.Add(1100*time.Millisecond) ||
		manifest.StopReason != "finish-accepted" || manifest.RunSpanNs != int64(time.Second) ||
		manifest.CaptureSpanNs != int64(1090*time.Millisecond) || manifest.HeadLossNs != int64(10*time.Millisecond) ||
		manifest.TailExcessNs != int64(100*time.Millisecond) || manifest.TailLossNs != 0 || !manifest.Complete ||
		manifest.Sidecar != "cpu_capture.meta.json" || manifest.SidecarSHA256 != "sidecar-sum" ||
		manifest.CoverageFile != "cpu_capture.coverage.json" || manifest.CoverageSHA256 != "coverage-sum" {
		t.Fatalf("manifest = %#v", manifest)
	}
}
func (f *fakeCPUOwner) ActiveLabelScope() *profilecapture.LabelScope { return nil }
func (f *fakeCPUOwner) LabelDictionary(string, uint64) (profilecapture.LabelDictionary, bool) {
	return profilecapture.LabelDictionary{}, false
}
func (f *fakeCPUOwner) Close() {}

func TestCPUWebBridgeAndRunObserverShareOneOwner(t *testing.T) {
	t.Parallel()

	owner := &fakeCPUOwner{}
	bridge := &cpuWebBridge{owner: owner}
	observer := cpuRunObserver{owner: owner}
	boundary := time.Date(2026, 8, 6, 4, 0, 0, 0, time.UTC)
	bridge.StartRun(context.Background(), web.CPUStartRequest{
		RunID: "run-1", Epoch: 7, State: "started", Validity: "valid", BoundaryStart: boundary,
	})
	observer.OnRunTermination(runctl.RunTerminationEvent{
		RunID: "run-1", Epoch: 7, State: runctl.StateFinishing,
		Validity: runctl.ValidityValid, Reason: runctl.ReasonFinishAccepted, BoundaryAt: boundary.Add(time.Second),
	})
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if len(owner.starts) != 1 || len(owner.stops) != 1 {
		t.Fatalf("owner calls: starts=%#v stops=%#v", owner.starts, owner.stops)
	}
	if owner.starts[0].RunID != owner.stops[0].RunID || owner.starts[0].Epoch != owner.stops[0].Epoch {
		t.Fatalf("owner identity diverged: start=%#v stop=%#v", owner.starts[0], owner.stops[0])
	}
}

func TestMeasurementOffDoesNotConstructManagedCPUOwner(t *testing.T) {
	t.Parallel()

	m := newMeasurementWith(envMap(map[string]string{
		"ISUTOOLS":        "off",
		envCPUProfileMode: "run",
		envDataDir:        t.TempDir(),
	}), runctl.Options{DisableWatchdog: true}, isolatedGenerationCollectors())
	if m.ctrl != nil || m.proc != nil || m.timeline != nil || m.boundary != nil ||
		m.cpu != nil || m.cpuBridge != nil || m.cpuRoot != nil || m.cpuMode != "" {
		t.Fatalf("off measurement constructed CPU owner: %#v", m)
	}
}

func TestRunCPUHardMaxParsing(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{want: defaultRunCPUHardMax},
		{value: "1", want: time.Second},
		{value: "600", want: maxRunCPUHardMax},
		{value: "0", wantErr: true},
		{value: "601", wantErr: true},
		{value: "secret", wantErr: true},
	} {
		got, err := runCPUHardMax(envMap(map[string]string{"ISUTOOLS_PPROF_SECONDS": tt.value}))
		if tt.wantErr != (err != nil) || (!tt.wantErr && got != tt.want) {
			t.Errorf("value %q: got=%v err=%v, want=%v err=%v", tt.value, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestFixedCPUProfileModeConstructsSharedTimerBridge(t *testing.T) {
	t.Parallel()

	owner, bridge, root, mode := newRunCPUCoordinator(envMap(map[string]string{
		envCPUProfileMode:        "fixed",
		envDataDir:               t.TempDir(),
		"ISUTOOLS_PPROF_SECONDS": "1",
	}))
	if root != nil {
		defer func() { _ = root.Close() }()
	}
	if owner != nil || bridge == nil || bridge.fixed == nil || root == nil || mode != "fixed" {
		t.Fatalf("fixed mode owner = (%#v, %#v, %#v, %q)", owner, bridge, root, mode)
	}
}

func TestFixedCPUBridgeGatesRunIdentityBeforeStartingTimer(t *testing.T) {
	t.Parallel()
	profiler := &fakeFixedCPUProfiler{start: true}
	bridge := &cpuWebBridge{fixed: profiler}
	invalid := web.FixedCPUStartRequest{RunID: "run-1", Epoch: 1, State: "aborted", Validity: "invalid", Generation: 4, Duration: time.Second, RequestedAt: time.Now()}
	if result := bridge.StartFixed(context.Background(), invalid); result.State != "skipped" || result.Code != "invalid-fixed-request" {
		t.Fatalf("invalid fixed start = %#v", result)
	}
	valid := invalid
	valid.State, valid.Validity = "started", "partial"
	if result := bridge.StartFixed(context.Background(), valid); result.State != "capturing" || result.Code != "" {
		t.Fatalf("valid fixed start = %#v", result)
	}
	profiler.mu.Lock()
	defer profiler.mu.Unlock()
	if len(profiler.generations) != 1 || profiler.generations[0] != 4 {
		t.Fatalf("fixed generations = %v", profiler.generations)
	}
}

func TestResetNowUsesSharedFixedCPUBridge(t *testing.T) {
	profiler := &fakeFixedCPUProfiler{start: true}
	core := newTestMeasurement(t, nil)
	core.cpuBridge = &cpuWebBridge{fixed: profiler}
	core.cpuMode = "fixed"
	core.cpuDuration = time.Second
	core.generation = func() int64 { return 11 }
	result, err := core.resetNow(context.Background(), runctl.StartRunOptions{Preempt: true, Reason: "test", Trigger: "test"})
	if err != nil || result.State != runctl.StateStarted {
		t.Fatalf("ResetNow = %#v err=%v", result, err)
	}
	profiler.mu.Lock()
	defer profiler.mu.Unlock()
	if len(profiler.generations) != 1 || profiler.generations[0] != 11 {
		t.Fatalf("fixed generations = %v", profiler.generations)
	}
}
