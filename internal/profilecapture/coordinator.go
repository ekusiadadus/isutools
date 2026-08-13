package profilecapture

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

const defaultLedgerSize = 64

type Options struct {
	Mode      Mode
	Backend   Backend
	Artifacts ArtifactFactory
	Journal   CompletionJournal
	// AfterPublish runs after completion evidence has settled and before the
	// process-wide owner is released. It is intended for bounded local
	// retention and must not perform network I/O.
	AfterPublish func(PublishedArtifact) error
	StartJoin    time.Duration
	StopStall    time.Duration
	HardMax      time.Duration
	Now          func() time.Time
}

type captureKey struct {
	runID string
	epoch uint64
}

type session struct {
	key              captureKey
	req              StartRequest
	captureID        string
	state            State
	code             string
	err              error
	artifact         Artifact
	published        PublishedArtifact
	sidecar          CompletionAttachment
	coverage         CompletionAttachment
	labels           *LabelScope
	startRequestedAt time.Time
	startCompletedAt time.Time
	stopRequestedAt  time.Time
	stopCompletedAt  time.Time
	runBoundaryAt    time.Time
	completionSeq    uint64
	journalErr       error

	started       bool
	stopRequested *StopRequest
	stopStarted   bool
	startDone     chan struct{}
	done          chan struct{}
	startOnce     sync.Once
	doneOnce      sync.Once
	journalMu     sync.Mutex
	hardTimer     *time.Timer
	stallTimer    *time.Timer
}

type Coordinator struct {
	mode         Mode
	backend      Backend
	artifacts    ArtifactFactory
	journal      CompletionJournal
	afterPublish func(PublishedArtifact) error
	startJoin    time.Duration
	stopStall    time.Duration
	hardMax      time.Duration
	now          func() time.Time

	mu             sync.Mutex
	active         *session
	sessions       map[captureKey]*session
	order          []captureKey
	tombstones     map[captureKey]StopRequest
	tombstoneOrder []captureKey
	closed         bool
}

