package multihost

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/internal/runctl"
)

var (
	ErrPeerDisabled = errors.New("isutools: peer disabled")
	ErrPeerConfig   = errors.New("isutools: invalid peer configuration")
)

type PeerOptions struct {
	Enabled      bool
	Token        string
	Role         string
	Form         string
	AgentID      string
	MaxBytes     int64
	Sections     []string
	Capabilities []string
	Identity     hoststats.Identity
	CgroupScope  string
	Targets      []TargetSummaryDTO
	Controller   *runctl.Controller
	Snapshot     func(*runctl.Snapshot) map[string]any
	Now          func() time.Time
	StartedAt    time.Time
}

type runRecord struct {
	runID, localRunID, nonce string
	origin, state, validity  string
	epoch                    uint64
	start                    StartResultDTO
	finish                   *FinishAcceptedDTO
	abort                    *AbortResultDTO
	snapshot                 *LocalSnapshot
	snapshotBytes            int64
	leaseExpiry              time.Time
	expiryReason, ackedBy    string
	updatedAt                time.Time
}

type nonceRecord struct {
	runID string
	at    time.Time
}

type Peer struct {
	opt             PeerOptions
	ctrl            *runctl.Controller
	ownedController bool
	mutation        sync.Mutex
	mu              sync.Mutex
	activeRunID     string
	runs            map[string]*runRecord
	order           []string
	nonces          map[string]nonceRecord
	nonceOrder      []string
	stop            chan struct{}
	stopOnce        sync.Once
}

func NewPeer(o PeerOptions) (*Peer, error) {
	if !o.Enabled {
		return nil, ErrPeerDisabled
	}
	if len(o.Token) < 32 || !validRole(o.Role) || !validAgentID(o.AgentID) || (o.Form != "embedded" && o.Form != "agent") {
		return nil, ErrPeerConfig
	}
	if o.MaxBytes <= 0 || o.MaxBytes > PeerSelfCapBytes {
		o.MaxBytes = PeerSelfCapBytes
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.StartedAt.IsZero() {
		o.StartedAt = o.Now().UTC()
	}
	if err := validateNames(o.Sections); err != nil {
		return nil, err
	}
	if err := validateNames(o.Capabilities); err != nil {
		return nil, err
	}
	o.Sections = sortedUnique(o.Sections)
	o.Capabilities = sortedUnique(o.Capabilities)
	p := &Peer{opt: o, ctrl: o.Controller, runs: map[string]*runRecord{}, nonces: map[string]nonceRecord{}, stop: make(chan struct{})}
	if p.ctrl == nil {
		var err error
		p.ctrl, err = runctl.New(runctl.Options{})
		if err != nil {
			return nil, err
		}
		p.ownedController = true
	}
	go p.watchLeases()
	return p, nil
}

func (p *Peer) Close() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() { close(p.stop) })
	if p.ownedController {
		p.ctrl.Close()
	}
}

func (p *Peer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		p.writeError(w, http.StatusUnauthorized, "unauthorized", "", nil)
		return
	}
	if r.URL.Path == "/peer/info" {
		if r.Method != http.MethodGet {
			p.writeError(w, http.StatusMethodNotAllowed, "malformed_request", "", nil)
			return
		}
		p.writeJSON(w, http.StatusOK, p.Info())
		return
	}
	if r.URL.Path == "/peer/runs" {
		if r.Method != http.MethodPost {
			p.writeError(w, http.StatusMethodNotAllowed, "malformed_request", "", nil)
			return
		}
		p.start(w, r)
		return
	}
	prefix := "/peer/runs/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		p.writeError(w, http.StatusNotFound, "unknown_run", "", nil)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) > 2 || !validID(parts[0]) {
		p.writeError(w, http.StatusBadRequest, "invalid_run_id", "", nil)
		return
	}
	runID := parts[0]
	action := "status"
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "status":
		if r.Method != http.MethodGet {
			p.writeError(w, http.StatusMethodNotAllowed, "malformed_request", runID, nil)
			return
		}
		p.status(w, runID)
	case "lease":
		if r.Method != http.MethodPost || !p.decodeEmpty(w, r, runID) {
			return
		}
		p.lease(w, runID)
	case "finish":
		if r.Method != http.MethodPost || !p.decodeEmpty(w, r, runID) {
			return
		}
		p.finishRun(w, r, runID)
	case "snapshot":
		if r.Method != http.MethodGet {
			p.writeError(w, http.StatusMethodNotAllowed, "malformed_request", runID, nil)
			return
		}
		p.snapshot(w, r, runID)
	case "abort":
		if r.Method != http.MethodPost {
			p.writeError(w, http.StatusMethodNotAllowed, "malformed_request", runID, nil)
			return
		}
		p.abortRun(w, r, runID)
	case "ack":
		if r.Method != http.MethodPost || !p.decodeEmpty(w, r, runID) {
			return
		}
		p.ack(w, runID)
	default:
		p.writeError(w, http.StatusNotFound, "unknown_run", runID, nil)
	}
}

