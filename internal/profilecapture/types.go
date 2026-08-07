package profilecapture

import (
	"context"
	"io"
	"time"
)

type Mode string

const (
	ModeOff   Mode = "off"
	ModeFixed Mode = "fixed"
	ModeRun   Mode = "run"
)

type State string

const (
	StateIdle        State = "idle"
	StateStarting    State = "starting"
	StateCapturing   State = "capturing"
	StateReplayed    State = "replayed"
	StateStopping    State = "stopping"
	StateStartWedged State = "start-wedged"
	StateWedged      State = "wedged"
	StatePublished   State = "published"
	StateOrphaned    State = "orphaned"
	StateFailed      State = "failed"
	StateSkipped     State = "skipped"
)

const (
	CodeInvalidRequest       = "invalid-request"
	CodeRunNotStarted        = "run-not-started"
	CodeRunInvalid           = "run-invalid"
	CodeTerminalFenced       = "terminal-fenced"
	CodeReplayMismatch       = "replay-mismatch"
	CodeCPUBusy              = "cpu-busy"
	CodeArtifactCreateFailed = "artifact-create-failed"
	CodeStartFailed          = "start-failed"
	CodeStartWedged          = "start-wedged"
	CodeStaleEpoch           = "stale-epoch"
	CodeNoActiveCapture      = "no-active-capture"
	CodeStopAlreadyRequested = "stop-already-requested"
	CodeStopWedged           = "stop-wedged"
)

const (
	ReasonHardMax     = "max-duration"
	ReasonStartWedged = "start-wedged"
)

// BoundaryWindow is copied from the run controller without importing it.
type BoundaryWindow struct {
	Min    time.Time
	Max    time.Time
	Spread time.Duration
}

type StartRequest struct {
	RunID            string
	Epoch            uint64
	State            string
	Validity         string
	BoundaryStart    time.Time
	GenerationWindow BoundaryWindow
	BoundaryWindow   BoundaryWindow
}

type StopRequest struct {
	RunID      string
	Epoch      uint64
	State      string
	Validity   string
	Reason     string
	BoundaryAt time.Time
}

type StartResult struct {
	RunID     string
	CaptureID string
	Epoch     uint64
	State     State
	Code      string
}

type StopTicket struct {
	RunID      string
	CaptureID  string
	Epoch      uint64
	State      State
	Code       string
	BoundaryAt time.Time
}

type Status struct {
	RunID            string
	CaptureID        string
	Epoch            uint64
	State            State
	Code             string
	Artifact         PublishedArtifact
	Sidecar          CompletionAttachment
	Coverage         CompletionAttachment
	Err              error
	BoundaryStart    time.Time
	BoundaryFinish   time.Time
	StartRequestedAt time.Time
	StartCompletedAt time.Time
	StopRequestedAt  time.Time
	StopCompletedAt  time.Time
	StopReason       string
	RunSpan          time.Duration
	CaptureSpan      time.Duration
	HeadLoss         time.Duration
	TailExcess       time.Duration
	TailLoss         time.Duration
	Complete         bool
}

type PublishedArtifact struct {
	File       string
	SHA256     string
	Bytes      int64
	Visible    bool
	Durability string
}

// Backend is the process-wide runtime CPU profiler. Its Stop method has no
// context by design: runtime/pprof.StopCPUProfile cannot be interrupted.
type Backend interface {
	StartCPUProfile(io.Writer) error
	StopCPUProfile()
}

// Artifact owns an unpublished, local regular file. Publish must be an
// immutable no-replace operation; Abort removes only its temporary file.
type Artifact interface {
	Writer() io.Writer
	Publish() (PublishedArtifact, error)
	Abort() error
}

type ArtifactFactory interface {
	New(StartRequest, string) (Artifact, error)
}

type CoordinatorAPI interface {
	StartRun(context.Context, StartRequest) StartResult
	RequestStop(StopRequest) StopTicket
	Await(StopTicket, context.Context) Status
}
