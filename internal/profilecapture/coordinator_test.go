package profilecapture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeBackend struct {
	starts    atomic.Int64
	stops     atomic.Int64
	startErr  error
	startWait chan struct{}
	stopWait  chan struct{}
}

func (b *fakeBackend) StartCPUProfile(io.Writer) error {
	b.starts.Add(1)
	if b.startWait != nil {
		<-b.startWait
	}
	return b.startErr
}

func (b *fakeBackend) StopCPUProfile() {
	b.stops.Add(1)
	if b.stopWait != nil {
		<-b.stopWait
	}
}

type fakeArtifact struct {
	bytes.Buffer
	mu         sync.Mutex
	published  int
	aborted    int
	result     PublishedArtifact
	publishErr error
}

func (a *fakeArtifact) Writer() io.Writer { return &a.Buffer }
func (a *fakeArtifact) Publish() (PublishedArtifact, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.published++
	if a.result.File == "" {
		a.result = PublishedArtifact{File: "cpu.pprof", SHA256: "hash", Bytes: int64(a.Len()), Visible: true, Durability: "durable"}
	}
	return a.result, a.publishErr
}
func (a *fakeArtifact) Abort() error {
	a.mu.Lock()
	a.aborted++
	a.mu.Unlock()
	return nil
}

type fakeFactory struct {
	mu         sync.Mutex
	created    int
	artifacts  []*fakeArtifact
	result     PublishedArtifact
	publishErr error
}

func (f *fakeFactory) New(StartRequest, string) (Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &fakeArtifact{result: f.result, publishErr: f.publishErr}
	f.created++
	f.artifacts = append(f.artifacts, a)
	return a, nil
}

func validStartRequest() StartRequest {
	return StartRequest{
		RunID: "run-1", Epoch: 1, State: "started", Validity: "valid",
		BoundaryStart: time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
	}
}

func TestSessionCaptureIDsAreChronologicallySortableWithRandomTail(t *testing.T) {
	older := newSessionCaptureID(time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC))
	newer := newSessionCaptureID(time.Date(2026, 8, 6, 10, 0, 1, 0, time.UTC))
	if len(older) != 32 || len(newer) != 32 || strings.Compare(older, newer) >= 0 || older[:12] == newer[:12] {
		t.Fatalf("capture IDs are not time sortable: old=%s new=%s", older, newer)
	}
}

func newTestCoordinator(t *testing.T, backend *fakeBackend, factory *fakeFactory, mutate func(*Options)) *Coordinator {
	t.Helper()
	o := Options{
		Mode: ModeRun, Backend: backend, Artifacts: factory,
		StartJoin: 100 * time.Millisecond, StopStall: 2 * time.Second,
		HardMax: time.Minute,
	}
	if mutate != nil {
		mutate(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestStartRunRejectsInvalidDTOBeforeCreatingArtifact(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		edit func(*StartRequest)
		code string
	}{
		{name: "empty-run", edit: func(r *StartRequest) { r.RunID = "" }, code: CodeInvalidRequest},
		{name: "zero-epoch", edit: func(r *StartRequest) { r.Epoch = 0 }, code: CodeInvalidRequest},
		{name: "aborted", edit: func(r *StartRequest) { r.State = "aborted" }, code: CodeRunNotStarted},
		{name: "unknown-state", edit: func(r *StartRequest) { r.State = "" }, code: CodeRunNotStarted},
		{name: "invalid", edit: func(r *StartRequest) { r.Validity = "invalid" }, code: CodeRunInvalid},
		{name: "unknown-validity", edit: func(r *StartRequest) { r.Validity = "mystery" }, code: CodeRunInvalid},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backend, factory := &fakeBackend{}, &fakeFactory{}
			c := newTestCoordinator(t, backend, factory, nil)
			req := validStartRequest()
			tt.edit(&req)
			result := c.StartRun(context.Background(), req)
			if result.State != StateSkipped || result.Code != tt.code {
				t.Fatalf("StartRun = %#v, want skipped/%s", result, tt.code)
			}
			if backend.starts.Load() != 0 || factory.created != 0 {
				t.Fatalf("invalid request touched backend/artifact: starts=%d created=%d", backend.starts.Load(), factory.created)
			}
		})
	}
}

func TestTerminalTombstoneRejectsLateStart(t *testing.T) {
	t.Parallel()

	backend, factory := &fakeBackend{}, &fakeFactory{}
	c := newTestCoordinator(t, backend, factory, nil)
	req := validStartRequest()
	ticket := c.RequestStop(StopRequest{
		RunID: req.RunID, Epoch: req.Epoch, State: "aborted", Validity: "invalid",
		Reason: "required-failed", BoundaryAt: req.BoundaryStart,
	})
	if ticket.Code != CodeNoActiveCapture {
		t.Fatalf("RequestStop = %#v, want tombstone without active capture", ticket)
	}
	result := c.StartRun(context.Background(), req)
	if result.State != StateSkipped || result.Code != CodeTerminalFenced {
		t.Fatalf("late StartRun = %#v, want terminal-fenced", result)
	}
	if backend.starts.Load() != 0 || factory.created != 0 {
		t.Fatal("terminal-fenced start created capture resources")
	}
}