func (p *Peer) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(value, prefix)), []byte(p.opt.Token)) == 1
}

func (p *Peer) Info() PeerInfoDTO {
	p.Sweep()
	p.mu.Lock()
	active := p.activeRunID
	p.mu.Unlock()
	return PeerInfoDTO{ProtocolVersion: ProtocolVersion, SchemaVersion: SchemaVersion, LibraryVersion: buildinfo.Get().Short(), AgentID: p.opt.AgentID, Form: p.opt.Form, Role: p.opt.Role, Sections: append([]string(nil), p.opt.Sections...), Capabilities: append([]string(nil), p.opt.Capabilities...), Identity: p.opt.Identity, CgroupScope: p.opt.CgroupScope, Targets: append([]TargetSummaryDTO(nil), p.opt.Targets...), ActiveRunID: active, StartedAt: p.opt.StartedAt}
}

func (p *Peer) start(w http.ResponseWriter, r *http.Request) {
	var req StartRunRequest
	if !p.decode(w, r, "", &req) {
		return
	}
	if req.RunID == nil || !validID(*req.RunID) {
		p.writeError(w, http.StatusBadRequest, "invalid_run_id", "", nil)
		return
	}
	if req.Nonce == nil || !validID(*req.Nonce) {
		p.writeError(w, http.StatusBadRequest, "invalid_nonce", *req.RunID, nil)
		return
	}
	if req.Preempt == nil || !printable(req.Trigger, 64) {
		p.writeError(w, http.StatusBadRequest, "malformed_request", *req.RunID, nil)
		return
	}
	lease := time.Duration(req.LeaseMS) * time.Millisecond
	if lease <= 0 {
		lease = PeerStartedLease
	}
	if lease > PeerStartedLeaseMax {
		lease = PeerStartedLeaseMax
	}
	p.mutation.Lock()
	defer p.mutation.Unlock()
	p.Sweep()
	p.mu.Lock()
	if prior, ok := p.nonces[*req.Nonce]; ok && prior.runID != *req.RunID {
		p.mu.Unlock()
		p.writeError(w, http.StatusConflict, "nonce_mismatch", *req.RunID, nil)
		return
	}
	if record := p.runs[*req.RunID]; record != nil {
		if record.nonce != *req.Nonce {
			p.mu.Unlock()
			p.writeError(w, http.StatusConflict, "nonce_mismatch", *req.RunID, nil)
			return
		}
		result := record.start
		p.mu.Unlock()
		p.writeJSON(w, http.StatusOK, result)
		return
	}
	active := p.runs[p.activeRunID]
	if active != nil && !*req.Preempt {
		errDTO := ErrorDTO{ActiveRunID: active.runID, ActiveState: active.state, LeaseExpiresInMS: max(0, time.Until(active.leaseExpiry).Milliseconds())}
		p.mu.Unlock()
		p.writeError(w, http.StatusConflict, "run_active", *req.RunID, &errDTO)
		return
	}
	preempted := ""
	if active != nil {
		preempted = active.runID
	}
	p.mu.Unlock()
	result, err := p.ctrl.StartRun(r.Context(), runctl.StartRunOptions{Nonce: *req.Nonce, Preempt: *req.Preempt, Reason: "hub", Trigger: req.Trigger})
	if err != nil {
		p.writeRunctlError(w, *req.RunID, err)
		return
	}
	now := p.opt.Now().UTC()
	dto := StartResultDTO{RunID: *req.RunID, LocalRunID: result.RunID, Nonce: *req.Nonce, Epoch: uint64(result.Epoch), State: string(result.State), Validity: string(result.Validity), Collectors: result.Collectors, GenerationWindow: result.GenerationWindow, BoundaryWindow: result.BoundaryWindow, PreemptedRunID: preempted, LeaseExpiresAt: now.Add(lease), LeaseMS: lease.Milliseconds(), StartedAt: result.StartedAt}
	p.mu.Lock()
	if preempted != "" {
		if old := p.runs[preempted]; old != nil {
			old.state = "aborted"
			old.validity = "invalid"
			old.expiryReason = "preempted"
			old.updatedAt = now
		}
	}
	record := &runRecord{runID: *req.RunID, localRunID: result.RunID, nonce: *req.Nonce, origin: "peer", state: string(result.State), validity: string(result.Validity), epoch: uint64(result.Epoch), start: dto, leaseExpiry: dto.LeaseExpiresAt, updatedAt: now}
	p.runs[record.runID] = record
	p.order = append(p.order, record.runID)
	p.activeRunID = record.runID
	p.rememberNonceLocked(record.nonce, record.runID, now)
	p.pruneLocked()
	p.mu.Unlock()
	p.writeJSON(w, http.StatusCreated, dto)
}

