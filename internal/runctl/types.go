package runctl

import "time"

// LifecycleObserverBudget is the maximum time a lifecycle API waits for its
// observer to accept a termination event. The observer may continue in its own
// goroutine after this budget, but can never hold the run state machine.
const LifecycleObserverBudget = 10 * time.Millisecond

// RunState is a run's position in the lifecycle state machine. Transport
// layers copy these strings onto the wire verbatim, so the values are part of
// the contract.
type RunState string

const (
	// StateIdle means no run exists. It is never stored on a run record; it
	// describes the Controller when nothing is retained.
	StateIdle RunState = "idle"
	// StateStarting means the opening boundary is being taken. Owner: the
	// StartRun caller.
	StateStarting RunState = "starting"
	// StateStarted means measurement is in progress. No owner goroutine once
	// the previous generations have been drained.
	StateStarted RunState = "started"
	// StateFinishing means the closing boundary is fixed and a background
	// worker is draining, collecting and building the snapshot.
	StateFinishing RunState = "finishing"
	// StateFinished means an immutable snapshot exists.
	StateFinished RunState = "finished"
	// StateAcknowledged means the snapshot was handed over. Terminal.
	StateAcknowledged RunState = "acknowledged"
	// StateAborting means the run is fenced and its worker is being joined.
	StateAborting RunState = "aborting"
	// StateAborted means the run was abandoned and holds no snapshot. Terminal.
	StateAborted RunState = "aborted"
	// StateExpired means the snapshot was released by TTL. Terminal tombstone.
	StateExpired RunState = "expired"
)

// active reports whether a run in this state owns Controller resources and may
// never be evicted by the RetainedRuns rule.
func (s RunState) active() bool {
	switch s {
	case StateStarting, StateStarted, StateFinishing, StateAborting:
		return true
	}
	return false
}

// blocksNewRun reports whether a run in this state prevents a non-preempting
// StartRun. StateFinished blocks too: its snapshot has not been collected yet,
// and silently starting over would lose it.
func (s RunState) blocksNewRun() bool {
	return s.active() || s == StateFinished
}

// Validity is the data-quality axis, orthogonal to RunState. A run can be
// perfectly "finished" and completely untrustworthy; callers that compare runs
// must filter on this, not on the state.
type Validity string

const (
	// ValidityValid means every registered collector contributed a complete
	// interval within its boundary window.
	ValidityValid Validity = "valid"
	// ValidityPartial means some optional sections are missing but the
	// interval itself is usable.
	ValidityPartial Validity = "partial"
	// ValidityInvalid means the interval cannot be trusted. Advisors and diffs
	// must exclude it.
	ValidityInvalid Validity = "invalid"
)

// validityRank orders validity from best to worst so degradation is monotonic.
func validityRank(v Validity) int {
	switch v {
	case ValidityPartial:
		return 1
	case ValidityInvalid:
		return 2
	default:
		return 0
	}
}

// worse returns the more severe of two validities. Validity only ever
// degrades: an invalid run never becomes partial again.
func worse(a, b Validity) Validity {
	if validityRank(b) > validityRank(a) {
		return b
	}
	if a == "" {
		return ValidityValid
	}
	return a
}

// failValidity maps a collector failure onto the validity it causes.
func failValidity(required bool) Validity {
	if required {
		return ValidityInvalid
	}
	return ValidityPartial
}

// Epoch is the Controller's monotonic fencing token. It advances on every
// successful StartRun and on every AbortRun, which is what makes a worker
// belonging to an abandoned run structurally unable to publish.
type Epoch uint64

// Phase names one step of a boundary. It appears in CollectorBoundary so a
// failure can be attributed to the exact step that produced it.
type Phase string

const (
	// PhaseStartBoundary is BeginBoundary on generation collectors.
	PhaseStartBoundary Phase = "start-boundary"
	// PhaseStartBaseline is CaptureBaseline on baseline collectors.
	PhaseStartBaseline Phase = "start-baseline"
	// PhaseFinishFreeze is Freeze on generation collectors.
	PhaseFinishFreeze Phase = "finish-freeze"
	// PhaseFinishFinal is CaptureFinal on baseline collectors.
	PhaseFinishFinal Phase = "finish-final"
	// PhaseCollect is the background Drain then Collect step.
	PhaseCollect Phase = "collect"
)

// Collector kinds as they appear in CollectorBoundary.Kind.
const (
	KindGeneration = "generation"
	KindBaseline   = "baseline"
)

