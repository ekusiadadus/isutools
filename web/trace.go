package web

import (
	"context"
	"time"

	"github.com/ekusiadadus/isutools/internal/health"
)

// TraceCaptureCoordinator is the transport-facing owner of one bounded
// runtime execution trace. Stop must be non-blocking.
type TraceCaptureCoordinator interface {
	StartRun(TraceStartRequest) TraceStartResult
	RequestStop(TraceStopRequest) TraceStopTicket
	Await(TraceStopTicket, context.Context) TraceCaptureStatus
	Manifest(runID string, epoch uint64) *TraceIntervalCapture
}

type TraceStartRequest struct {
	RunID      string
	Epoch      uint64
	State      string
	Validity   string
	BoundaryAt time.Time
}

type TraceStartResult struct {
	RunID     string
	Epoch     uint64
	CaptureID string
	State     string
	Code      string
}

type TraceStopRequest struct {
	RunID      string
	Epoch      uint64
	Reason     string
	BoundaryAt time.Time
	Abort      bool
}

type TraceStopTicket struct {
	RunID     string
	Epoch     uint64
	CaptureID string
	State     string
	Code      string
}

type TraceCaptureStatus struct {
	State string
	Code  string
}

type TraceIntervalCapture struct {
	RunID           string    `json:"run_id"`
	Epoch           uint64    `json:"epoch"`
	CaptureID       string    `json:"capture_id"`
	ExpectedFile    string    `json:"expected_file"`
	File            string    `json:"file,omitempty"`
	SHA256          string    `json:"sha256,omitempty"`
	Bytes           int64     `json:"bytes,omitempty"`
	Sidecar         string    `json:"sidecar,omitempty"`
	SidecarSHA256   string    `json:"sidecar_sha256,omitempty"`
	Status          string    `json:"status"`
	Code            string    `json:"code,omitempty"`
	BoundaryStart   time.Time `json:"boundary_start,omitzero"`
	BoundaryFinish  time.Time `json:"boundary_finish,omitzero"`
	StartCompleted  time.Time `json:"start_completed_at,omitzero"`
	StopCompleted   time.Time `json:"stop_completed_at,omitzero"`
	StopReason      string    `json:"stop_reason,omitempty"`
	RequestedSpanNs int64     `json:"requested_span_ns"`
	CaptureSpanNs   int64     `json:"capture_span_ns"`
	HeadLossNs      int64     `json:"head_loss_ns"`
	TailExcessNs    int64     `json:"tail_excess_ns"`
	Complete        bool      `json:"complete"`
}

const traceStopPublicationBudget = 2 * time.Second

func (h *handler) startTrace(run RunStart) TraceStartResult {
	if h.p.TraceCapture == nil || run.RunID == "" {
		return TraceStartResult{}
	}
	result := h.p.TraceCapture.StartRun(TraceStartRequest{
		RunID: run.RunID, Epoch: run.Epoch, State: run.State, Validity: run.Validity, BoundaryAt: cpuStartBoundary(run),
	})
	if h.p.Health != nil {
		if result.State == "capturing" || result.State == "replayed" {
			h.p.Health.Set("profile-trace", health.StatusOK, "")
		} else {
			h.p.Health.Set("profile-trace", health.StatusDegraded, result.Code)
		}
	}
	return result
}

func (h *handler) requestTraceStop(runID string, epoch uint64, reason string, boundary time.Time, abort bool) TraceStopTicket {
	if h.p.TraceCapture == nil || runID == "" || epoch == 0 {
		return TraceStopTicket{}
	}
	return h.p.TraceCapture.RequestStop(TraceStopRequest{RunID: runID, Epoch: epoch, Reason: reason, BoundaryAt: boundary, Abort: abort})
}

func (h *handler) awaitTraceStop(ticket TraceStopTicket) {
	if h.p.TraceCapture == nil || ticket.CaptureID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), traceStopPublicationBudget)
	defer cancel()
	status := h.p.TraceCapture.Await(ticket, ctx)
	if h.p.Health != nil && status.State != "published" && status.State != "aborted" {
		h.p.Health.Set("profile-trace", health.StatusDegraded, status.Code)
	}
}