func (p *Peer) finishRun(w http.ResponseWriter, r *http.Request, runID string) {
	p.mutation.Lock()
	defer p.mutation.Unlock()
	p.Sweep()
	p.mu.Lock()
	record := p.runs[runID]
	if record == nil {
		p.mu.Unlock()
		p.writeError(w, http.StatusNotFound, "unknown_run", runID, nil)
		return
	}
	if record.finish != nil {
		dto := *record.finish
		p.mu.Unlock()
		p.writeJSON(w, http.StatusOK, dto)
		return
	}
	if record.state == "aborted" {
		p.mu.Unlock()
		p.writeError(w, http.StatusGone, "run_aborted", runID, nil)
		return
	}
	localID := record.localRunID
	p.mu.Unlock()
	accepted, err := p.ctrl.FinishRun(r.Context(), localID)
	if err != nil {
		p.writeRunctlError(w, runID, err)
		return
	}
	dto := FinishAcceptedDTO{RunID: runID, Epoch: uint64(accepted.Epoch), State: "finishing", Validity: string(accepted.Validity), Collectors: accepted.Collectors, GenerationWindow: accepted.GenerationWindow, BoundaryWindow: accepted.BoundaryWindow, FinishLeaseExpiresAt: accepted.AcceptedAt.Add(20 * time.Second), AcceptedAt: accepted.AcceptedAt}
	p.mu.Lock()
	record.finish = &dto
	record.state = "finishing"
	record.validity = dto.Validity
	record.leaseExpiry = time.Time{}
	record.updatedAt = p.opt.Now().UTC()
	p.mu.Unlock()
	go p.awaitSnapshot(runID, localID)
	p.writeJSON(w, http.StatusOK, dto)
}

func (p *Peer) awaitSnapshot(runID, localID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	status, err := p.ctrl.Await(ctx, localID)
	if err != nil {
		return
	}
	snapshot, err := p.ctrl.SnapshotOf(localID)
	if err != nil {
		return
	}
	local := p.buildSnapshot(runID, snapshot, p.opt.MaxBytes)
	body, _ := json.Marshal(local)
	p.mu.Lock()
	defer p.mu.Unlock()
	record := p.runs[runID]
	if record == nil || record.localRunID != localID {
		return
	}
	record.state = string(status.State)
	record.validity = string(status.Validity)
	record.snapshot = local
	record.snapshotBytes = int64(len(body))
	record.leaseExpiry = p.opt.Now().UTC().Add(PeerAckLease)
	record.updatedAt = p.opt.Now().UTC()
}

func (p *Peer) status(w http.ResponseWriter, runID string) {
	p.Sweep()
	p.mu.Lock()
	record := p.runs[runID]
	if record == nil {
		p.mu.Unlock()
		p.writeError(w, http.StatusNotFound, "unknown_run", runID, nil)
		return
	}
	dto := p.statusLocked(record)
	p.mu.Unlock()
	p.writeJSON(w, http.StatusOK, dto)
}

func (p *Peer) statusLocked(record *runRecord) RunStatusDTO {
	if status, ok := p.ctrl.Status(record.localRunID); ok {
		record.state = string(status.State)
		record.validity = string(status.Validity)
		record.ackedBy = status.AckedBy
	}
	return RunStatusDTO{RunID: record.runID, LocalRunID: record.localRunID, Epoch: record.epoch, State: record.state, Validity: record.validity, AckedBy: record.ackedBy, Since: record.updatedAt, Origin: record.origin, LeaseExpiresAt: record.leaseExpiry, ExpiryReason: record.expiryReason, SnapshotReady: record.snapshot != nil, SnapshotBytes: record.snapshotBytes}
}

