// Package tracecapture owns bounded, run-aligned runtime execution traces.
package tracecapture

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime/trace"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/profileowner"
	"github.com/ekusiadadus/isutools/internal/safefs"
)

const (
	SchemaV1               = "isutools.runtime-trace/v1"
	DefaultDuration        = 5 * time.Second
	MaxDuration            = 30 * time.Second
	DefaultMaxBytes int64  = 64 << 20
	MaxBytes        int64  = 256 << 20
	MinFreeReserve  uint64 = 16 << 20

	StateCapturing = "capturing"
	StateReplayed  = "replayed"
	StateStopping  = "stopping"
	StatePublished = "published"
	StateSkipped   = "skipped"
	StateFailed    = "failed"
	StateAborted   = "aborted"

	CodeRunInvalid       = "run-invalid"
	CodeProfilerBusy     = "profiler-busy"
	CodeDiskBudget       = "disk-budget"
	CodeArtifactFailed   = "artifact-create-failed"
	CodeStartFailed      = "start-failed"
	CodeOutputTooLarge   = "output-too-large"
	CodeStopFailed       = "stop-failed"
	CodePublishFailed    = "publish-failed"
	CodeDurationComplete = "duration-complete"
	CodeAborted          = "aborted"
	CodeNoCapture        = "no-active-capture"
	CodeDurability       = "durability-unknown"

	ReasonMaxDuration = "max-duration"
)

var errOutputTooLarge = errors.New("tracecapture: output exceeds byte ceiling")

type Backend interface {
	Start(io.Writer) error
	Stop()
}

type RuntimeBackend struct{}

func (RuntimeBackend) Start(writer io.Writer) error { return trace.Start(writer) }
func (RuntimeBackend) Stop()                        { trace.Stop() }

type Owner interface {
	Acquire(string) bool
	Release(string) bool
}

type Options struct {
	Root     *safefs.Root
	Backend  Backend
	Owner    Owner
	Duration time.Duration
	MaxBytes int64
	MinFree  uint64
	Now      func() time.Time
	ID       func() string
}

type StartRequest struct {
	RunID      string
	Epoch      uint64
	State      string
	Validity   string
	BoundaryAt time.Time
}

type StartResult struct {
	RunID     string `json:"run_id"`
	Epoch     uint64 `json:"epoch"`
	CaptureID string `json:"capture_id,omitempty"`
	State     string `json:"state"`
	Code      string `json:"code,omitempty"`
}

type StopRequest struct {
	RunID      string
	Epoch      uint64
	Reason     string
	BoundaryAt time.Time
	Abort      bool
}

type StopTicket struct {
	RunID     string
	Epoch     uint64
	CaptureID string
	State     string
	Code      string
}

type Status struct {
	Schema         string        `json:"schema"`
	RunID          string        `json:"run_id"`
	Epoch          uint64        `json:"epoch"`
	CaptureID      string        `json:"capture_id"`
	State          string        `json:"state"`
	Code           string        `json:"code,omitempty"`
	File           string        `json:"file,omitempty"`
	SHA256         string        `json:"sha256,omitempty"`
	Bytes          int64         `json:"bytes,omitempty"`
	Sidecar        string        `json:"sidecar,omitempty"`
	SidecarSHA256  string        `json:"-"`
	BoundaryStart  time.Time     `json:"boundary_start,omitzero"`
	BoundaryFinish time.Time     `json:"boundary_finish,omitzero"`
	StartRequested time.Time     `json:"start_requested_at,omitzero"`
	StartCompleted time.Time     `json:"start_completed_at,omitzero"`
	StopRequested  time.Time     `json:"stop_requested_at,omitzero"`
	StopCompleted  time.Time     `json:"stop_completed_at,omitzero"`
	StopReason     string        `json:"stop_reason,omitempty"`
	RequestedSpan  time.Duration `json:"requested_span_ns"`
	CaptureSpan    time.Duration `json:"capture_span_ns"`
	HeadLoss       time.Duration `json:"head_loss_ns"`
	TailExcess     time.Duration `json:"tail_excess_ns"`
	Complete       bool          `json:"complete"`
}

type captureKey struct {
	runID string
	epoch uint64
}

type capture struct {
	mu       sync.Mutex
	status   Status
	file     *os.File
	temp     string
	final    string
	writer   *boundedHashWriter
	stopOnce sync.Once
	done     chan struct{}
}

