package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/internal/health"
)

func (h *handler) pprof(w http.ResponseWriter, r *http.Request) {
	if h.p.CPUProfiles != nil && h.p.CPUProfileMode == "run" && strings.TrimPrefix(r.URL.Path, "/pprof/") == "profile" {
		http.Error(w, "managed-run-cpu-profile-enabled", http.StatusConflict)
		return
	}
	pprofHandler(w, r)
}

// CPUCaptureCoordinator is the transport-facing view of the process-wide CPU
// profiler owner. Implementations must make RequestStop non-blocking; the
// runtime profiler's synchronous flush belongs to its worker, never an HTTP or
// run-controller goroutine.
type CPUCaptureCoordinator interface {
	StartRun(context.Context, CPUStartRequest) CPUStartResult
	StartFixed(context.Context, FixedCPUStartRequest) CPUStartResult
	RequestStop(CPUStopRequest) CPUStopTicket
	Await(CPUStopTicket, context.Context) CPUCaptureStatus
	Manifest(runID string, epoch uint64) *CPUIntervalCapture
	LabelDictionary(runID string, epoch uint64) *CPULabelDictionary
}

type CPUStartRequest struct {
	RunID            string
	Epoch            uint64
	State            string
	Validity         string
	BoundaryStart    time.Time
	GenerationWindow BoundaryWindow
	BoundaryWindow   BoundaryWindow
}

type FixedCPUStartRequest struct {
	RunID            string
	Epoch            uint64
	State            string
	Validity         string
	Generation       int64
	Duration         time.Duration
	RequestedAt      time.Time
	GenerationWindow BoundaryWindow
	BoundaryWindow   BoundaryWindow
}

type CPUStartResult struct {
	RunID     string
	CaptureID string
	Epoch     uint64
	State     string
	Code      string
}

type CPUStopRequest struct {
	RunID      string
	Epoch      uint64
	State      string
	Validity   string
	Reason     string
	BoundaryAt time.Time
}

type CPUStopTicket struct {
	RunID      string
	CaptureID  string
	Epoch      uint64
	State      string
	Code       string
	BoundaryAt time.Time
}

