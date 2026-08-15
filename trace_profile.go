package isutools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/internal/safefs"
	"github.com/ekusiadadus/isutools/internal/tracecapture"
	"github.com/ekusiadadus/isutools/web"
)

const (
	envTraceSeconds  = "ISUTOOLS_TRACE_SECONDS"
	envTraceMaxBytes = "ISUTOOLS_TRACE_MAX_BYTES"
)

type traceCoordinator interface {
	Start(tracecapture.StartRequest) tracecapture.StartResult
	RequestStop(tracecapture.StopRequest) tracecapture.StopTicket
	Await(tracecapture.StopTicket, context.Context) tracecapture.Status
	Status(string, uint64) (tracecapture.Status, bool)
	Close()
}

type traceWebBridge struct {
	owner   traceCoordinator
	dataDir string
}

func (b *traceWebBridge) StartRun(request web.TraceStartRequest) web.TraceStartResult {
	if b == nil || b.owner == nil {
		return web.TraceStartResult{RunID: request.RunID, Epoch: request.Epoch, State: tracecapture.StateSkipped, Code: "trace-owner-unavailable"}
	}
	result := b.owner.Start(tracecapture.StartRequest{
		RunID: request.RunID, Epoch: request.Epoch, State: request.State, Validity: request.Validity, BoundaryAt: request.BoundaryAt,
	})
	return web.TraceStartResult{RunID: result.RunID, Epoch: result.Epoch, CaptureID: result.CaptureID, State: result.State, Code: result.Code}
}

func (b *traceWebBridge) RequestStop(request web.TraceStopRequest) web.TraceStopTicket {
	if b == nil || b.owner == nil {
		return web.TraceStopTicket{RunID: request.RunID, Epoch: request.Epoch, State: tracecapture.StateSkipped, Code: "trace-owner-unavailable"}
	}
	ticket := b.owner.RequestStop(tracecapture.StopRequest{
		RunID: request.RunID, Epoch: request.Epoch, Reason: request.Reason, BoundaryAt: request.BoundaryAt, Abort: request.Abort,
	})
	return web.TraceStopTicket{RunID: ticket.RunID, Epoch: ticket.Epoch, CaptureID: ticket.CaptureID, State: ticket.State, Code: ticket.Code}
}

func (b *traceWebBridge) Await(ticket web.TraceStopTicket, ctx context.Context) web.TraceCaptureStatus {
	if b == nil || b.owner == nil {
		return web.TraceCaptureStatus{State: tracecapture.StateSkipped, Code: "trace-owner-unavailable"}
	}
	status := b.owner.Await(tracecapture.StopTicket{
		RunID: ticket.RunID, Epoch: ticket.Epoch, CaptureID: ticket.CaptureID, State: ticket.State, Code: ticket.Code,
	}, ctx)
	if status.State == tracecapture.StatePublished && status.File != "" {
		web.PruneProfileArtifacts(b.dataDir, status.File)
	}
	return web.TraceCaptureStatus{State: status.State, Code: status.Code}
}

func (b *traceWebBridge) Manifest(runID string, epoch uint64) *web.TraceIntervalCapture {
	if b == nil || b.owner == nil {
		return nil
	}
	status, ok := b.owner.Status(runID, epoch)
	if !ok {
		return nil
	}
	return &web.TraceIntervalCapture{
		RunID: status.RunID, Epoch: status.Epoch, CaptureID: status.CaptureID, ExpectedFile: "trace_" + status.CaptureID + ".out",
		File: status.File, SHA256: status.SHA256, Bytes: status.Bytes, Sidecar: status.Sidecar, SidecarSHA256: status.SidecarSHA256,
		Status: status.State, Code: status.Code, BoundaryStart: status.BoundaryStart, BoundaryFinish: status.BoundaryFinish,
		StartCompleted: status.StartCompleted, StopCompleted: status.StopCompleted, StopReason: status.StopReason,
		RequestedSpanNs: int64(status.RequestedSpan), CaptureSpanNs: int64(status.CaptureSpan),
		HeadLossNs: int64(status.HeadLoss), TailExcessNs: int64(status.TailExcess), Complete: status.Complete,
	}
}

type traceRunObserver struct{ owner traceCoordinator }

func (o traceRunObserver) OnRunTermination(event runctl.RunTerminationEvent) {
	abort := event.State == runctl.StateAborting || event.State == runctl.StateAborted || event.Validity == runctl.ValidityInvalid
	o.owner.RequestStop(tracecapture.StopRequest{
		RunID: event.RunID, Epoch: uint64(event.Epoch), Reason: event.Reason, BoundaryAt: event.BoundaryAt, Abort: abort,
	})
}

func newRunTraceCoordinator(getenv func(string) string, cpuMode string, profiles []string) (traceCoordinator, *traceWebBridge, *safefs.Root) {
	duration, enabled, err := traceDuration(getenv(envTraceSeconds))
	if err != nil {
		collectorHealth.Set("profile-trace", health.StatusDegraded, err.Error())
		return nil, nil, nil
	}
	if !enabled {
		collectorHealth.Set("profile-trace", health.StatusDisabled, "trace=off")
		return nil, nil, nil
	}
	if cpuMode != "" || len(profiles) != 0 {
		collectorHealth.Set("profile-trace", health.StatusDegraded, "profiler-conflict: trace requires CPU and runtime profile capture to be off")
		return nil, nil, nil
	}
	dataDir := strings.TrimSpace(getenv(envDataDir))
	if dataDir == "" {
		collectorHealth.Set("profile-trace", health.StatusDegraded, "trace requires ISUTOOLS_DATA_DIR")
		return nil, nil, nil
	}
	maxBytes := tracecapture.DefaultMaxBytes
	if value := strings.TrimSpace(getenv(envTraceMaxBytes)); value != "" {
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || parsed < 1 || parsed > tracecapture.MaxBytes {
			collectorHealth.Set("profile-trace", health.StatusDegraded, "invalid ISUTOOLS_TRACE_MAX_BYTES")
			return nil, nil, nil
		}
		maxBytes = parsed
	}
	root, err := safefs.Open(dataDir, safefs.Options{RequireStrongVisibility: true, Exclusive: true})
	if err != nil {
		collectorHealth.Set("profile-trace", health.StatusDegraded, "trace data directory is unavailable")
		return nil, nil, nil
	}
	manager, err := tracecapture.New(tracecapture.Options{Root: root, Backend: tracecapture.RuntimeBackend{}, Duration: duration, MaxBytes: maxBytes})
	if err != nil {
		_ = root.Close()
		collectorHealth.Set("profile-trace", health.StatusDegraded, "trace coordinator is unavailable")
		return nil, nil, nil
	}
	collectorHealth.Set("profile-trace", health.StatusOK, fmt.Sprintf("duration=%s max_bytes=%d", duration, maxBytes))
	return manager, &traceWebBridge{owner: manager, dataDir: dataDir}, root
}

func traceDuration(value string) (time.Duration, bool, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "0", "off", "false", "no", "disabled":
		return 0, false, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 1 || seconds > int(tracecapture.MaxDuration/time.Second) {
		return 0, false, errors.New("invalid ISUTOOLS_TRACE_SECONDS")
	}
	return time.Duration(seconds) * time.Second, true, nil
}