// Machine-readable failure codes. An empty code means the step succeeded.
// The set is closed: transports and advisors switch on these values.
const (
	// CodeNotCaptured means the collector was never sampled because the phase
	// budget ran out.
	CodeNotCaptured = "not-captured"
	// CodeDrainTimeout means in-flight work did not settle within DrainBudget.
	// Partial data still exists and is kept.
	CodeDrainTimeout = "drain-timeout"
	// CodeCollectFailed means the interval value could not be derived.
	CodeCollectFailed = "collect-failed"
	// CodeBoundaryFailed means the boundary operation itself returned an error.
	CodeBoundaryFailed = "boundary-failed"
	// CodeSpreadExceeded marks a collector whose measured time sits outside the
	// allowed boundary window.
	CodeSpreadExceeded = "spread-exceeded"
	// CodeContractViolation means the collector reported success without
	// committing, which is a collector bug rather than a runtime condition.
	CodeContractViolation = "contract-violation"
)

// Stable AckedBy values. This package owns the set; multi-host transports copy
// the field one-to-one and must not invent values.
const (
	// AckedByExplicit is a direct Ack call.
	AckedByExplicit = "explicit"
	// AckedBySave is the implicit acknowledgement performed by POST /save.
	AckedBySave = "save"
	// AckedByPreempt is the implicit acknowledgement of a finished run whose
	// successor started with Preempt. The snapshot is retained.
	AckedByPreempt = "preempt"
	// AckedByHub is an acknowledgement driven by a multi-host hub.
	AckedByHub = "hub"
	// AckedByLease is a self-acknowledgement after a peer ack lease expired, so
	// a vanished hub cannot wedge the system in "finished".
	AckedByLease = "lease"
)

// Stable AbortResult.Reason values.
const (
	// ReasonExplicit is an operator- or API-driven abort.
	ReasonExplicit = "explicit"
	// ReasonRequiredFailed is a required collector failing the opening boundary.
	ReasonRequiredFailed = "required-failed"
	// ReasonPreemptedBy is the prefix for "preempted-by:<runID>".
	ReasonPreemptedBy = "preempted-by:"
	// ReasonFinishLeaseExpired is the watchdog reclaiming a stuck worker.
	ReasonFinishLeaseExpired = "finish-lease-expired"
	// ReasonStartedTTL is the watchdog reclaiming a run nobody finished.
	ReasonStartedTTL = "started-ttl"
	// ReasonHubAbort is a multi-host hub aborting a peer's run.
	ReasonHubAbort = "hub-abort"
	// ReasonFinishAccepted marks the closing boundary accepted by FinishRun.
	ReasonFinishAccepted = "finish-accepted"
)

// RunTerminationEvent is emitted exactly once when a run stops accepting
// measured work: after an invalid opening boundary is committed, after a
// closing boundary is accepted, or immediately after an abort fence is
// installed. It deliberately carries the run's epoch, not the Controller's
// possibly advanced epoch, so process-wide resource owners can reject stale
// or duplicate requests.
type RunTerminationEvent struct {
	RunID      string
	Epoch      Epoch
	State      RunState
	Validity   Validity
	Reason     string
	BoundaryAt time.Time
}

// LifecycleObserver receives run termination boundaries. Implementations must
// return immediately; work that can block (notably runtime/pprof shutdown) has
// to be handed to an independently bounded coordinator. The Controller calls
// this interface without holding its mutex and contains observer panics.
type LifecycleObserver interface {
	OnRunTermination(RunTerminationEvent)
}

func terminationBoundary(window BoundaryWindow, fallback time.Time) time.Time {
	if !window.Max.IsZero() {
		return window.Max
	}
	return fallback
}

// Health keys this package reports. The set is fixed at four so that a health
// snapshot stays readable; new conditions reuse these keys with a different
// message rather than adding keys.
const (
	// HealthBoundarySpread reports a boundary window wider than its limit.
	HealthBoundarySpread = "runctl-boundary-spread"
	// HealthContractViolation reports a collector returning success without
	// committing its boundary.
	HealthContractViolation = "runctl-contract-violation"
	// HealthWorkerDetached reports an abort that could not join its worker.
	HealthWorkerDetached = "runctl-worker-detached"
	// HealthLeaseExpired reports a finishing worker killed by FinishLease.
	HealthLeaseExpired = "runctl-lease-expired"
)

// GenerationHandle is an immutable reference to a closed or frozen generation.
// Holding one gives access to fixed data only; the collector's mutable current
// generation is never reachable through it.
type GenerationHandle struct {
	RunID     string
	Epoch     Epoch
	Collector string
	Gen       uint64

	token any // collector-internal generation reference
}