func (p *Peer) lease(w http.ResponseWriter, runID string) {
	p.Sweep()
	p.mu.Lock()
	defer p.mu.Unlock()
	record := p.runs[runID]
	if record == nil {
		p.writeError(w, http.StatusNotFound, "unknown_run", runID, nil)
		return
	}
	if record.state != "started" {
		p.writeError(w, http.StatusConflict, "lease_not_renewable", runID, nil)
		return
	}
	duration := time.Duration(record.start.LeaseMS) * time.Millisecond
	record.leaseExpiry = p.opt.Now().UTC().Add(duration)
	record.start.LeaseExpiresAt = record.leaseExpiry
	p.writeJSON(w, http.StatusOK, LeaseDTO{RunID: runID, State: record.state, LeaseExpiresAt: record.leaseExpiry, LeaseMS: duration.Milliseconds()})
}

func (p *Peer) snapshot(w http.ResponseWriter, r *http.Request, runID string) {
	p.Sweep()
	maxBytes := p.opt.MaxBytes
	if raw := r.URL.Query().Get("max_bytes"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < BudgetReserve {
			p.writeError(w, http.StatusBadRequest, "malformed_request", runID, nil)
			return
		}
		if value < maxBytes {
			maxBytes = value
		}
	}
	p.mu.Lock()
	record := p.runs[runID]
	if record == nil {
		p.mu.Unlock()
		p.writeError(w, http.StatusNotFound, "unknown_run", runID, nil)
		return
	}
	if record.snapshot == nil {
		state := record.state
		p.mu.Unlock()
		if state == "aborted" {
			p.writeError(w, http.StatusGone, "run_aborted", runID, nil)
		} else {
			p.writeError(w, http.StatusConflict, "not_finished", runID, nil)
		}
		return
	}
	source := cloneSnapshot(record.snapshot)
	p.mu.Unlock()
	bounded := boundSnapshot(source, maxBytes)
	body, _ := json.Marshal(bounded)
	if int64(len(body)) > maxBytes {
		p.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", runID, nil)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (p *Peer) abortRun(w http.ResponseWriter, r *http.Request, runID string) {
	var req AbortRequest
	if !p.decode(w, r, runID, &req) || !printable(req.Reason, 128) {
		return
	}
	p.mutation.Lock()
	defer p.mutation.Unlock()
	p.mu.Lock()
	record := p.runs[runID]
	if record == nil {
		p.mu.Unlock()
		p.writeJSON(w, http.StatusOK, AbortResultDTO{RunID: runID, State: "aborted", Reason: "hub-abort", PeerReason: "no-op", AbortedAt: p.opt.Now().UTC()})
		return
	}
	if record.abort != nil {
		dto := *record.abort
		p.mu.Unlock()
		p.writeJSON(w, http.StatusOK, dto)
		return
	}
	localID := record.localRunID
	snapshotDiscarded := record.snapshot != nil
	p.mu.Unlock()
	result, err := p.ctrl.AbortRun(r.Context(), localID, "hub-abort")
	if err != nil {
		p.writeRunctlError(w, runID, err)
		return
	}
	dto := AbortResultDTO{RunID: runID, Epoch: uint64(result.Epoch), State: "aborted", Reason: result.Reason, PeerReason: "explicit", Detached: result.Detached, Partial: result.Partial, SnapshotDiscarded: snapshotDiscarded, AbortedAt: result.AbortedAt}
	p.mu.Lock()
	record.abort = &dto
	record.state = "aborted"
	record.validity = "invalid"
	record.snapshot = nil
	record.snapshotBytes = 0
	record.leaseExpiry = time.Time{}
	if p.activeRunID == runID {
		p.activeRunID = ""
	}
	p.mu.Unlock()
	p.writeJSON(w, http.StatusOK, dto)
}

func (p *Peer) ack(w http.ResponseWriter, runID string) {
	p.mutation.Lock()
	defer p.mutation.Unlock()
	p.mu.Lock()
	record := p.runs[runID]
	if record == nil {
		p.mu.Unlock()
		p.writeError(w, http.StatusNotFound, "unknown_run", runID, nil)
		return
	}
	localID := record.localRunID
	p.mu.Unlock()
	if err := p.ctrl.AckBy(localID, "hub"); err != nil {
		p.writeRunctlError(w, runID, err)
		return
	}
	p.mu.Lock()
	record.state = "acknowledged"
	record.ackedBy = "hub"
	record.leaseExpiry = time.Time{}
	if p.activeRunID == runID {
		p.activeRunID = ""
	}
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (p *Peer) buildSnapshot(wireID string, snapshot *runctl.Snapshot, maxBytes int64) *LocalSnapshot {
	sections := map[string]json.RawMessage{}
	values := snapshot.Sections
	if p.opt.Snapshot != nil {
		values = p.opt.Snapshot(snapshot)
	}
	for name, value := range values {
		body, err := json.Marshal(value)
		if err == nil {
			sections[name] = body
		}
	}
	local := &LocalSnapshot{SchemaVersion: SchemaVersion, RunID: wireID, LocalRunID: snapshot.RunID, Epoch: uint64(snapshot.Epoch), Validity: string(snapshot.Validity), Meta: SnapshotMetaDTO{Capabilities: append([]string(nil), p.opt.Capabilities...), BoundaryWindow: snapshot.BoundaryWindow, GenerationWindow: snapshot.GenerationWindow, Identity: p.opt.Identity, CgroupScope: p.opt.CgroupScope}, Sections: sections, Budget: SnapshotBudgetDTO{MaxBytes: maxBytes}}
	return boundSnapshot(local, maxBytes)
}

func boundSnapshot(input *LocalSnapshot, maxBytes int64) *LocalSnapshot {
	out := cloneSnapshot(input)
	out.Budget.MaxBytes = maxBytes
	for attempts := 0; attempts < 3; attempts++ {
		body, _ := json.Marshal(out)
		out.Budget.EncodedBytes = int64(len(body))
		body, _ = json.Marshal(out)
		if int64(len(body)) <= maxBytes {
			return out
		}
		names := make([]string, 0, len(out.Sections))
		for name := range out.Sections {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool { return len(out.Sections[names[i]]) > len(out.Sections[names[j]]) })
		if len(names) == 0 {
			break
		}
		drop := names[0]
		delete(out.Sections, drop)
		out.Budget.DroppedSections = append(out.Budget.DroppedSections, drop)
		out.Issues = append(out.Issues, SectionIssueDTO{Section: drop, Code: "size-dropped"})
	}
	for name := range out.Sections {
		delete(out.Sections, name)
		out.Budget.DroppedSections = append(out.Budget.DroppedSections, name)
	}
	sort.Strings(out.Budget.DroppedSections)
	body, _ := json.Marshal(out)
	out.Budget.EncodedBytes = int64(len(body))
	return out
}

func cloneSnapshot(in *LocalSnapshot) *LocalSnapshot {
	out := *in
	out.Meta.Capabilities = append([]string(nil), in.Meta.Capabilities...)
	out.Sections = make(map[string]json.RawMessage, len(in.Sections))
	for k, v := range in.Sections {
		out.Sections[k] = append(json.RawMessage(nil), v...)
	}
	out.Issues = append([]SectionIssueDTO(nil), in.Issues...)
	out.Budget.ShrunkSections = append([]string(nil), in.Budget.ShrunkSections...)
	out.Budget.DroppedSections = append([]string(nil), in.Budget.DroppedSections...)
	return &out
}

func (p *Peer) Sweep() {
	now := p.opt.Now().UTC()
	var aborts, acks []string
	p.mu.Lock()
	for id, record := range p.runs {
		if record.leaseExpiry.IsZero() || now.Before(record.leaseExpiry) {
			continue
		}
		switch record.state {
		case "starting", "started":
			aborts = append(aborts, id)
		case "finished":
			acks = append(acks, id)
		}
	}
	p.mu.Unlock()
	for _, id := range aborts {
		p.expireStarted(id, now)
	}
	for _, id := range acks {
		p.expireFinished(id, now)
	}
}
func (p *Peer) expireStarted(id string, now time.Time) {
	p.mu.Lock()
	record := p.runs[id]
	if record == nil || record.leaseExpiry.After(now) || (record.state != "started" && record.state != "starting") {
		p.mu.Unlock()
		return
	}
	localID := record.localRunID
	p.mu.Unlock()
	result, _ := p.ctrl.AbortRun(context.Background(), localID, "hub-abort")
	p.mu.Lock()
	defer p.mu.Unlock()
	record = p.runs[id]
	if record == nil {
		return
	}
	record.state = "aborted"
	record.validity = "invalid"
	record.expiryReason = "started-lease-expired"
	record.leaseExpiry = time.Time{}
	record.abort = &AbortResultDTO{RunID: id, Epoch: uint64(result.Epoch), State: "aborted", Reason: result.Reason, PeerReason: "started-lease-expired", Detached: result.Detached, Partial: result.Partial, AbortedAt: result.AbortedAt}
	if p.activeRunID == id {
		p.activeRunID = ""
	}
}
func (p *Peer) expireFinished(id string, now time.Time) {
	p.mu.Lock()
	record := p.runs[id]
	if record == nil || record.state != "finished" || record.leaseExpiry.After(now) {
		p.mu.Unlock()
		return
	}
	localID := record.localRunID
	p.mu.Unlock()
	if p.ctrl.AckBy(localID, "lease") != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	record = p.runs[id]
	if record == nil {
		return
	}
	record.state = "acknowledged"
	record.ackedBy = "lease"
	record.expiryReason = "ack-lease"
	record.leaseExpiry = time.Time{}
	if p.activeRunID == id {
		p.activeRunID = ""
	}
}
func (p *Peer) watchLeases() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.Sweep()
		case <-p.stop:
			return
		}
	}
}