type Manager struct {
	mu       sync.Mutex
	root     *safefs.Root
	backend  Backend
	owner    Owner
	duration time.Duration
	maxBytes int64
	minFree  uint64
	now      func() time.Time
	id       func() string
	records  map[captureKey]*capture
	active   *capture
	closed   bool
}

func New(options Options) (*Manager, error) {
	if options.Root == nil || options.Backend == nil {
		return nil, errors.New("tracecapture: root and backend are required")
	}
	if options.Owner == nil {
		options.Owner = &profileowner.Default
	}
	if options.Duration == 0 {
		options.Duration = DefaultDuration
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultMaxBytes
	}
	if options.MinFree == 0 {
		options.MinFree = MinFreeReserve
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.ID == nil {
		options.ID = newCaptureID
	}
	if options.Duration < 10*time.Millisecond || options.Duration > MaxDuration || options.MaxBytes < 1 || options.MaxBytes > MaxBytes {
		return nil, errors.New("tracecapture: invalid resource budget")
	}
	return &Manager{
		root: options.Root, backend: options.Backend, owner: options.Owner, duration: options.Duration,
		maxBytes: options.MaxBytes, minFree: options.MinFree, now: options.Now, id: options.ID,
		records: make(map[captureKey]*capture),
	}, nil
}

func (m *Manager) Start(request StartRequest) StartResult {
	result := StartResult{RunID: request.RunID, Epoch: request.Epoch, State: StateSkipped}
	if request.RunID == "" || request.Epoch == 0 || request.State != "started" ||
		(request.Validity != "valid" && request.Validity != "partial") || request.BoundaryAt.IsZero() {
		result.Code = CodeRunInvalid
		return result
	}
	key := captureKey{runID: request.RunID, epoch: request.Epoch}
	m.mu.Lock()
	if existing := m.records[key]; existing != nil {
		existing.mu.Lock()
		result.CaptureID, result.State, result.Code = existing.status.CaptureID, StateReplayed, existing.status.Code
		existing.mu.Unlock()
		m.mu.Unlock()
		return result
	}
	if m.closed || m.active != nil || !m.owner.Acquire("managed-trace") {
		result.Code = CodeProfilerBusy
		m.mu.Unlock()
		return result
	}
	available, err := m.root.AvailableBytes()
	if err != nil || available < uint64(m.maxBytes)+m.minFree {
		m.owner.Release("managed-trace")
		result.Code = CodeDiskBudget
		m.mu.Unlock()
		return result
	}
	id := m.id()
	if len(id) != 32 || !lowerHex(id) {
		m.owner.Release("managed-trace")
		result.Code = CodeArtifactFailed
		m.mu.Unlock()
		return result
	}
	final := "trace_" + id + ".out"
	temp := final + ".tmp"
	file, err := m.root.CreateExclusive(temp, 0o600)
	if err != nil {
		m.owner.Release("managed-trace")
		result.Code = CodeArtifactFailed
		m.mu.Unlock()
		return result
	}
	hasher := sha256.New()
	c := &capture{
		file: file, temp: temp, final: final, done: make(chan struct{}),
		status: Status{Schema: SchemaV1, RunID: request.RunID, Epoch: request.Epoch, CaptureID: id, State: StateCapturing,
			BoundaryStart: request.BoundaryAt, StartRequested: m.now(), RequestedSpan: m.duration},
	}
	c.writer = &boundedHashWriter{output: io.MultiWriter(file, hasher), hash: hasher, max: m.maxBytes}
	m.records[key], m.active = c, c
	m.mu.Unlock()

	if err := m.backend.Start(c.writer); err != nil {
		code := CodeStartFailed
		if errors.Is(err, errOutputTooLarge) || errors.Is(c.writer.Err(), errOutputTooLarge) {
			code = CodeOutputTooLarge
		}
		m.failStart(c, code)
		return StartResult{RunID: request.RunID, Epoch: request.Epoch, CaptureID: id, State: StateFailed, Code: code}
	}
	c.mu.Lock()
	c.status.StartCompleted = m.now()
	c.status.HeadLoss = positiveDuration(request.BoundaryAt, c.status.StartCompleted)
	c.mu.Unlock()
	result.CaptureID, result.State = id, StateCapturing
	go func() {
		timer := time.NewTimer(m.duration)
		defer timer.Stop()
		select {
		case <-timer.C:
			m.RequestStop(StopRequest{RunID: request.RunID, Epoch: request.Epoch, Reason: ReasonMaxDuration})
		case <-c.done:
		}
	}()
	return result
}

func (m *Manager) failStart(c *capture, code string) {
	_ = c.file.Close()
	_ = m.root.Remove(c.temp)
	m.owner.Release("managed-trace")
	c.mu.Lock()
	c.status.State, c.status.Code, c.status.StopCompleted = StateFailed, code, m.now()
	c.mu.Unlock()
	m.mu.Lock()
	if m.active == c {
		m.active = nil
	}
	m.mu.Unlock()
	close(c.done)
}

func (m *Manager) RequestStop(request StopRequest) StopTicket {
	key := captureKey{runID: request.RunID, epoch: request.Epoch}
	m.mu.Lock()
	c := m.records[key]
	m.mu.Unlock()
	if c == nil {
		return StopTicket{RunID: request.RunID, Epoch: request.Epoch, State: StateSkipped, Code: CodeNoCapture}
	}
	c.mu.Lock()
	ticket := StopTicket{RunID: request.RunID, Epoch: request.Epoch, CaptureID: c.status.CaptureID, State: c.status.State, Code: c.status.Code}
	c.mu.Unlock()
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.status.State, c.status.StopRequested, c.status.StopReason, c.status.BoundaryFinish = StateStopping, m.now(), safeReason(request.Reason), request.BoundaryAt
		c.mu.Unlock()
		ticket.State = StateStopping
		go m.settle(c, request.Abort)
	})
	return ticket
}

