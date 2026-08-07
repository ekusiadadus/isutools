package isutools

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/profilecapture"
	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/internal/safefs"
	"github.com/ekusiadadus/isutools/web"
)

const (
	envCPUProfileMode     = "ISUTOOLS_CPU_PROFILE_MODE"
	envCPUProfileLabels   = "ISUTOOLS_PPROF_LABELS"
	envSafeProfileRoutes  = "ISUTOOLS_PPROF_SAFE_ROUTE_RULES"
	envProfileAnalysis    = "ISUTOOLS_PROFILE_ANALYSIS"
	defaultRunCPUHardMax  = 120 * time.Second
	maxRunCPUHardMax      = 600 * time.Second
	cpuStartJoinBudget    = 100 * time.Millisecond
	cpuStopStallThreshold = 2 * time.Second
	cpuArtifactMaxBytes   = 32 << 20
)

type cpuCoordinator interface {
	StartRun(context.Context, profilecapture.StartRequest) profilecapture.StartResult
	RequestStop(profilecapture.StopRequest) profilecapture.StopTicket
	Await(profilecapture.StopTicket, context.Context) profilecapture.Status
	Status(runID string, epoch uint64) (profilecapture.Status, bool)
	ActiveLabelScope() *profilecapture.LabelScope
	LabelDictionary(runID string, epoch uint64) (profilecapture.LabelDictionary, bool)
	Close()
}

type fixedCPUProfiler interface {
	Capture(generation int64) bool
}

type cpuWebBridge struct {
	owner cpuCoordinator
	fixed fixedCPUProfiler
}

func (b *cpuWebBridge) StartRun(ctx context.Context, req web.CPUStartRequest) web.CPUStartResult {
	if b == nil || b.owner == nil {
		return web.CPUStartResult{RunID: req.RunID, Epoch: req.Epoch, State: "skipped", Code: "run-owner-unavailable"}
	}
	result := b.owner.StartRun(ctx, profilecapture.StartRequest{
		RunID: req.RunID, Epoch: req.Epoch, State: req.State, Validity: req.Validity,
		BoundaryStart:    req.BoundaryStart,
		GenerationWindow: cpuBoundaryWindow(req.GenerationWindow), BoundaryWindow: cpuBoundaryWindow(req.BoundaryWindow),
	})
	return web.CPUStartResult{RunID: result.RunID, CaptureID: result.CaptureID, Epoch: result.Epoch, State: string(result.State), Code: result.Code}
}

func (b *cpuWebBridge) StartFixed(_ context.Context, req web.FixedCPUStartRequest) web.CPUStartResult {
	result := web.CPUStartResult{RunID: req.RunID, Epoch: req.Epoch, State: "skipped"}
	if b == nil || b.fixed == nil {
		result.Code = "fixed-owner-unavailable"
		return result
	}
	if req.RunID == "" || req.Epoch == 0 || req.State != "started" ||
		(req.Validity != "valid" && req.Validity != "partial") || req.Generation <= 0 || req.Duration <= 0 || req.RequestedAt.IsZero() {
		result.Code = "invalid-fixed-request"
		return result
	}
	if !b.fixed.Capture(req.Generation) {
		result.Code = "fixed-start-failed"
		return result
	}
	result.State = "capturing"
	return result
}

func (b *cpuWebBridge) RequestStop(req web.CPUStopRequest) web.CPUStopTicket {
	if b == nil || b.owner == nil {
		return web.CPUStopTicket{RunID: req.RunID, Epoch: req.Epoch, State: "skipped", Code: "fixed-timer-owned", BoundaryAt: req.BoundaryAt}
	}
	ticket := b.owner.RequestStop(profilecapture.StopRequest{
		RunID: req.RunID, Epoch: req.Epoch, State: req.State, Validity: req.Validity,
		Reason: req.Reason, BoundaryAt: req.BoundaryAt,
	})
	return web.CPUStopTicket{
		RunID: ticket.RunID, CaptureID: ticket.CaptureID, Epoch: ticket.Epoch,
		State: string(ticket.State), Code: ticket.Code, BoundaryAt: ticket.BoundaryAt,
	}
}

func (b *cpuWebBridge) Await(ticket web.CPUStopTicket, ctx context.Context) web.CPUCaptureStatus {
	if b == nil || b.owner == nil {
		return web.CPUCaptureStatus{RunID: ticket.RunID, Epoch: ticket.Epoch, CaptureID: ticket.CaptureID, State: "skipped", Code: ticket.Code}
	}
	status := b.owner.Await(profilecapture.StopTicket{
		RunID: ticket.RunID, CaptureID: ticket.CaptureID, Epoch: ticket.Epoch,
		State: profilecapture.State(ticket.State), Code: ticket.Code, BoundaryAt: ticket.BoundaryAt,
	}, ctx)
	return web.CPUCaptureStatus{
		RunID: status.RunID, CaptureID: status.CaptureID, Epoch: status.Epoch,
		State: string(status.State), Code: status.Code, Err: status.Err,
		BoundaryStart: status.BoundaryStart, BoundaryFinish: status.BoundaryFinish,
		StartRequestedAt: status.StartRequestedAt, StartCompletedAt: status.StartCompletedAt,
		StopRequestedAt: status.StopRequestedAt, StopCompletedAt: status.StopCompletedAt,
		StopReason: status.StopReason, RunSpan: status.RunSpan, CaptureSpan: status.CaptureSpan,
		HeadLoss: status.HeadLoss, TailExcess: status.TailExcess, TailLoss: status.TailLoss,
		Complete: status.Complete,
	}
}