// NewGenerationHandle builds a handle. Collectors live in other packages, so
// this constructor is the only way to populate the unexported token; that
// keeps the token opaque to everyone except the collector that created it.
func NewGenerationHandle(runID string, ep Epoch, collector string, gen uint64, token any) GenerationHandle {
	return GenerationHandle{RunID: runID, Epoch: ep, Collector: collector, Gen: gen, token: token}
}

// Token returns the collector-internal generation reference. Only the
// collector that created the handle may interpret it.
func (h GenerationHandle) Token() any { return h.token }

// Zero reports whether the handle refers to nothing.
func (h GenerationHandle) Zero() bool { return h.token == nil && h.Collector == "" }

// BaselineHandle is an immutable sample taken at a boundary. The sample is
// carried inside the handle rather than left in the collector, because
// Collect(base, final) must be able to build an interval from fixed values
// alone — reading the collector's live state at snapshot time is exactly the
// bug this design removes.
type BaselineHandle struct {
	RunID     string
	Epoch     Epoch
	Collector string
	Phase     Phase
	SampledAt time.Time

	sample any // deep-copied at SampledAt
}

// NewBaselineHandle builds a handle around an already-copied sample. The
// caller must never mutate the value it passes in afterwards: handles are
// copied and shared, so a later mutation would be observed by every holder and
// would silently change an interval that was supposed to be frozen. Prefer a
// value type over a pointer for exactly that reason.
func NewBaselineHandle(runID string, ep Epoch, collector string, phase Phase, sampledAt time.Time, sample any) BaselineHandle {
	return BaselineHandle{
		RunID:     runID,
		Epoch:     ep,
		Collector: collector,
		Phase:     phase,
		SampledAt: sampledAt,
		sample:    sample,
	}
}

// Sample returns the frozen sample this handle carries. It is the only
// official way for a BaselineCollector to reach the values it needs inside
// Collect(base, final); reaching into the collector's own fields instead would
// violate the "fixed values only" contract even when it happens to work.
//
// The returned value must be treated as read-only.
func (h BaselineHandle) Sample() any { return h.sample }

// Zero reports whether the handle carries no sample.
func (h BaselineHandle) Zero() bool { return h.sample == nil && h.Collector == "" }

// BoundaryResult is what a generation collector returns from a boundary
// operation. It is returned even on error, never as a zero value, because the
// Controller must know whether the switch took effect before it decides
// whether the collector's data can still be used.
type BoundaryResult struct {
	Handle GenerationHandle
	// At is the measured moment of the swap or freeze.
	At time.Time
	// Committed states whether the switch is in effect for this run ID. It is
	// a state predicate, not a "did this call do it" flag, so a retry of the
	// same (runID, epoch) returns the same value.
	Committed bool
}

// SampleResult is what a baseline collector returns from a sampling
// operation. Like BoundaryResult it is returned even on error.
type SampleResult struct {
	Handle BaselineHandle
	// At is the measured moment of sampling and equals Handle.SampledAt.
	At time.Time
	// Committed states whether the sample is fixed for this run ID.
	Committed bool
}

// BoundaryWindow is the measured width of a boundary. Boundaries are intervals
// rather than instants, so the width is recorded and judged instead of
// assumed away.
type BoundaryWindow struct {
	Min    time.Time     `json:"min"`
	Max    time.Time     `json:"max"`
	Spread time.Duration `json:"spread"`
}

// CollectorBoundary is one collector's record of one boundary step.
type CollectorBoundary struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Required  bool      `json:"required"`
	Phase     Phase     `json:"phase"`
	At        time.Time `json:"at"`
	Committed bool      `json:"committed"`
	// Code is a stable machine-readable code; empty means success.
	Code string `json:"code,omitempty"`
	// Err is the human-readable original message.
	Err string `json:"err,omitempty"`
	// Dropped marks the section as excluded from the snapshot.
	Dropped bool `json:"dropped,omitempty"`
}

// Registration describes how a collector participates in runs.
type Registration struct {
	// Name identifies the snapshot section this collector fills.
	Name string
	// Required marks a collector whose failure invalidates the run rather than
	// merely degrading it.
	Required bool
	// SerialOnly excludes the collector from the parallel baseline group. It
	// widens the boundary window, so it is only for collectors that are
	// genuinely unsafe to sample concurrently.
	SerialOnly bool
}