func TestStartRunReplayRequiresIdenticalDTO(t *testing.T) {
	t.Parallel()

	backend, factory := &fakeBackend{}, &fakeFactory{}
	c := newTestCoordinator(t, backend, factory, nil)
	req := validStartRequest()
	first := c.StartRun(context.Background(), req)
	if first.State != StateCapturing {
		t.Fatalf("first = %#v", first)
	}
	replay := c.StartRun(context.Background(), req)
	if replay.CaptureID != first.CaptureID || replay.State != StateReplayed {
		t.Fatalf("replay = %#v, first = %#v", replay, first)
	}
	mismatch := req
	mismatch.Validity = "partial"
	bad := c.StartRun(context.Background(), mismatch)
	if bad.State != StateSkipped || bad.Code != CodeReplayMismatch {
		t.Fatalf("mismatch = %#v, want replay-mismatch", bad)
	}
	if backend.starts.Load() != 1 || factory.created != 1 {
		t.Fatalf("replay duplicated resources: starts=%d created=%d", backend.starts.Load(), factory.created)
	}
}

func TestTerminalTombstonesAreBoundedAndFenceNewestRuns(t *testing.T) {
	t.Parallel()

	c := newTestCoordinator(t, &fakeBackend{}, &fakeFactory{}, nil)
	boundary := time.Now()
	for index := 1; index <= defaultLedgerSize+10; index++ {
		c.RequestStop(StopRequest{
			RunID: fmt.Sprintf("required-%03d", index), Epoch: 1, State: "aborted", Validity: "invalid",
			Reason: "required-failed", BoundaryAt: boundary,
		})
	}
	c.mu.Lock()
	count := len(c.tombstones)
	c.mu.Unlock()
	if count != defaultLedgerSize {
		t.Fatalf("tombstones = %d, want %d", count, defaultLedgerSize)
	}
	newest := validStartRequest()
	newest.RunID, newest.Epoch = fmt.Sprintf("required-%03d", defaultLedgerSize+10), 1
	if got := c.StartRun(context.Background(), newest); got.Code != CodeTerminalFenced {
		t.Fatalf("newest tombstone start = %#v", got)
	}
	oldest := validStartRequest()
	oldest.RunID, oldest.Epoch = "required-001", 1
	if got := c.StartRun(context.Background(), oldest); got.State != StateCapturing {
		t.Fatalf("evicted tombstone start = %#v", got)
	}
}

func TestRequestStopIsNonBlockingIdempotentAndEpochFenced(t *testing.T) {
	t.Parallel()

	stopWait := make(chan struct{})
	backend, factory := &fakeBackend{stopWait: stopWait}, &fakeFactory{}
	c := newTestCoordinator(t, backend, factory, nil)
	req := validStartRequest()
	started := c.StartRun(context.Background(), req)
	if started.State != StateCapturing {
		t.Fatalf("StartRun = %#v", started)
	}

	begin := time.Now()
	stale := c.RequestStop(StopRequest{RunID: req.RunID, Epoch: req.Epoch + 1, Reason: "finish", BoundaryAt: time.Now()})
	if stale.Code != CodeStaleEpoch || backend.stops.Load() != 0 {
		t.Fatalf("stale stop = %#v, stops=%d", stale, backend.stops.Load())
	}
	ticket := c.RequestStop(StopRequest{RunID: req.RunID, Epoch: req.Epoch, Reason: "finish", BoundaryAt: time.Now()})
	if elapsed := time.Since(begin); elapsed > 50*time.Millisecond {
		t.Fatalf("RequestStop blocked for %v", elapsed)
	}
	replay := c.RequestStop(StopRequest{RunID: req.RunID, Epoch: req.Epoch, Reason: "finish", BoundaryAt: ticket.BoundaryAt})
	if replay.CaptureID != ticket.CaptureID || replay.Code != CodeStopAlreadyRequested {
		t.Fatalf("stop replay = %#v, first = %#v", replay, ticket)
	}
	close(stopWait)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status := c.Await(ticket, ctx)
	if status.State != StatePublished {
		t.Fatalf("Await = %#v, want published", status)
	}
	if backend.stops.Load() != 1 {
		t.Fatalf("StopCPUProfile calls = %d, want 1", backend.stops.Load())
	}
}