func (m *Manager) settle(c *capture, abort bool) {
	stopFailed := stopBackend(m.backend)
	m.owner.Release("managed-trace")
	c.mu.Lock()
	c.status.StopCompleted = m.now()
	c.status.CaptureSpan = positiveDuration(c.status.StartCompleted, c.status.StopCompleted)
	c.status.TailExcess = positiveDuration(c.status.BoundaryFinish, c.status.StopCompleted)
	c.mu.Unlock()
	if abort {
		_ = c.file.Close()
		_ = m.root.Remove(c.temp)
		m.finish(c, StateAborted, CodeAborted, false)
		return
	}
	if stopFailed {
		_ = c.file.Close()
		_ = m.root.Remove(c.temp)
		m.finish(c, StateFailed, CodeStopFailed, false)
		return
	}
	if errors.Is(c.writer.Err(), errOutputTooLarge) {
		_ = c.file.Close()
		_ = m.root.Remove(c.temp)
		m.finish(c, StateFailed, CodeOutputTooLarge, false)
		return
	}
	if err := c.file.Sync(); err != nil {
		_ = c.file.Close()
		_ = m.root.Remove(c.temp)
		m.finish(c, StateFailed, CodePublishFailed, false)
		return
	}
	if err := c.file.Close(); err != nil {
		_ = m.root.Remove(c.temp)
		m.finish(c, StateFailed, CodePublishFailed, false)
		return
	}
	publication, err := m.root.PublishNoReplace(c.temp, c.final)
	if err != nil && !publication.Visible {
		_ = m.root.Remove(c.temp)
		m.finish(c, StateFailed, CodePublishFailed, false)
		return
	}
	c.mu.Lock()
	c.status.File, c.status.SHA256, c.status.Bytes = c.final, c.writer.SHA256(), c.writer.Bytes()
	code := ""
	if errors.Is(err, safefs.ErrDurabilityUnknown) {
		code = CodeDurability
	}
	if c.status.StopReason == ReasonMaxDuration && code == "" {
		code = CodeDurationComplete
	}
	c.mu.Unlock()
	m.finish(c, StatePublished, code, true)
}