func (p *Peer) rememberNonceLocked(nonce, runID string, now time.Time) {
	if _, ok := p.nonces[nonce]; !ok {
		p.nonceOrder = append(p.nonceOrder, nonce)
	}
	p.nonces[nonce] = nonceRecord{runID: runID, at: now}
	for len(p.nonceOrder) > NonceHistoryMax {
		old := p.nonceOrder[0]
		p.nonceOrder = p.nonceOrder[1:]
		delete(p.nonces, old)
	}
}
func (p *Peer) pruneLocked() {
	for len(p.order) > RetainedRuns {
		index := -1
		for i, id := range p.order {
			if id != p.activeRunID {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		id := p.order[index]
		p.order = append(p.order[:index], p.order[index+1:]...)
		delete(p.runs, id)
	}
}
func (p *Peer) DebugCounts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.runs), len(p.nonces)
}

func (p *Peer) decodeEmpty(w http.ResponseWriter, r *http.Request, runID string) bool {
	var value struct{}
	return p.decode(w, r, runID, &value)
}
func (p *Peer) decode(w http.ResponseWriter, r *http.Request, runID string, target any) bool {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		p.writeError(w, http.StatusUnsupportedMediaType, "malformed_request", runID, nil)
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		p.writeError(w, http.StatusBadRequest, "malformed_request", runID, nil)
		return false
	}
	if len(body) > MaxRequestBytes {
		p.writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", runID, nil)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		p.writeError(w, http.StatusBadRequest, "malformed_request", runID, nil)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		p.writeError(w, http.StatusBadRequest, "trailing_data", runID, nil)
		return false
	}
	return true
}
func (p *Peer) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (p *Peer) writeError(w http.ResponseWriter, status int, code, runID string, base *ErrorDTO) {
	dto := ErrorDTO{Code: code, RunID: runID}
	if base != nil {
		dto.ActiveRunID = base.ActiveRunID
		dto.ActiveState = base.ActiveState
		dto.LeaseExpiresInMS = base.LeaseExpiresInMS
	}
	p.writeJSON(w, status, dto)
}
func (p *Peer) writeRunctlError(w http.ResponseWriter, runID string, err error) {
	code, status := "internal", http.StatusInternalServerError
	switch {
	case errors.Is(err, runctl.ErrRunActive):
		code, status = "run_active", http.StatusConflict
	case errors.Is(err, runctl.ErrRunAborted):
		code, status = "run_aborted", http.StatusGone
	case errors.Is(err, runctl.ErrUnknownRun):
		code, status = "unknown_run", http.StatusNotFound
	case errors.Is(err, runctl.ErrRunTransitioning):
		code, status = "run_transitioning", http.StatusConflict
	}
	p.writeError(w, status, code, runID, nil)
}
func validID(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._-", r) {
			return false
		}
	}
	return true
}
func validRole(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
func validAgentID(v string) bool {
	if len(v) != 36 || v[8] != '-' || v[13] != '-' || v[14] != '4' || v[18] != '-' || v[23] != '-' {
		return false
	}
	for i, r := range v {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
func printable(value string, maxLen int) bool {
	if len(value) > maxLen {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}
func validateNames(values []string) error {
	if len(values) > 64 {
		return fmt.Errorf("%w: too many names", ErrPeerConfig)
	}
	for _, v := range values {
		if len(v) < 1 || len(v) > 32 {
			return ErrPeerConfig
		}
		for _, r := range v {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
				return ErrPeerConfig
			}
		}
	}
	return nil
}
func sortedUnique(values []string) []string {
	set := map[string]struct{}{}
	for _, v := range values {
		set[v] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