// StartRunOptions configures StartRun.
type StartRunOptions struct {
	// Nonce makes the call idempotent. Empty means the Controller mints one.
	Nonce string
	// Preempt aborts an in-flight run instead of failing with ErrRunActive.
	// This is how "the last initialize wins" is made deterministic.
	Preempt bool
	// Reason records who asked: "api", "initialize", "http", "hub".
	Reason string
	// Trigger is recorded on the run as its reset trigger.
	Trigger string
}

// StartResult is the immutable record of an opening boundary. err == nil with
// a degraded Validity is the normal way collector failures are reported;
// callers must inspect Validity rather than only err.
type StartResult struct {
	RunID            string
	Nonce            string
	Epoch            Epoch
	State            RunState
	Validity         Validity
	Collectors       []CollectorBoundary
	GenerationWindow BoundaryWindow
	BoundaryWindow   BoundaryWindow
	// PreemptedRunID names the run this one displaced, if any.
	PreemptedRunID string
	StartedAt      time.Time
	// CPUProfileStart is optional transport-neutral evidence added by the
	// embedding measurement after the run boundary opens. The controller does
	// not interpret it.
	CPUProfileStart *ProfileStartEvidence
	// TraceStart is optional transport-neutral evidence for a run-aligned
	// execution trace start attempt.
	TraceStart *ProfileStartEvidence
}

// ProfileStartEvidence makes an optional profiler start attempt visible to
// direct ResetNow callers without coupling runctl to a profiler package.
type ProfileStartEvidence struct {
	CaptureID string
	State     string
	Code      string
}

// clone returns a defensively copied value so a caller cannot mutate the
// Collectors backing array that the Controller replays to the next caller.
func (r StartResult) clone() StartResult {
	out := r
	out.Collectors = cloneBoundaries(r.Collectors)
	if r.CPUProfileStart != nil {
		copy := *r.CPUProfileStart
		out.CPUProfileStart = &copy
	}
	if r.TraceStart != nil {
		copy := *r.TraceStart
		out.TraceStart = &copy
	}
	return out
}

// FinishAccepted is the immutable record of a closing boundary. It is returned
// as soon as the boundary is fixed; draining and snapshot building continue in
// the background.
type FinishAccepted struct {
	RunID            string
	Epoch            Epoch
	Validity         Validity
	Collectors       []CollectorBoundary
	GenerationWindow BoundaryWindow
	BoundaryWindow   BoundaryWindow
	AcceptedAt       time.Time
}

// clone returns a defensively copied value.
func (r FinishAccepted) clone() FinishAccepted {
	out := r
	out.Collectors = cloneBoundaries(r.Collectors)
	return out
}

// AbortResult is the immutable record of an abort.
type AbortResult struct {
	RunID string
	Epoch Epoch
	// Reason is one of the stable Reason* values.
	Reason string
	// Detached reports that the worker outlived AbortJoinBudget. Correctness is
	// unaffected because the run is already fenced; only resource release is
	// delayed.
	Detached  bool
	AbortedAt time.Time
	// Partial lists collectors that had already switched when the run died.
	Partial []string
}

// clone returns a defensively copied value.
func (r AbortResult) clone() AbortResult {
	out := r
	if r.Partial != nil {
		out.Partial = append([]string(nil), r.Partial...)
	}
	return out
}

// RunStatus is the queryable state of a run.
type RunStatus struct {
	RunID    string   `json:"run_id"`
	Epoch    Epoch    `json:"epoch"`
	State    RunState `json:"state"`
	Validity Validity `json:"validity"`
	Reason   string   `json:"reason,omitempty"`
	// AckedBy is one of the stable AckedBy* values.
	AckedBy  string    `json:"acked_by,omitempty"`
	Detached bool      `json:"detached,omitempty"`
	Since    time.Time `json:"since"`
}

// Snapshot is a run's immutable result. Sections holds each collector's
// interval value keyed by collector name; the concrete types belong to the
// collectors, so the transport layer decides how to serialize them.
type Snapshot struct {
	RunID            string
	Epoch            Epoch
	Validity         Validity
	Trigger          string
	Sections         map[string]any
	Collectors       []CollectorBoundary
	GenerationWindow BoundaryWindow
	BoundaryWindow   BoundaryWindow
	StartedAt        time.Time
	FinishedAt       time.Time
}

// cloneBoundaries copies a boundary slice. Boundaries are flat value structs,
// so a shallow element copy is a deep copy.
func cloneBoundaries(in []CollectorBoundary) []CollectorBoundary {
	if in == nil {
		return nil
	}
	return append([]CollectorBoundary(nil), in...)
}
