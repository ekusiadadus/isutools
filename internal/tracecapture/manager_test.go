package tracecapture

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/profileowner"
	"github.com/ekusiadadus/isutools/internal/safefs"
)

type fakeBackend struct {
	mu      sync.Mutex
	writer  io.Writer
	started int
	stopped int
	start   func(io.Writer) error
}

func (b *fakeBackend) Start(writer io.Writer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writer = writer
	b.started++
	if b.start != nil {
		return b.start(writer)
	}
	_, err := io.WriteString(writer, "trace-start\n")
	return err
}

func (b *fakeBackend) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped++
	_, _ = io.WriteString(b.writer, "trace-stop\n")
}

func newManager(t *testing.T, backend Backend, duration time.Duration, maxBytes int64) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := safefs.Open(dir, safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	manager, err := New(Options{Root: root, Backend: backend, Owner: &profileowner.Registry{}, Duration: duration, MaxBytes: maxBytes, ID: func() string { return strings.Repeat("a", 32) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager, dir
}

func TestManagerPublishesRunBoundTraceAndSidecar(t *testing.T) {
	backend := &fakeBackend{}
	manager, dir := newManager(t, backend, time.Second, 1024)
	boundary := time.Now().Add(-time.Millisecond)
	start := manager.Start(StartRequest{RunID: "run-1", Epoch: 1, State: "started", Validity: "valid", BoundaryAt: boundary})
	if start.State != StateCapturing || start.CaptureID == "" {
		t.Fatalf("start=%+v", start)
	}
	ticket := manager.RequestStop(StopRequest{RunID: "run-1", Epoch: 1, Reason: "finish-accepted", BoundaryAt: time.Now()})
	status := manager.Await(ticket, context.Background())
	if status.State != StatePublished || status.File == "" || status.SHA256 == "" || status.Bytes == 0 || status.Sidecar == "" || status.SidecarSHA256 == "" {
		t.Fatalf("status=%+v", status)
	}
	if status.HeadLoss < 0 || status.TailExcess < 0 || status.CaptureSpan <= 0 || !status.Complete {
		t.Fatalf("coverage=%+v", status)
	}
	for _, name := range []string{status.File, status.Sidecar} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("stat %s = (%v, %v)", name, info, err)
		}
	}
	replay := manager.Start(StartRequest{RunID: "run-1", Epoch: 1, State: "started", Validity: "valid", BoundaryAt: boundary})
	if replay.State != StateReplayed || replay.CaptureID != start.CaptureID || backend.started != 1 {
		t.Fatalf("replay=%+v starts=%d", replay, backend.started)
	}
	repeated := manager.RequestStop(StopRequest{RunID: "run-1", Epoch: 1, Reason: "repeated-save", BoundaryAt: time.Now()})
	if repeated.State != StatePublished || backend.stopped != 1 {
		t.Fatalf("repeated=%+v stops=%d", repeated, backend.stopped)
	}
}

func TestManagerDoesNotLeaveReadyRawTraceWhenSidecarPublicationFails(t *testing.T) {
	backend := &fakeBackend{}
	manager, directory := newManager(t, backend, time.Second, 1024)
	sidecar := filepath.Join(directory, "trace_"+strings.Repeat("a", 32)+".meta.json")
	if err := os.WriteFile(sidecar, []byte("conflicting sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := manager.Start(StartRequest{RunID: "run-sidecar", Epoch: 3, State: "started", Validity: "valid", BoundaryAt: time.Now()})
	status := manager.Await(manager.RequestStop(StopRequest{RunID: "run-sidecar", Epoch: 3, Reason: "finish-accepted", BoundaryAt: time.Now()}), context.Background())
	if started.State != StateCapturing || status.State != StateFailed || status.Code != CodePublishFailed || status.Complete || status.File != "" {
		t.Fatalf("start=%+v status=%+v", started, status)
	}
	if matches, _ := filepath.Glob(filepath.Join(directory, "*.out")); len(matches) != 0 {
		t.Fatalf("orphan ready trace=%v", matches)
	}
}

func TestManagerShutdownAbortsActiveCapture(t *testing.T) {
	manager, directory := newManager(t, &fakeBackend{}, time.Second, 1024)
	started := manager.Start(StartRequest{RunID: "run-shutdown", Epoch: 4, State: "started", Validity: "valid", BoundaryAt: time.Now()})
	manager.Close()
	status, ok := manager.Status("run-shutdown", 4)
	if started.State != StateCapturing || !ok || status.State != StateAborted || status.Complete {
		t.Fatalf("started=%+v status=%+v ok=%v", started, status, ok)
	}
	if matches, _ := filepath.Glob(filepath.Join(directory, "*.out")); len(matches) != 0 {
		t.Fatalf("shutdown raw trace=%v", matches)
	}
}

func TestManagerRejectsConcurrentOwnerAndAbortsWithoutRawArtifact(t *testing.T) {
	backend := &fakeBackend{}
	manager, dir := newManager(t, backend, time.Second, 1024)
	first := manager.Start(StartRequest{RunID: "run-1", Epoch: 1, State: "started", Validity: "valid", BoundaryAt: time.Now()})
	second := manager.Start(StartRequest{RunID: "run-2", Epoch: 2, State: "started", Validity: "valid", BoundaryAt: time.Now()})
	if first.State != StateCapturing || second.State != StateSkipped || second.Code != CodeProfilerBusy {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	ticket := manager.RequestStop(StopRequest{RunID: "run-1", Epoch: 1, Reason: "aborted", Abort: true, BoundaryAt: time.Now()})
	status := manager.Await(ticket, context.Background())
	if status.State != StateAborted || status.File != "" || status.Complete {
		t.Fatalf("status=%+v", status)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.out")); len(matches) != 0 {
		t.Fatalf("aborted raw traces=%v", matches)
	}
}

func TestManagerBoundsOutputAndHardDuration(t *testing.T) {
	tooLarge := &fakeBackend{start: func(writer io.Writer) error { _, err := io.WriteString(writer, strings.Repeat("x", 65)); return err }}
	manager, dir := newManager(t, tooLarge, time.Second, 64)
	result := manager.Start(StartRequest{RunID: "run-large", Epoch: 1, State: "started", Validity: "valid", BoundaryAt: time.Now()})
	if result.State != StateFailed || result.Code != CodeOutputTooLarge {
		t.Fatalf("result=%+v", result)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(matches) != 0 {
		t.Fatalf("temporary files=%v", matches)
	}

	timerBackend := &fakeBackend{}
	timerManager, _ := newManager(t, timerBackend, 20*time.Millisecond, 1024)
	started := timerManager.Start(StartRequest{RunID: "run-timer", Epoch: 2, State: "started", Validity: "valid", BoundaryAt: time.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status := timerManager.Await(StopTicket{RunID: "run-timer", Epoch: 2, CaptureID: started.CaptureID}, ctx)
	if status.State != StatePublished || status.StopReason != ReasonMaxDuration || status.Code != CodeDurationComplete {
		t.Fatalf("status=%+v", status)
	}
}

func TestManagerValidatesRunAndBudgets(t *testing.T) {
	root, err := safefs.Open(t.TempDir(), safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := New(Options{Root: root, Backend: &fakeBackend{}, Owner: &profileowner.Registry{}, Duration: time.Hour, MaxBytes: 1}); err == nil {
		t.Fatal("oversized duration accepted")
	}
	manager, _ := newManager(t, &fakeBackend{}, time.Second, 1024)
	result := manager.Start(StartRequest{RunID: "run-1", Epoch: 1, State: "aborted", Validity: "invalid"})
	if result.State != StateSkipped || result.Code != CodeRunInvalid {
		t.Fatalf("result=%+v", result)
	}
}