func (m *Manager) finish(c *capture, state, code string, complete bool) {
	c.mu.Lock()
	c.status.State, c.status.Code, c.status.Complete = state, code, complete
	status := c.status
	c.mu.Unlock()
	if sidecar, digest, err := m.publishSidecar(status); err == nil {
		c.mu.Lock()
		c.status.Sidecar, c.status.SidecarSHA256 = sidecar, digest
		c.mu.Unlock()
	} else if state == StatePublished {
		// The sidecar is the completion record. A raw trace without that
		// record is not a ready artifact and must not survive as an orphan.
		_ = m.root.Remove(status.File)
		c.mu.Lock()
		c.status.State = StateFailed
		c.status.Code = CodePublishFailed
		c.status.File = ""
		c.status.SHA256 = ""
		c.status.Bytes = 0
		c.status.Complete = false
		c.mu.Unlock()
	}
	m.mu.Lock()
	if m.active == c {
		m.active = nil
	}
	m.mu.Unlock()
	close(c.done)
}

func (m *Manager) publishSidecar(status Status) (string, string, error) {
	name := "trace_" + status.CaptureID + ".meta.json"
	status.Sidecar = name
	body, err := json.Marshal(status)
	if err != nil {
		return "", "", err
	}
	body = append(body, '\n')
	if len(body) > 64<<10 {
		return "", "", errors.New("tracecapture: sidecar too large")
	}
	temp := name + ".tmp"
	file, err := m.root.CreateExclusive(temp, 0o600)
	if err != nil {
		return "", "", err
	}
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = m.root.Remove(temp)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return "", "", err
	}
	if err := file.Sync(); err != nil {
		return "", "", err
	}
	if err := file.Close(); err != nil {
		return "", "", err
	}
	publication, err := m.root.PublishNoReplace(temp, name)
	if err != nil && !publication.Visible {
		return "", "", err
	}
	failed = false
	sum := sha256.Sum256(body)
	return name, hex.EncodeToString(sum[:]), nil
}

func (m *Manager) Await(ticket StopTicket, ctx context.Context) Status {
	m.mu.Lock()
	c := m.records[captureKey{runID: ticket.RunID, epoch: ticket.Epoch}]
	m.mu.Unlock()
	if c == nil {
		return Status{Schema: SchemaV1, RunID: ticket.RunID, Epoch: ticket.Epoch, State: StateSkipped, Code: CodeNoCapture}
	}
	c.mu.Lock()
	captureID := c.status.CaptureID
	c.mu.Unlock()
	if ticket.CaptureID != "" && ticket.CaptureID != captureID {
		return Status{Schema: SchemaV1, RunID: ticket.RunID, Epoch: ticket.Epoch, State: StateSkipped, Code: CodeNoCapture}
	}
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.status
	case <-ctx.Done():
		return Status{Schema: SchemaV1, RunID: ticket.RunID, Epoch: ticket.Epoch, CaptureID: ticket.CaptureID, State: StateStopping, Code: "await-timeout"}
	}
}

func (m *Manager) Status(runID string, epoch uint64) (Status, bool) {
	m.mu.Lock()
	c := m.records[captureKey{runID: runID, epoch: epoch}]
	m.mu.Unlock()
	if c == nil {
		return Status{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status, true
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	c := m.active
	m.mu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	runID, epoch := c.status.RunID, c.status.Epoch
	c.mu.Unlock()
	ticket := m.RequestStop(StopRequest{RunID: runID, Epoch: epoch, Reason: "shutdown", Abort: true, BoundaryAt: m.now()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.Await(ticket, ctx)
}

type boundedHashWriter struct {
	mu      sync.Mutex
	output  io.Writer
	hash    hash.Hash
	max     int64
	written int64
	err     error
}

func (w *boundedHashWriter) Write(body []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(body)) > w.max-w.written {
		w.err = errOutputTooLarge
		return 0, w.err
	}
	n, err := w.output.Write(body)
	w.written += int64(n)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *boundedHashWriter) Err() error   { w.mu.Lock(); defer w.mu.Unlock(); return w.err }
func (w *boundedHashWriter) Bytes() int64 { w.mu.Lock(); defer w.mu.Unlock(); return w.written }
func (w *boundedHashWriter) SHA256() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return hex.EncodeToString(w.hash.Sum(nil))
}

func stopBackend(backend Backend) (failed bool) {
	defer func() { failed = recover() != nil }()
	backend.Stop()
	return false
}

func positiveDuration(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

func safeReason(reason string) string {
	if reason == "" || len(reason) > 64 || strings.IndexFunc(reason, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-'
	}) >= 0 {
		return "operator-stop"
	}
	return reason
}

func newCaptureID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return hex.EncodeToString(sum[:16])
}

func lowerHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
