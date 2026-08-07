package profilecapture

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingCompletionJournal struct {
	mu      sync.Mutex
	records []CompletionRecord
}

type blockingCompletionJournal struct {
	started chan struct{}
	release chan struct{}
}

func (j *blockingCompletionJournal) Record(record CompletionRecord) (CompletionAttachment, error) {
	select {
	case <-j.started:
	default:
		close(j.started)
	}
	<-j.release
	return CompletionAttachment{Phase: record.Phase, Sequence: record.Sequence, File: "blocked.json", Visible: true}, nil
}

func (j *recordingCompletionJournal) Record(record CompletionRecord) (CompletionAttachment, error) {
	j.mu.Lock()
	j.records = append(j.records, record)
	j.mu.Unlock()
	name := "cpu_" + record.CaptureID + ".meta.json"
	if record.Phase == CompletionPhaseCoverage {
		name = "cpu_" + record.CaptureID + ".coverage.json"
	}
	return CompletionAttachment{Phase: record.Phase, Sequence: record.Sequence, File: name, SHA256: strings.Repeat("a", 64), Bytes: 10, Visible: true, Durability: "durable"}, nil
}

func (j *recordingCompletionJournal) snapshot() []CompletionRecord {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]CompletionRecord(nil), j.records...)
}

func TestStatusReportsMeasuredCoverage(t *testing.T) {
	boundary := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	now := boundary.Add(10 * time.Millisecond)
	coordinator := newTestCoordinator(t, &fakeBackend{}, &fakeFactory{}, func(options *Options) {
		options.Now = func() time.Time { return now }
	})
	request := validStartRequest()
	request.BoundaryStart = boundary
	start := coordinator.StartRun(context.Background(), request)
	if start.State != StateCapturing {
		t.Fatalf("start = %#v", start)
	}
	now = boundary.Add(time.Second)
	ticket := coordinator.RequestStop(StopRequest{
		RunID: request.RunID, Epoch: request.Epoch, State: "finishing", Validity: "valid",
		Reason: "finish-accepted", BoundaryAt: boundary.Add(900 * time.Millisecond),
	})
	status := coordinator.Await(ticket, context.Background())
	if status.HeadLoss != 10*time.Millisecond || status.RunSpan != 900*time.Millisecond ||
		status.CaptureSpan != 990*time.Millisecond || status.TailExcess != 100*time.Millisecond || status.TailLoss != 0 || !status.Complete {
		t.Fatalf("coverage = %#v", status)
	}
	if status.StopReason != "finish-accepted" || status.StartRequestedAt.IsZero() || status.StopCompletedAt.IsZero() {
		t.Fatalf("coverage timestamps = %#v", status)
	}
}

func TestLateFinishAfterHardMaxRecordsTailLossWithoutRepublishing(t *testing.T) {
	boundary := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	now := boundary
	coordinator := newTestCoordinator(t, &fakeBackend{}, &fakeFactory{}, func(options *Options) {
		options.Now = func() time.Time { return now }
	})
	request := validStartRequest()
	request.BoundaryStart = boundary
	start := coordinator.StartRun(context.Background(), request)
	if start.State != StateCapturing {
		t.Fatalf("start = %#v", start)
	}
	now = boundary.Add(time.Second)
	maxTicket := coordinator.RequestStop(StopRequest{
		RunID: request.RunID, Epoch: request.Epoch, State: "started", Validity: "valid",
		Reason: ReasonHardMax, BoundaryAt: now,
	})
	if status := coordinator.Await(maxTicket, context.Background()); status.State != StatePublished {
		t.Fatalf("hard max status = %#v", status)
	}
	now = boundary.Add(2 * time.Second)
	late := coordinator.RequestStop(StopRequest{
		RunID: request.RunID, Epoch: request.Epoch, State: "finishing", Validity: "valid",
		Reason: "finish-accepted", BoundaryAt: now,
	})
	status := coordinator.Await(late, context.Background())
	if status.State != StatePublished || status.StopReason != ReasonHardMax || status.TailLoss != time.Second || status.Complete {
		t.Fatalf("late finish coverage = %#v", status)
	}
}