func (b *cpuWebBridge) Manifest(runID string, epoch uint64) *web.CPUIntervalCapture {
	if b == nil || b.owner == nil {
		return nil
	}
	status, ok := b.owner.Status(runID, epoch)
	if !ok {
		return nil
	}
	return &web.CPUIntervalCapture{
		RunID: status.RunID, Epoch: status.Epoch, CaptureID: status.CaptureID,
		ExpectedFile: "cpu_" + status.CaptureID + ".pprof",
		File:         status.Artifact.File, SHA256: status.Artifact.SHA256, Bytes: status.Artifact.Bytes,
		Sidecar: status.Sidecar.File, SidecarSHA256: status.Sidecar.SHA256,
		CoverageFile: status.Coverage.File, CoverageSHA256: status.Coverage.SHA256,
		Status: string(status.State), Code: status.Code,
		BoundaryStart: status.BoundaryStart, BoundaryFinish: status.BoundaryFinish,
		StartRequestedAt: status.StartRequestedAt, StartCompletedAt: status.StartCompletedAt,
		StopRequestedAt: status.StopRequestedAt, StopCompletedAt: status.StopCompletedAt,
		StopReason: status.StopReason, RunSpanNs: int64(status.RunSpan), CaptureSpanNs: int64(status.CaptureSpan),
		HeadLossNs: int64(status.HeadLoss), TailExcessNs: int64(status.TailExcess), TailLossNs: int64(status.TailLoss),
		Complete: status.Complete,
	}
}

func (b *cpuWebBridge) LabelDictionary(runID string, epoch uint64) *web.CPULabelDictionary {
	if b == nil || b.owner == nil {
		return nil
	}
	dictionary, ok := b.owner.LabelDictionary(runID, epoch)
	if !ok {
		return nil
	}
	tuples := make([]web.SafeLabelTuple, len(dictionary.Tuples))
	for i, tuple := range dictionary.Tuples {
		tuples[i] = web.SafeLabelTuple{
			TupleID: tuple.TupleID, Method: tuple.Method, Route: tuple.Route,
			Scenario: tuple.Scenario, Region: tuple.Region, Overflow: tuple.Overflow,
		}
	}
	return &web.CPULabelDictionary{
		RunID: dictionary.RunID, Epoch: dictionary.Epoch, CaptureID: dictionary.CaptureID,
		Sealed: dictionary.Sealed, Tuples: tuples, SHA256: dictionary.SHA256,
	}
}

type cpuHTTPLabeler struct{ owner cpuCoordinator }

func (l cpuHTTPLabeler) DoProfileLabels(ctx context.Context, label httpstats.ProfileLabel, fn func(context.Context)) bool {
	if fn == nil {
		return false
	}
	if l.owner == nil {
		fn(ctx)
		return false
	}
	scope := l.owner.ActiveLabelScope()
	if scope == nil {
		fn(ctx)
		return false
	}
	logical, _ := profilecapture.LogicalLabels(ctx)
	logical.Method, logical.Route = label.Method, label.Route
	return scope.Do(ctx, logical, fn)
}

// ProfileScenario binds a safe logical scenario to CPU samples taken while fn
// runs. The value is never written directly into the pprof string table; the
// active capture stores it behind an opaque tuple ID. Invalid values and an
// inactive profiler fail open and still invoke fn.
func ProfileScenario(ctx context.Context, scenario string, fn func(context.Context)) {
	doProfileDimension(ctx, scenario, "", fn)
}

// ProfileRegion is the region counterpart to ProfileScenario.
func ProfileRegion(ctx context.Context, region string, fn func(context.Context)) {
	doProfileDimension(ctx, "", region, fn)
}

func doProfileDimension(ctx context.Context, scenario, region string, fn func(context.Context)) {
	if fn == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	core := defaultMeasurement()
	if core.cpu == nil {
		fn(ctx)
		return
	}
	scope := core.cpu.ActiveLabelScope()
	if scope == nil {
		fn(ctx)
		return
	}
	logical, ok := profilecapture.LogicalLabels(ctx)
	if !ok {
		logical.Method, logical.Route = httpstats.ProfileMethodOther, httpstats.ProfileRouteUnmatched
	}
	if scenario != "" {
		logical.Scenario = scenario
	}
	if region != "" {
		logical.Region = region
	}
	scope.Do(ctx, logical, fn)
}