func New(o Options) (*Coordinator, error) {
	if o.Mode != ModeRun {
		return nil, fmt.Errorf("profilecapture: coordinator mode %q is not implemented", o.Mode)
	}
	if o.Backend == nil || o.Artifacts == nil {
		return nil, errors.New("profilecapture: backend and artifact factory are required")
	}
	if o.StartJoin <= 0 || o.StopStall <= 0 || o.HardMax <= 0 {
		return nil, errors.New("profilecapture: positive start, stop, and hard-max budgets are required")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Coordinator{
		mode: o.Mode, backend: o.Backend, artifacts: o.Artifacts, journal: o.Journal, afterPublish: o.AfterPublish,
		startJoin: o.StartJoin, stopStall: o.StopStall, hardMax: o.HardMax, now: now,
		sessions: make(map[captureKey]*session), tombstones: make(map[captureKey]StopRequest),
	}, nil
}

func (c *Coordinator) StartRun(ctx context.Context, req StartRequest) StartResult {
	if code := validateStart(req); code != "" {
		return StartResult{RunID: req.RunID, Epoch: req.Epoch, State: StateSkipped, Code: code}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := captureKey{runID: req.RunID, epoch: req.Epoch}

	var handoff <-chan struct{}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return StartResult{RunID: req.RunID, Epoch: req.Epoch, State: StateSkipped, Code: CodeCPUBusy}
	}
	if _, terminal := c.tombstones[key]; terminal {
		c.mu.Unlock()
		return StartResult{RunID: req.RunID, Epoch: req.Epoch, State: StateSkipped, Code: CodeTerminalFenced}
	}
	if existing := c.sessions[key]; existing != nil {
		if existing.state == StateSkipped && existing.code == CodeCPUBusy && c.active == nil {
			delete(c.sessions, key)
			c.removeOrderKeyLocked(key)
		} else {
			result := c.startResultLocked(existing)
			if !reflect.DeepEqual(existing.req, req) {
				result.State, result.Code = StateSkipped, CodeReplayMismatch
			} else if existing.state != StateSkipped {
				result.State = StateReplayed
			}
			c.mu.Unlock()
			return result
		}
	}
	if c.active != nil {
		if c.active.stopRequested != nil {
			handoff = c.active.done
		} else {
			result := c.recordSkippedLocked(req, CodeCPUBusy)
			c.mu.Unlock()
			return result
		}
	}
	c.mu.Unlock()
	if handoff != nil {
		timer := time.NewTimer(c.startJoin)
		defer timer.Stop()
		select {
		case <-handoff:
			return c.StartRun(ctx, req)
		case <-ctx.Done():
		case <-timer.C:
		}
		c.mu.Lock()
		if existing := c.sessions[key]; existing != nil {
			result := c.startResultLocked(existing)
			c.mu.Unlock()
			return result
		}
		result := c.recordSkippedLocked(req, CodeCPUBusy)
		c.mu.Unlock()
		return result
	}

	c.mu.Lock()
	if c.active != nil {
		c.mu.Unlock()
		return c.StartRun(ctx, req)
	}
	s := &session{
		key: key, req: req, captureID: newSessionCaptureID(c.now()), state: StateStarting,
		startRequestedAt: c.now(),
		startDone:        make(chan struct{}), done: make(chan struct{}),
	}
	s.labels = newLabelScope(s.captureID)
	c.active = s
	c.sessions[key] = s
	c.order = append(c.order, key)
	c.pruneLocked()
	c.mu.Unlock()

	artifact, err := c.artifacts.New(req, s.captureID)
	if err != nil {
		c.failStart(s, CodeArtifactCreateFailed, err)
		return c.startResult(s)
	}
	c.mu.Lock()
	s.artifact = artifact
	terminal := s.stopRequested != nil
	c.mu.Unlock()
	if terminal {
		_ = artifact.Abort()
		c.failStart(s, CodeTerminalFenced, errors.New("run terminated before CPU profiler start"))
		return c.startResult(s)
	}

	go func() {
		err := c.backend.StartCPUProfile(artifact.Writer())
		c.finishStart(s, err)
	}()

	timer := time.NewTimer(c.startJoin)
	defer timer.Stop()
	select {
	case <-s.startDone:
	case <-timer.C:
		c.mu.Lock()
		if s.state == StateStarting {
			s.state = StateStartWedged
			s.code = CodeStartWedged
		}
		c.mu.Unlock()
	}
	return c.startResult(s)
}

func (c *Coordinator) removeOrderKeyLocked(key captureKey) {
	for i, candidate := range c.order {
		if candidate == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func (c *Coordinator) recordSkippedLocked(req StartRequest, code string) StartResult {
	key := captureKey{runID: req.RunID, epoch: req.Epoch}
	now := c.now()
	s := &session{
		key: key, req: req, captureID: newSessionCaptureID(now), state: StateSkipped, code: code,
		startRequestedAt: now, startDone: make(chan struct{}), done: make(chan struct{}),
	}
	close(s.startDone)
	close(s.done)
	c.sessions[key] = s
	c.order = append(c.order, key)
	c.pruneLocked()
	return c.startResultLocked(s)
}

func validateStart(req StartRequest) string {
	if req.RunID == "" || req.Epoch == 0 || req.BoundaryStart.IsZero() {
		return CodeInvalidRequest
	}
	if req.State != "started" {
		return CodeRunNotStarted
	}
	if req.Validity != "valid" && req.Validity != "partial" {
		return CodeRunInvalid
	}
	return ""
}

func (c *Coordinator) finishStart(s *session, startErr error) {
	if startErr != nil {
		_ = s.artifact.Abort()
		c.failStart(s, CodeStartFailed, startErr)
		return
	}

	c.mu.Lock()
	s.started = true
	s.startCompletedAt = c.now()
	if s.state == StateStartWedged && s.stopRequested == nil {
		req := StopRequest{
			RunID: s.key.runID, Epoch: s.key.epoch, State: "aborted", Validity: "invalid",
			Reason: ReasonStartWedged, BoundaryAt: c.now(),
		}
		s.stopRequested = &req
		s.stopRequestedAt = c.now()
		s.state = StateStopping
	}
	if s.stopRequested != nil {
		c.beginStopLocked(s)
	} else {
		s.state = StateCapturing
		s.code = ""
		s.hardTimer = time.AfterFunc(c.hardMax, func() {
			c.RequestStop(StopRequest{
				RunID: s.key.runID, Epoch: s.key.epoch, State: "started", Validity: s.req.Validity,
				Reason: ReasonHardMax, BoundaryAt: c.now(),
			})
		})
	}
	s.startOnce.Do(func() { close(s.startDone) })
	c.mu.Unlock()
}

func (c *Coordinator) failStart(s *session, code string, err error) {
	c.mu.Lock()
	s.labels.Seal()
	s.state, s.code, s.err = StateFailed, code, err
	if c.active == s {
		c.active = nil
	}
	s.startOnce.Do(func() { close(s.startDone) })
	s.doneOnce.Do(func() { close(s.done) })
	c.mu.Unlock()
}

func (c *Coordinator) RequestStop(req StopRequest) StopTicket {
	if req.RunID == "" || req.Epoch == 0 || req.Reason == "" || req.BoundaryAt.IsZero() {
		return StopTicket{RunID: req.RunID, Epoch: req.Epoch, State: StateSkipped, Code: CodeInvalidRequest, BoundaryAt: req.BoundaryAt}
	}
	key := captureKey{runID: req.RunID, epoch: req.Epoch}
	c.mu.Lock()
	c.recordTombstoneLocked(key, req)
	target := c.sessions[key]
	if target != nil && noteRunBoundaryLocked(target, req) && c.journal != nil &&
		(target.state == StatePublished || target.state == StateOrphaned) && target.published.File != "" {
		target.completionSeq++
		record := c.completionRecordLocked(target, CompletionPhaseCoverage)
		go c.recordCompletion(target, record)
	}
	s := c.active
	if s == nil {
		if target != nil {
			ticket := c.stopTicketLocked(target, CodeStopAlreadyRequested)
			c.mu.Unlock()
			return ticket
		}
		c.mu.Unlock()
		return StopTicket{RunID: req.RunID, Epoch: req.Epoch, State: StateSkipped, Code: CodeNoActiveCapture, BoundaryAt: req.BoundaryAt}
	}
	if s.key != key {
		c.mu.Unlock()
		return StopTicket{RunID: req.RunID, Epoch: req.Epoch, State: StateSkipped, Code: CodeStaleEpoch, BoundaryAt: req.BoundaryAt}
	}
	if s.stopRequested != nil {
		ticket := c.stopTicketLocked(s, CodeStopAlreadyRequested)
		c.mu.Unlock()
		return ticket
	}
	copyReq := req
	s.stopRequested = &copyReq
	s.stopRequestedAt = c.now()
	s.labels.Seal()
	s.state = StateStopping
	if s.started {
		c.beginStopLocked(s)
	}
	ticket := c.stopTicketLocked(s, "")
	c.mu.Unlock()
	return ticket
}

func (c *Coordinator) beginStopLocked(s *session) {
	if s.stopStarted {
		return
	}
	s.stopStarted = true
	s.labels.Seal()
	if s.hardTimer != nil {
		s.hardTimer.Stop()
	}
	s.stallTimer = time.AfterFunc(c.stopStall, func() {
		c.mu.Lock()
		if s.state == StateStopping {
			s.state, s.code = StateWedged, CodeStopWedged
		}
		c.mu.Unlock()
	})
	go c.stopSession(s)
}

func (c *Coordinator) stopSession(s *session) {
	c.backend.StopCPUProfile()
	published, err := s.artifact.Publish()
	c.mu.Lock()
	s.stopCompletedAt = c.now()
	if s.stallTimer != nil {
		s.stallTimer.Stop()
	}
	if err != nil && !published.Visible {
		s.state, s.code, s.err = StateFailed, CodeArtifactCreateFailed, err
	} else {
		s.published = published
		if orphanReason(s.stopRequested.Reason) {
			s.state = StateOrphaned
		} else {
			s.state = StatePublished
		}
		s.code = ""
		if err != nil {
			s.err = errors.Join(s.err, err)
		}
	}
	var record *CompletionRecord
	if published.Visible && c.journal != nil {
		value := c.completionRecordLocked(s, CompletionPhaseInitial)
		record = &value
	}
	c.mu.Unlock()
	if record != nil {
		c.recordCompletion(s, *record)
	} else if published.Visible {
		c.runAfterPublish(s)
	}
	c.mu.Lock()
	if c.active == s {
		c.active = nil
	}
	s.doneOnce.Do(func() { close(s.done) })
	c.mu.Unlock()
}

func orphanReason(reason string) bool {
	return reason == ReasonStartWedged || reason == "required-failed" || reason == "explicit" ||
		reason == "hub-abort" || reason == "started-ttl" || len(reason) >= len("preempted-by:") && reason[:len("preempted-by:")] == "preempted-by:"
}

func noteRunBoundaryLocked(s *session, req StopRequest) bool {
	if req.Reason == ReasonHardMax || req.Reason == ReasonStartWedged || req.BoundaryAt.IsZero() {
		return false
	}
	if s.runBoundaryAt.IsZero() || req.BoundaryAt.Before(s.runBoundaryAt) {
		s.runBoundaryAt = req.BoundaryAt
		return true
	}
	return false
}

func (c *Coordinator) Await(ticket StopTicket, ctx context.Context) Status {
	key := captureKey{runID: ticket.RunID, epoch: ticket.Epoch}
	c.mu.Lock()
	s := c.sessions[key]
	if s == nil || s.captureID != ticket.CaptureID {
		c.mu.Unlock()
		return Status{RunID: ticket.RunID, CaptureID: ticket.CaptureID, Epoch: ticket.Epoch, State: StateSkipped, Code: CodeNoActiveCapture}
	}
	done := s.done
	c.mu.Unlock()
	select {
	case <-done:
		return c.status(s, nil)
	case <-ctx.Done():
		return c.status(s, ctx.Err())
	}
}

// Status returns the current immutable projection for one run/epoch. It never
// falls back to the active session of another epoch.
func (c *Coordinator) Status(runID string, epoch uint64) (Status, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sessions[captureKey{runID: runID, epoch: epoch}]
	if s == nil {
		return Status{}, false
	}
	return Status{
		RunID: s.key.runID, CaptureID: s.captureID, Epoch: s.key.epoch,
		State: s.state, Code: s.code, Artifact: s.published, Sidecar: s.sidecar, Coverage: s.coverage,
		Err:           errors.Join(s.err, s.journalErr),
		BoundaryStart: s.req.BoundaryStart, BoundaryFinish: s.runBoundaryAt,
		StartRequestedAt: s.startRequestedAt, StartCompletedAt: s.startCompletedAt,
		StopRequestedAt: s.stopRequestedAt, StopCompletedAt: s.stopCompletedAt,
		StopReason:  stopReason(s),
		RunSpan:     nonNegativeBetween(s.req.BoundaryStart, s.runBoundaryAt),
		CaptureSpan: nonNegativeBetween(s.startCompletedAt, s.stopCompletedAt),
		HeadLoss:    nonNegativeBetween(s.req.BoundaryStart, s.startCompletedAt),
		TailExcess:  positiveDifference(s.stopCompletedAt, s.runBoundaryAt),
		// StopCPUProfile does not expose the instant at which sampling stops.
		// The request instant is therefore the conservative boundary for a
		// possible missing tail; completion is used separately for excess.
		TailLoss: positiveDifference(s.runBoundaryAt, s.stopRequestedAt),
		Complete: captureComplete(s),
	}, true
}

// ActiveLabelScope returns a scope only while the runtime profiler is actively
// capturing. A caller that already holds an older scope is still fenced by
// Seal when stop is requested.
func (c *Coordinator) ActiveLabelScope() *LabelScope {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || c.active.state != StateCapturing {
		return nil
	}
	return c.active.labels
}

func (c *Coordinator) LabelDictionary(runID string, epoch uint64) (LabelDictionary, bool) {
	c.mu.Lock()
	s := c.sessions[captureKey{runID: runID, epoch: epoch}]
	c.mu.Unlock()
	if s == nil || s.labels == nil {
		return LabelDictionary{}, false
	}
	return s.labels.Dictionary(runID, epoch), true
}

func (c *Coordinator) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	s := c.active
	c.mu.Unlock()
	if s == nil {
		return
	}
	ticket := c.RequestStop(StopRequest{
		RunID: s.key.runID, Epoch: s.key.epoch, State: "aborted", Validity: "invalid",
		Reason: "explicit", BoundaryAt: c.now(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), c.stopStall)
	defer cancel()
	_ = c.Await(ticket, ctx)
}

func (c *Coordinator) startResult(s *session) StartResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startResultLocked(s)
}

func (c *Coordinator) startResultLocked(s *session) StartResult {
	return StartResult{RunID: s.key.runID, CaptureID: s.captureID, Epoch: s.key.epoch, State: s.state, Code: s.code}
}

func (c *Coordinator) stopTicketLocked(s *session, code string) StopTicket {
	boundary := time.Time{}
	if s.stopRequested != nil {
		boundary = s.stopRequested.BoundaryAt
	}
	return StopTicket{RunID: s.key.runID, CaptureID: s.captureID, Epoch: s.key.epoch, State: s.state, Code: code, BoundaryAt: boundary}
}

func (c *Coordinator) status(s *session, awaitErr error) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := errors.Join(s.err, s.journalErr)
	if awaitErr != nil {
		err = awaitErr
	}
	return Status{
		RunID: s.key.runID, CaptureID: s.captureID, Epoch: s.key.epoch,
		State: s.state, Code: s.code, Artifact: s.published, Sidecar: s.sidecar, Coverage: s.coverage, Err: err,
		BoundaryStart: s.req.BoundaryStart, BoundaryFinish: s.runBoundaryAt,
		StartRequestedAt: s.startRequestedAt, StartCompletedAt: s.startCompletedAt,
		StopRequestedAt: s.stopRequestedAt, StopCompletedAt: s.stopCompletedAt,
		StopReason:  stopReason(s),
		RunSpan:     nonNegativeBetween(s.req.BoundaryStart, s.runBoundaryAt),
		CaptureSpan: nonNegativeBetween(s.startCompletedAt, s.stopCompletedAt),
		HeadLoss:    nonNegativeBetween(s.req.BoundaryStart, s.startCompletedAt),
		TailExcess:  positiveDifference(s.stopCompletedAt, s.runBoundaryAt),
		TailLoss:    positiveDifference(s.runBoundaryAt, s.stopRequestedAt),
		Complete:    captureComplete(s),
	}
}

func (c *Coordinator) completionRecordLocked(s *session, phase string) CompletionRecord {
	coverage := CaptureCoverage{
		BoundaryStart: s.req.BoundaryStart, BoundaryFinish: s.runBoundaryAt,
		StartRequestedAt: s.startRequestedAt, StartCompletedAt: s.startCompletedAt,
		StopRequestedAt: s.stopRequestedAt, StopCompletedAt: s.stopCompletedAt,
		StopReason:  stopReason(s),
		RunSpan:     nonNegativeBetween(s.req.BoundaryStart, s.runBoundaryAt),
		CaptureSpan: nonNegativeBetween(s.startCompletedAt, s.stopCompletedAt),
		HeadLoss:    nonNegativeBetween(s.req.BoundaryStart, s.startCompletedAt),
		TailExcess:  positiveDifference(s.stopCompletedAt, s.runBoundaryAt),
		TailLoss:    positiveDifference(s.runBoundaryAt, s.stopRequestedAt),
		Complete:    captureComplete(s),
	}
	return CompletionRecord{
		Schema: CompletionSchemaV1, Phase: phase, Sequence: s.completionSeq,
		RunID: s.key.runID, Epoch: s.key.epoch, CaptureID: s.captureID,
		State: s.state, Code: s.code, Profile: s.published,
		Labels: s.labels.Dictionary(s.key.runID, s.key.epoch), Coverage: coverage,
	}
}

func (c *Coordinator) recordCompletion(s *session, record CompletionRecord) {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	attachment, err := c.journal.Record(record)
	c.mu.Lock()
	if attachment.Visible {
		if record.Phase == CompletionPhaseInitial {
			s.sidecar = attachment
		} else if record.Phase == CompletionPhaseCoverage && attachment.Sequence >= s.coverage.Sequence {
			s.coverage = attachment
		}
	}
	if err != nil {
		s.journalErr = errors.Join(s.journalErr, err)
	}
	c.mu.Unlock()
	c.runAfterPublish(s)
}

func (c *Coordinator) runAfterPublish(s *session) {
	if c.afterPublish == nil || !s.published.Visible {
		return
	}
	if err := c.afterPublish(s.published); err != nil {
		c.mu.Lock()
		s.journalErr = errors.Join(s.journalErr, err)
		c.mu.Unlock()
	}
}

func stopReason(s *session) string {
	if s.stopRequested == nil {
		return ""
	}
	return s.stopRequested.Reason
}

func nonNegativeBetween(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

func positiveDifference(later, earlier time.Time) time.Duration {
	if later.IsZero() || earlier.IsZero() || !later.After(earlier) {
		return 0
	}
	return later.Sub(earlier)
}

func captureComplete(s *session) bool {
	return s.state == StatePublished && stopReason(s) == "finish-accepted" &&
		!s.runBoundaryAt.IsZero() && !s.stopRequestedAt.Before(s.runBoundaryAt)
}

func (c *Coordinator) pruneLocked() {
	for len(c.order) > defaultLedgerSize {
		key := c.order[0]
		if c.active != nil && c.active.key == key {
			return
		}
		delete(c.sessions, key)
		delete(c.tombstones, key)
		c.order = c.order[1:]
	}
}

func (c *Coordinator) recordTombstoneLocked(key captureKey, request StopRequest) {
	if _, exists := c.tombstones[key]; !exists {
		c.tombstoneOrder = append(c.tombstoneOrder, key)
	}
	c.tombstones[key] = request
	for len(c.tombstoneOrder) > defaultLedgerSize {
		oldest := c.tombstoneOrder[0]
		c.tombstoneOrder = c.tombstoneOrder[1:]
		if c.active != nil && c.active.key == oldest {
			c.tombstoneOrder = append(c.tombstoneOrder, oldest)
			continue
		}
		delete(c.tombstones, oldest)
	}
}

func newCaptureID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	// Label provenance never treats this ID as an authentication secret, but
	// the wire grammar and no-replace filename contract still require a unique
	// 128-bit lowercase hexadecimal value if the platform RNG is unavailable.
	fallback := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", time.Now().UnixNano(), fallbackIDCounter.Add(1))))
	return hex.EncodeToString(fallback[:16])
}

// newSessionCaptureID keeps 80 bits of cryptographic randomness while making
// the first 48 bits a big-endian Unix-millisecond key. Raw artifact retention
// can therefore order secret-free cpu_<capture-id> groups together with the
// timestamp-named legacy groups without trusting mutable file mtimes.
func newSessionCaptureID(now time.Time) string {
	millis := now.UnixMilli()
	if millis < 0 || uint64(millis) > (uint64(1)<<48)-1 {
		return newCaptureID()
	}
	var raw [16]byte
	value := uint64(millis)
	for index := 5; index >= 0; index-- {
		raw[index] = byte(value)
		value >>= 8
	}
	if _, err := rand.Read(raw[6:]); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", now.UnixNano(), fallbackIDCounter.Add(1))))
		copy(raw[6:], sum[:10])
	}
	return hex.EncodeToString(raw[:])
}

var fallbackIDCounter atomic.Uint64
