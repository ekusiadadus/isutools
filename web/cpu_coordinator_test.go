package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordingCPUCoordinator struct {
	mu        sync.Mutex
	starts    []CPUStartRequest
	fixed     []FixedCPUStartRequest
	stops     []CPUStopRequest
	startHook func()
	manifest  *CPUIntervalCapture
}

func (c *recordingCPUCoordinator) StartRun(_ context.Context, req CPUStartRequest) CPUStartResult {
	if c.startHook != nil {
		c.startHook()
	}
	c.mu.Lock()
	c.starts = append(c.starts, req)
	c.mu.Unlock()
	return CPUStartResult{RunID: req.RunID, Epoch: req.Epoch, CaptureID: "capture", State: "capturing"}
}

func (c *recordingCPUCoordinator) StartFixed(_ context.Context, req FixedCPUStartRequest) CPUStartResult {
	c.mu.Lock()
	c.fixed = append(c.fixed, req)
	c.mu.Unlock()
	return CPUStartResult{RunID: req.RunID, Epoch: req.Epoch, CaptureID: "capture", State: "capturing"}
}

func (c *recordingCPUCoordinator) RequestStop(req CPUStopRequest) CPUStopTicket {
	c.mu.Lock()
	c.stops = append(c.stops, req)
	c.mu.Unlock()
	return CPUStopTicket{RunID: req.RunID, Epoch: req.Epoch, CaptureID: "capture", BoundaryAt: req.BoundaryAt}
}

func (c *recordingCPUCoordinator) Await(ticket CPUStopTicket, _ context.Context) CPUCaptureStatus {
	return CPUCaptureStatus{RunID: ticket.RunID, Epoch: ticket.Epoch, CaptureID: ticket.CaptureID, State: "published"}
}

func (c *recordingCPUCoordinator) Manifest(string, uint64) *CPUIntervalCapture { return c.manifest }
func (c *recordingCPUCoordinator) LabelDictionary(string, uint64) *CPULabelDictionary {
	return nil
}

func TestResetStartsRunCPUAfterCumulativeOpenCapture(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cpu := &recordingCPUCoordinator{}
	startRef := time.Now().Add(-5 * time.Millisecond)
	cpu.startHook = func() {
		matches, err := filepath.Glob(filepath.Join(dir, "*_heap_open.pprof"))
		if err != nil || len(matches) != 1 {
			t.Errorf("CPU Start observed cumulative open files %v (err=%v), want exactly one", matches, err)
		}
	}
	h := NewHandler(Provider{
		DataDir: dir, RuntimeProfiles: []string{"heap"},
		CPUProfiles: cpu, CPUProfileMode: "run",
		StartRun: func(context.Context) (RunStart, error) {
			return RunStart{RunID: "run-1", Epoch: 9, State: "started", Validity: "valid", StartedAt: time.Now(),
				GenerationWindow: BoundaryWindow{Max: startRef}}, nil
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(cpu.starts) != 1 || cpu.starts[0].RunID != "run-1" || cpu.starts[0].Epoch != 9 || cpu.starts[0].State != "started" {
		t.Fatalf("CPU starts = %#v", cpu.starts)
	}
	if cpu.starts[0].BoundaryStart != startRef {
		t.Fatalf("CPU start boundary = %v, want generation max %v", cpu.starts[0].BoundaryStart, startRef)
	}
}

func TestFinishRequestsCPUStopBeforeCumulativeCloseCapture(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cpu := &recordingCPUCoordinator{}
	finishRef := time.Now().Add(time.Second)
	h := NewHandler(Provider{
		DataDir: dir, RuntimeProfiles: []string{"heap"},
		CPUProfiles: cpu, CPUProfileMode: "run",
		StartRun: func(context.Context) (RunStart, error) {
			return RunStart{RunID: "run-1", Epoch: 9, State: "started", Validity: "valid", StartedAt: time.Now()}, nil
		},
		FinishRun: func(context.Context) (RunFinish, error) {
			return RunFinish{RunID: "run-1", Epoch: 9, Validity: "valid", AcceptedAt: finishRef.Add(5 * time.Millisecond),
				GenerationWindow: BoundaryWindow{Max: finishRef}}, nil
		},
	})
	reset := httptest.NewRecorder()
	h.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/reset", nil))
	finish := httptest.NewRecorder()
	h.ServeHTTP(finish, httptest.NewRequest(http.MethodPost, "/finish", nil))
	if finish.Code != http.StatusAccepted {
		t.Fatalf("finish status = %d, body=%s", finish.Code, finish.Body.String())
	}
	if len(cpu.stops) != 1 {
		t.Fatalf("CPU stops = %#v", cpu.stops)
	}
	stop := cpu.stops[0]
	if stop.RunID != "run-1" || stop.Epoch != 9 || stop.State != "finishing" || stop.Reason != "finish-accepted" {
		t.Fatalf("stop = %#v", stop)
	}
	if stop.BoundaryAt != finishRef {
		t.Fatalf("stop boundary = %v, want generation max %v", stop.BoundaryAt, finishRef)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*_heap_close.pprof"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("close captures = %v err=%v", matches, err)
	}
}

func TestManualCPUProfileConflictsOnlyWithManagedRunMode(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/pprof/profile", nil)
	NewHandler(Provider{CPUProfiles: &recordingCPUCoordinator{}, CPUProfileMode: "run"}).ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestCPUOnlyRunStillProducesProfileManifest(t *testing.T) {
	t.Parallel()

	cpu := &recordingCPUCoordinator{manifest: &CPUIntervalCapture{
		RunID: "run-1", Epoch: 9, CaptureID: "capture", File: "cpu.pprof", SHA256: "hash", Status: "published",
	}}
	h := newHandler(Provider{DataDir: t.TempDir(), CPUProfiles: cpu, CPUProfileMode: "run"})
	manifest := h.profileManifestFor("run-1", 9)
	if manifest == nil || manifest.CPU == nil || manifest.CPU.CaptureID != "capture" || manifest.RunID != "run-1" || manifest.Epoch != 9 {
		t.Fatalf("manifest = %#v", manifest)
	}
}