type cpuRunObserver struct{ owner cpuCoordinator }

func (o cpuRunObserver) OnRunTermination(event runctl.RunTerminationEvent) {
	// RequestStop only records a tombstone and queues one stop goroutine. It
	// does not call runtime/pprof.StopCPUProfile on this lifecycle goroutine.
	o.owner.RequestStop(profilecapture.StopRequest{
		RunID: event.RunID, Epoch: uint64(event.Epoch), State: string(event.State),
		Validity: string(event.Validity), Reason: event.Reason, BoundaryAt: event.BoundaryAt,
	})
}

type joinedRunObservers []runctl.LifecycleObserver

func (observers joinedRunObservers) OnRunTermination(event runctl.RunTerminationEvent) {
	for _, observer := range observers {
		if observer != nil {
			observer.OnRunTermination(event)
		}
	}
}

func newRunCPUCoordinator(getenv func(string) string) (cpuCoordinator, *cpuWebBridge, *safefs.Root, string) {
	mode := getenv(envCPUProfileMode)
	if mode == "" {
		return nil, nil, nil, ""
	}
	if mode == string(profilecapture.ModeFixed) {
		dataDir, duration := getenv(envDataDir), pprofDuration(getenv)
		profiler := web.NewFixedCPUProfiler(dataDir, duration)
		if profiler == nil {
			collectorHealth.Set("profile-cpu", health.StatusDegraded, "fixed CPU profiling requires ISUTOOLS_DATA_DIR and positive ISUTOOLS_PPROF_SECONDS")
			return nil, nil, nil, ""
		}
		root, err := safefs.Open(dataDir, safefs.Options{RequireStrongVisibility: true, Exclusive: true})
		if err != nil {
			collectorHealth.Set("profile-cpu", health.StatusDegraded, err.Error())
			return nil, nil, nil, ""
		}
		// Fixed mode has one shared timer owner for Handler and ResetNow, but no
		// run-lifecycle observer: finish/abort must never stop this mode.
		return nil, &cpuWebBridge{fixed: profiler}, root, string(profilecapture.ModeFixed)
	}
	if mode != string(profilecapture.ModeRun) {
		collectorHealth.Set(healthProfiles, health.StatusDegraded, fmt.Sprintf("unknown %s=%q; CPU profiling disabled", envCPUProfileMode, mode))
		return nil, nil, nil, ""
	}
	dataDir := getenv(envDataDir)
	if dataDir == "" {
		collectorHealth.Set("profile-cpu", health.StatusDegraded, "run CPU profiling requires ISUTOOLS_DATA_DIR")
		return nil, nil, nil, ""
	}
	root, err := safefs.Open(dataDir, safefs.Options{RequireStrongVisibility: true, Exclusive: true})
	if err != nil {
		collectorHealth.Set("profile-cpu", health.StatusDegraded, err.Error())
		return nil, nil, nil, ""
	}
	hardMax, err := runCPUHardMax(getenv)
	if err != nil {
		_ = root.Close()
		collectorHealth.Set("profile-cpu", health.StatusDegraded, err.Error())
		return nil, nil, nil, ""
	}
	owner, err := profilecapture.New(profilecapture.Options{
		Mode: profilecapture.ModeRun, Backend: profilecapture.RuntimeBackend{},
		Artifacts: profilecapture.NewFileArtifactFactory(root, cpuArtifactMaxBytes),
		Journal:   profilecapture.NewFileCompletionJournal(root),
		AfterPublish: func(artifact profilecapture.PublishedArtifact) error {
			web.PruneProfileArtifacts(dataDir, artifact.File)
			return nil
		},
		StartJoin: cpuStartJoinBudget, StopStall: cpuStopStallThreshold, HardMax: hardMax,
	})
	if err != nil {
		_ = root.Close()
		collectorHealth.Set("profile-cpu", health.StatusDegraded, err.Error())
		return nil, nil, nil, ""
	}
	bridge := &cpuWebBridge{owner: owner}
	return owner, bridge, root, string(profilecapture.ModeRun)
}

func runCPUHardMax(getenv func(string) string) (time.Duration, error) {
	value := getenv("ISUTOOLS_PPROF_SECONDS")
	if value == "" {
		return defaultRunCPUHardMax, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("invalid ISUTOOLS_PPROF_SECONDS=%q for run CPU mode", value)
	}
	if seconds > int(maxRunCPUHardMax/time.Second) {
		return 0, fmt.Errorf("ISUTOOLS_PPROF_SECONDS=%q exceeds run CPU maximum %v", value, maxRunCPUHardMax)
	}
	return time.Duration(seconds) * time.Second, nil
}

func cpuBoundaryWindow(window web.BoundaryWindow) profilecapture.BoundaryWindow {
	return profilecapture.BoundaryWindow{Min: window.Min, Max: window.Max, Spread: window.Spread}
}