func TestVisibleArtifactWithDurabilityErrorRemainsPublished(t *testing.T) {
	t.Parallel()
	durabilityErr := errors.New("directory fsync failed after visibility")
	factory := &fakeFactory{
		result:     PublishedArtifact{File: "cpu.pprof", SHA256: "hash", Bytes: 12, Visible: true, Durability: "unknown"},
		publishErr: durabilityErr,
	}
	callbackCalls := 0
	c := newTestCoordinator(t, &fakeBackend{}, factory, func(options *Options) {
		options.AfterPublish = func(artifact PublishedArtifact) error {
			callbackCalls++
			if !artifact.Visible || artifact.Durability != "unknown" {
				t.Fatalf("after-publish artifact = %#v", artifact)
			}
			return nil
		}
	})
	req := validStartRequest()
	c.StartRun(context.Background(), req)
	ticket := c.RequestStop(StopRequest{RunID: req.RunID, Epoch: req.Epoch, Reason: "finish-accepted", BoundaryAt: req.BoundaryStart.Add(time.Second)})
	status := c.Await(ticket, context.Background())
	if status.State != StatePublished || !status.Artifact.Visible || status.Artifact.Durability != "unknown" ||
		!errors.Is(status.Err, durabilityErr) || callbackCalls != 1 {
		t.Fatalf("status = %#v callbackCalls=%d", status, callbackCalls)
	}
}

func TestStartFailureAbortsArtifactAndReleasesOwner(t *testing.T) {
	t.Parallel()

	backend, factory := &fakeBackend{startErr: errors.New("manual profiler active")}, &fakeFactory{}
	c := newTestCoordinator(t, backend, factory, nil)
	result := c.StartRun(context.Background(), validStartRequest())
	if result.State != StateFailed || result.Code != CodeStartFailed {
		t.Fatalf("StartRun = %#v, want failed/start-failed", result)
	}
	if len(factory.artifacts) != 1 || factory.artifacts[0].aborted != 1 {
		t.Fatalf("failed artifact cleanup = %#v", factory.artifacts)
	}
	next := validStartRequest()
	next.RunID, next.Epoch = "run-2", 2
	backend.startErr = nil
	if got := c.StartRun(context.Background(), next); got.State != StateCapturing {
		t.Fatalf("next StartRun = %#v, want capturing", got)
	}
}

func TestAwaitReturnsPendingWhenCallerBudgetExpires(t *testing.T) {
	t.Parallel()

	stopWait := make(chan struct{})
	backend, factory := &fakeBackend{stopWait: stopWait}, &fakeFactory{}
	c := newTestCoordinator(t, backend, factory, nil)
	req := validStartRequest()
	c.StartRun(context.Background(), req)
	ticket := c.RequestStop(StopRequest{RunID: req.RunID, Epoch: req.Epoch, Reason: "finish", BoundaryAt: time.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	status := c.Await(ticket, ctx)
	if status.State != StateStopping || !errors.Is(status.Err, context.DeadlineExceeded) {
		t.Fatalf("Await = %#v, want stopping/deadline", status)
	}
	close(stopWait)
}

func TestStopStallBecomesWedgedAndLateCompletionRecoversOwner(t *testing.T) {
	t.Parallel()

	stopWait := make(chan struct{})
	backend, factory := &fakeBackend{stopWait: stopWait}, &fakeFactory{}
	c := newTestCoordinator(t, backend, factory, func(o *Options) { o.StopStall = 10 * time.Millisecond })
	req := validStartRequest()
	c.StartRun(context.Background(), req)
	ticket := c.RequestStop(StopRequest{RunID: req.RunID, Epoch: req.Epoch, Reason: "finish", BoundaryAt: time.Now()})
	time.Sleep(25 * time.Millisecond)
	status, ok := c.Status(req.RunID, req.Epoch)
	if !ok || status.State != StateWedged || status.Code != CodeStopWedged {
		t.Fatalf("wedged status = %#v ok=%v", status, ok)
	}
	next := validStartRequest()
	next.RunID, next.Epoch = "run-2", 2
	if result := c.StartRun(context.Background(), next); result.State != StateSkipped || result.Code != CodeCPUBusy {
		t.Fatalf("start while stop wedged = %#v", result)
	}
	close(stopWait)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status = c.Await(ticket, ctx)
	if status.State != StatePublished {
		t.Fatalf("late completion status = %#v", status)
	}
	if result := c.StartRun(context.Background(), next); result.State != StateCapturing {
		t.Fatalf("start after late recovery = %#v", result)
	}
}

func TestStartStallLateSuccessIsStoppedAndOrphaned(t *testing.T) {
	t.Parallel()

	startWait := make(chan struct{})
	backend, factory := &fakeBackend{startWait: startWait}, &fakeFactory{}
	c := newTestCoordinator(t, backend, factory, func(o *Options) { o.StartJoin = 10 * time.Millisecond })
	req := validStartRequest()
	result := c.StartRun(context.Background(), req)
	if result.State != StateStartWedged || result.Code != CodeStartWedged {
		t.Fatalf("StartRun = %#v, want start-wedged", result)
	}
	close(startWait)
	deadline := time.Now().Add(time.Second)
	for {
		status, ok := c.Status(req.RunID, req.Epoch)
		if ok && status.State == StateOrphaned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late start was not stopped/orphaned: %#v ok=%v", status, ok)
		}
		time.Sleep(time.Millisecond)
	}
	if backend.stops.Load() != 1 {
		t.Fatalf("late start StopCPUProfile calls = %d", backend.stops.Load())
	}
}