func TestCompletionJournalRecordsInitialAndAppendOnlyLateCoverage(t *testing.T) {
	boundary := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	now := boundary
	journal := &recordingCompletionJournal{}
	var retentionPasses atomic.Int64
	coordinator := newTestCoordinator(t, &fakeBackend{}, &fakeFactory{}, func(options *Options) {
		options.Now = func() time.Time { return now }
		options.Journal = journal
		options.AfterPublish = func(PublishedArtifact) error {
			retentionPasses.Add(1)
			return nil
		}
	})
	request := validStartRequest()
	request.BoundaryStart = boundary
	start := coordinator.StartRun(context.Background(), request)
	now = boundary.Add(time.Second)
	ticket := coordinator.RequestStop(StopRequest{
		RunID: request.RunID, Epoch: request.Epoch, State: "started", Validity: "valid",
		Reason: ReasonHardMax, BoundaryAt: now,
	})
	if status := coordinator.Await(ticket, context.Background()); status.State != StatePublished {
		t.Fatalf("hard-max status = %#v", status)
	}
	records := journal.snapshot()
	if len(records) != 1 || records[0].Phase != CompletionPhaseInitial || records[0].Sequence != 0 ||
		records[0].CaptureID != start.CaptureID || records[0].Profile.File == "" {
		t.Fatalf("initial completion records = %#v", records)
	}
	status, ok := coordinator.Status(request.RunID, request.Epoch)
	if !ok || status.Sidecar.File == "" || status.Sidecar.Phase != CompletionPhaseInitial || status.Coverage.File != "" {
		t.Fatalf("initial status attachments = %#v ok=%v", status, ok)
	}
	if retentionPasses.Load() != 1 {
		t.Fatalf("initial retention passes = %d", retentionPasses.Load())
	}

	now = boundary.Add(2 * time.Second)
	coordinator.RequestStop(StopRequest{
		RunID: request.RunID, Epoch: request.Epoch, State: "finishing", Validity: "valid",
		Reason: "finish-accepted", BoundaryAt: now,
	})
	deadline := time.Now().Add(time.Second)
	for len(journal.snapshot()) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	records = journal.snapshot()
	if len(records) != 2 || records[1].Phase != CompletionPhaseCoverage || records[1].Sequence != 1 ||
		records[1].Coverage.TailLoss != time.Second || records[1].Coverage.Complete {
		t.Fatalf("append-only completion records = %#v", records)
	}
	status, ok = coordinator.Status(request.RunID, request.Epoch)
	if !ok || status.Coverage.File == "" || status.Coverage.Sequence != 1 {
		t.Fatalf("coverage status attachment = %#v ok=%v", status, ok)
	}
	if retentionPasses.Load() != 2 {
		t.Fatalf("coverage retention passes = %d", retentionPasses.Load())
	}
}

func TestBlockedCompletionJournalRetainsOwnerAndBoundsStalledWriters(t *testing.T) {
	journal := &blockingCompletionJournal{started: make(chan struct{}), release: make(chan struct{})}
	coordinator := newTestCoordinator(t, &fakeBackend{}, &fakeFactory{}, func(options *Options) {
		options.Journal = journal
	})
	request := validStartRequest()
	start := coordinator.StartRun(context.Background(), request)
	ticket := coordinator.RequestStop(StopRequest{
		RunID: request.RunID, Epoch: request.Epoch, State: "finishing", Validity: "valid",
		Reason: "finish-accepted", BoundaryAt: request.BoundaryStart.Add(time.Second),
	})
	select {
	case <-journal.started:
	case <-time.After(time.Second):
		t.Fatal("completion journal did not start")
	}
	next := validStartRequest()
	next.RunID, next.Epoch = "run-next", 2
	if result := coordinator.StartRun(context.Background(), next); result.State != StateSkipped || result.Code != CodeCPUBusy {
		close(journal.release)
		t.Fatalf("next capture while journal is blocked = %#v, first=%#v", result, start)
	}
	close(journal.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if status := coordinator.Await(ticket, ctx); status.State != StatePublished || status.Err != nil {
		t.Fatalf("completion status = %#v", status)
	}
	if result := coordinator.StartRun(context.Background(), next); result.State != StateCapturing {
		t.Fatalf("next capture after journal completion = %#v", result)
	}
}

func TestAfterPublishRunsOnceBeforeOwnerIsReleased(t *testing.T) {
	var mu sync.Mutex
	var published []PublishedArtifact
	coordinator := newTestCoordinator(t, &fakeBackend{}, &fakeFactory{}, func(options *Options) {
		options.AfterPublish = func(artifact PublishedArtifact) error {
			mu.Lock()
			published = append(published, artifact)
			mu.Unlock()
			return nil
		}
	})
	request := validStartRequest()
	coordinator.StartRun(context.Background(), request)
	ticket := coordinator.RequestStop(StopRequest{
		RunID: request.RunID, Epoch: request.Epoch, State: "finishing", Validity: "valid",
		Reason: "finish-accepted", BoundaryAt: request.BoundaryStart.Add(time.Second),
	})
	status := coordinator.Await(ticket, context.Background())
	mu.Lock()
	defer mu.Unlock()
	if status.State != StatePublished || status.Err != nil || len(published) != 1 || published[0].File != status.Artifact.File {
		t.Fatalf("status=%#v after-publish=%#v", status, published)
	}
}