type CPUCaptureStatus struct {
	RunID            string
	CaptureID        string
	Epoch            uint64
	State            string
	Code             string
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

// CPUIntervalCapture is the immutable snapshot projection. Fields are added
// only when the underlying artifact and hash have been fixed.
type CPUIntervalCapture struct {
	RunID            string    `json:"run_id"`
	Epoch            uint64    `json:"epoch"`
	CaptureID        string    `json:"capture_id"`
	ExpectedFile     string    `json:"expected_file"`
	File             string    `json:"file,omitempty"`
	SHA256           string    `json:"sha256,omitempty"`
	Bytes            int64     `json:"bytes,omitempty"`
	Sidecar          string    `json:"sidecar,omitempty"`
	SidecarSHA256    string    `json:"sidecar_sha256,omitempty"`
	CoverageFile     string    `json:"coverage_file,omitempty"`
	CoverageSHA256   string    `json:"coverage_sha256,omitempty"`
	Status           string    `json:"status"`
	Code             string    `json:"code,omitempty"`
	BoundaryStart    time.Time `json:"boundary_start,omitzero"`
	BoundaryFinish   time.Time `json:"boundary_finish,omitzero"`
	StartRequestedAt time.Time `json:"start_requested_at,omitzero"`
	StartCompletedAt time.Time `json:"start_completed_at,omitzero"`
	StopRequestedAt  time.Time `json:"stop_requested_at,omitzero"`
	StopCompletedAt  time.Time `json:"stop_completed_at,omitzero"`
	StopReason       string    `json:"stop_reason,omitempty"`
	RunSpanNs        int64     `json:"run_span_ns,omitempty"`
	CaptureSpanNs    int64     `json:"capture_span_ns,omitempty"`
	HeadLossNs       int64     `json:"head_loss_ns,omitempty"`
	TailExcessNs     int64     `json:"tail_excess_ns,omitempty"`
	TailLossNs       int64     `json:"tail_loss_ns,omitempty"`
	Complete         bool      `json:"complete"`
}

type CPULabelDictionary struct {
	RunID     string           `json:"run_id"`
	Epoch     uint64           `json:"epoch"`
	CaptureID string           `json:"capture_id"`
	Sealed    bool             `json:"sealed"`
	Tuples    []SafeLabelTuple `json:"tuples"`
	SHA256    string           `json:"sha256"`
}

type SafeLabelTuple struct {
	TupleID  string `json:"tuple_id"`
	Method   string `json:"method"`
	Route    string `json:"route"`
	Scenario string `json:"scenario,omitempty"`
	Region   string `json:"region,omitempty"`
	Overflow bool   `json:"overflow,omitempty"`
}

const cpuStopPublicationBudget = 2 * time.Second

func (h *handler) startCPUProfiles(ctx context.Context, run RunStart, generation int64) {
	if h.p.CPUProfiles == nil {
		// Compatibility path for standalone web.Provider users. The standard
		// isutools stack injects its process-wide owner instead.
		h.captureCPUProfile(generation)
		return
	}
	switch h.p.CPUProfileMode {
	case "run":
		result := h.p.CPUProfiles.StartRun(ctx, CPUStartRequest{
			RunID: run.RunID, Epoch: run.Epoch, State: run.State, Validity: run.Validity,
			BoundaryStart: cpuStartBoundary(run), GenerationWindow: run.GenerationWindow, BoundaryWindow: run.BoundaryWindow,
		})
		h.noteCPUStart(result)
	case "fixed":
		if h.p.PprofDuration <= 0 {
			return
		}
		result := h.p.CPUProfiles.StartFixed(ctx, FixedCPUStartRequest{
			RunID: run.RunID, Epoch: run.Epoch, State: run.State, Validity: run.Validity,
			Generation: generation, Duration: h.p.PprofDuration, RequestedAt: time.Now(),
			GenerationWindow: run.GenerationWindow, BoundaryWindow: run.BoundaryWindow,
		})
		h.noteCPUStart(result)
	}
}

func cpuStartBoundary(run RunStart) time.Time {
	if !run.GenerationWindow.Max.IsZero() {
		return run.GenerationWindow.Max
	}
	return run.StartedAt
}

func cpuFinishBoundary(run RunFinish) time.Time {
	if !run.GenerationWindow.Max.IsZero() {
		return run.GenerationWindow.Max
	}
	return run.AcceptedAt
}

func (h *handler) noteCPUStart(result CPUStartResult) {
	if h.p.Health == nil {
		return
	}
	if result.State == "capturing" || result.State == "replayed" {
		h.p.Health.Set("profile-cpu", health.StatusOK, "")
		return
	}
	h.p.Health.Set("profile-cpu", health.StatusDegraded, result.Code)
}

func (h *handler) requestCPUStop(runID string, epoch uint64, state, validity, reason string, boundary time.Time) CPUStopTicket {
	if h.p.CPUProfiles == nil || h.p.CPUProfileMode != "run" || runID == "" || epoch == 0 || boundary.IsZero() {
		return CPUStopTicket{}
	}
	return h.p.CPUProfiles.RequestStop(CPUStopRequest{
		RunID: runID, Epoch: epoch, State: state, Validity: validity, Reason: reason, BoundaryAt: boundary,
	})
}

func (h *handler) awaitCPUStop(ticket CPUStopTicket) {
	if h.p.CPUProfiles == nil || ticket.CaptureID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cpuStopPublicationBudget)
	defer cancel()
	status := h.p.CPUProfiles.Await(ticket, ctx)
	if h.p.Health != nil && status.Err != nil {
		h.p.Health.Set("profile-cpu", health.StatusDegraded, status.Err.Error())
	}
}
