// Package web renders isutools measurements: a live report, a self-contained
// downloadable snapshot.html, machine-readable JSON, and a reset endpoint.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekusiadadus/isutools/accesslog"
	"github.com/ekusiadadus/isutools/advisor"
	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/counters"
	"github.com/ekusiadadus/isutools/dbinspect"
	"github.com/ekusiadadus/isutools/dbpool"
	"github.com/ekusiadadus/isutools/hoststats"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/sysinfo"
	"github.com/ekusiadadus/isutools/netstats"
	"github.com/ekusiadadus/isutools/procstats"
	"github.com/ekusiadadus/isutools/queryplan"
	"github.com/ekusiadadus/isutools/sqlrows"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// reportTZ pins every displayed/persisted timestamp to JST. FixedZone keeps
// this working in containers without tzdata.
var reportTZ = time.FixedZone("JST", 9*60*60)

// schemaVersion identifies the Snapshot JSON layout for downstream tooling.
const schemaVersion = 3

const (
	defaultInspectionTimeout = 5 * time.Second
	maxSnapshotBytes         = 32 << 20
)

var errSnapshotTooLarge = errors.New("snapshot exceeds 32 MiB limit")

type sqlSnapshotter interface {
	Snapshot() []agg.Entry
}

type httpCollector interface {
	Snapshot() httpstats.Snapshot
	Reset() httpstats.Snapshot
}

// httpCutShortReporter is the optional "did the last rotation give up waiting"
// probe. httpstats.Collector.Reset waits a bounded time for requests that
// started in the generation it closes; when the budget expires it returns the
// table as it stands, which is a usable but incomplete section. The count is
// only a signal if somebody reads it, so /reset samples it across the
// rotation and marks the section partial when it moves.
type httpCutShortReporter interface {
	ResetsCutShort() int64
}

type accessLogCollector interface {
	Collect() error
	Snapshot() accesslog.Snapshot
	Reset() error
}

// accessLogPeeker is the optional boundary-safe read of an access-log
// collector. Snapshot reads the log to end of file, which is unsafe once a
// generation boundary has fixed a freeze point the drain has not reached yet:
// the bytes it pulls in belong after the boundary, and the drain would seal
// them into the section it is cutting. Peek never crosses an outstanding
// freeze point.
type accessLogPeeker interface {
	Peek() accesslog.Snapshot
}

// The optional probes above are satisfied by silence: a collector that stops
// matching one loses the behaviour without anything failing. These pin the two
// real collectors to their probes at compile time, because a signal nobody can
// match is exactly the defect the cut-short probe was added to repair.
var (
	_ httpCutShortReporter = (*httpstats.Collector)(nil)
	_ accessLogPeeker      = (*accesslog.Collector)(nil)
)

type stableAccessLogCollector interface {
	CollectUntilStable(context.Context, time.Duration, time.Duration) error
}

type contextAccessLogCollector interface {
	CollectContext(context.Context) error
}

type processCollector interface {
	Snapshot() procstats.Snapshot
	Reset() error
}

// Provider supplies the collectors to render. Nil fields are skipped.
type Provider struct {
	SQL sqlSnapshotter
	// SQLGeneration and RotateSQL opt into atomic generation boundaries. They
	// are separate callbacks so simple aggregation tables remain usable in
	// tests and custom integrations.
	SQLGeneration func() int64
	RotateSQL     func() (generation int64, entries []agg.Entry)
	// RunGenerationManaged is shorthand for custom providers whose SQL, HTTP,
	// and Counters generations are all owned by StartRun. The per-collector
	// flags below let production preserve that guarantee when only a subset
	// registered successfully.
	RunGenerationManaged      bool
	SQLGenerationManaged      bool
	HTTPGenerationManaged     bool
	CountersGenerationManaged bool
	Health                    *health.Registry
	HTTP                      httpCollector
	AccessLog                 accessLogCollector
	AccessLogQuiet            time.Duration
	AccessLogPoll             time.Duration
	// AccessLogGenerationManaged says the access log's generation adapter is
	// registered with the run coordinator, so the coordinator's
	// BeginBoundary → Drain → Release cycle owns the aggregate's lifetime.
	//
	// POST /reset must then keep its hands off the legacy Snapshot()+Reset()
	// pair. Reset re-opens the log at the current end of file, drops the
	// aggregate and zeroes the health counters the drain's file-replacement
	// guard reads; running it between a closing boundary and that boundary's
	// drain leaves the drain nothing to seal, and the finished run's
	// access-log section silently comes out empty.
	AccessLogGenerationManaged bool
	CollectTimeout             time.Duration
	// InspectionTimeout bounds the context passed to DB and Advisor callbacks.
	InspectionTimeout time.Duration
	Proc              processCollector
	// DB captures the database schema (tables/indexes). Called at handler
	// startup and on every reset so each generation records the pre-run state.
	DB func(context.Context) *dbinspect.Schema
	// Advisor reports well-known settings that are not configured. Captured
	// alongside the DB schema at startup and on every reset.
	Advisor func(context.Context) []advisor.Check
	// CacheTelemetry is evaluated at snapshot time so application cache
	// hit/miss/eviction counters can match the measured interval.
	CacheTelemetry func() (*advisor.CacheTelemetry, error)
	// QUICTelemetry is evaluated at snapshot time so packet counters can match
	// the completed benchmark interval rather than handler startup.
	QUICTelemetry func() (*advisor.QUICTelemetry, error)
	// ProtocolTrafficClientFacing is false when a CDN/LB terminates the client
	// connection before the locally collected access log.
	ProtocolTrafficClientFacing *bool
	// Counters exposes user-defined counters (isutools.Count). Reset per
	// generation.
	Counters interface {
		Snapshot() []counters.Entry
		Reset()
	}
	// DataDir persists snapshots for the dashboard history ("" = disabled).
	DataDir string
	// PprofDuration > 0 captures a CPU profile for that long after every
	// reset (i.e. covering the benchmark), stored in DataDir (0 = disabled).
	PprofDuration time.Duration
	// StartRun opens a measurement run at the reset boundary and names it in
	// the reset response. Nil keeps the legacy behaviour, in which a reset
	// only rotates the collector generations and no run id exists.
	//
	// A failure is not fatal to the reset: the generations have already been
	// rotated by the time it is called, and refusing to answer would leave the
	// bench script unable to proceed with measurements that are, in fact,
	// running.
	StartRun func(ctx context.Context) (RunStart, error)
	// FinishRun fixes the closing boundary of the run in flight and returns as
	// soon as that boundary exists. POST /finish calls it and answers 202:
	// draining and snapshot building continue in the background, and making
	// the caller wait for them would put snapshot-building time inside the
	// measured window of whatever runs next.
	//
	// Nil leaves POST /finish unavailable, which is the legacy behaviour of a
	// transport wired without a run coordinator.
	FinishRun func(ctx context.Context) (RunFinish, error)
	// CompleteRun finishes the run in flight, waits for its immutable
	// snapshot, and acknowledges it. POST /save calls it before rendering, so
	// the persisted report describes the interval the run measured rather than
	// whatever the live collectors happen to hold after their generations were
	// frozen.
	//
	// A process that never opened a run must report the zero RunFinish and no
	// error: /save predates the run lifecycle and has to keep working without
	// one.
	CompleteRun func(ctx context.Context) (RunFinish, error)
	// AbortRun abandons the run in flight and fences its background worker.
	// POST /abort is idempotent and publishes no snapshot; profile artifacts
	// already captured at the opening boundary are marked as orphans.
	AbortRun func(ctx context.Context) (RunAbort, error)
	// Sections supplies the completed run's collector sections keyed by
	// collector name, as produced by the run coordinator. Unknown keys and
	// unexpected types are ignored, so a collector can be added on one side of
	// the wiring before the other catches up.
	Sections func() map[string]any
	// RunSnapshot supplies the same sections together with the lifecycle
	// evidence that makes a completed run auditable. When set it supersedes
	// Sections; the older callback remains for custom-provider compatibility.
	RunSnapshot func() *RunSnapshot
	// RuntimeProfiles lists the runtime profiles ("mutex", "block", "heap")
	// captured at a run boundary, in capture order. Empty — the default —
	// captures nothing. The rates themselves are process-wide runtime
	// settings owned by the caller; this package only writes what it is told
	// is enabled, so a profile whose rate is zero must not appear here.
	RuntimeProfiles []string
}

// RunStart identifies the measurement run a reset opened.
type RunStart struct {
	// RunID is the coordinator's run identifier, echoed in the reset response
	// so a bench script can name the run it started.
	RunID string
	// Epoch is the coordinator fencing token. It prevents a delayed boundary
	// artifact from being attached to a newer incarnation of the same run id.
	Epoch uint64
	// StartedAt is the opening boundary's moment. Boundary artifacts are named
	// after it rather than after the moment they are written, so an opening
	// and a closing artifact of one run share a filename prefix.
	StartedAt time.Time
	// Validity is the run's data-quality verdict ("valid", "partial",
	// "invalid"). It is a plain string because the transport copies it
	// verbatim and never branches on it.
	Validity string
	// GenerationWindow and BoundaryWindow preserve the coordinator's measured
	// uncertainty at the opening boundary for profile residual accounting.
	GenerationWindow BoundaryWindow
	BoundaryWindow   BoundaryWindow
}

// RunFinish is the record of a run's closing boundary, as reported by
// Provider.FinishRun and Provider.CompleteRun.
type RunFinish struct {
	// RunID names the run whose boundary was fixed. Empty means no run was in
	// flight, which is not an error: the save/collect loop predates runs.
	RunID string `json:"run_id,omitempty"`
	// Epoch is the coordinator fencing token for this closing boundary.
	Epoch uint64 `json:"epoch,omitempty"`
	// Validity is the run's data-quality verdict ("valid", "partial",
	// "invalid"), copied verbatim like RunStart.Validity.
	Validity string `json:"validity,omitempty"`
	// AcceptedAt is the measured moment the closing boundary was fixed. It is
	// the end of the interval every section of this run describes.
	AcceptedAt time.Time `json:"accepted_at,omitzero"`
	// GenerationWindow and BoundaryWindow preserve the coordinator's measured
	// uncertainty at the closing boundary for profile residual accounting.
	GenerationWindow BoundaryWindow `json:"generation_window,omitzero"`
	BoundaryWindow   BoundaryWindow `json:"boundary_window,omitzero"`
}

// RunAbort is the transport projection of the coordinator's abort result.
// POST /abort itself answers 204, but the handler needs this record to orphan
// the right profile pair and report the run id back in its header.
type RunAbort struct {
	RunID     string    `json:"run_id,omitempty"`
	Epoch     uint64    `json:"epoch,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Detached  bool      `json:"detached,omitempty"`
	AbortedAt time.Time `json:"aborted_at,omitzero"`
	Partial   []string  `json:"partial,omitempty"`
}

// BoundaryWindow is the measured span of one coordinated boundary.
type BoundaryWindow struct {
	Min    time.Time     `json:"min,omitzero"`
	Max    time.Time     `json:"max,omitzero"`
	Spread time.Duration `json:"spread_ns"`
}

// RunCollectorBoundary records one collector operation at a run boundary.
type RunCollectorBoundary struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Required  bool      `json:"required"`
	Phase     string    `json:"phase"`
	At        time.Time `json:"at,omitzero"`
	Committed bool      `json:"committed"`
	Code      string    `json:"code,omitempty"`
	Err       string    `json:"err,omitempty"`
	Dropped   bool      `json:"dropped,omitempty"`
}

// RunInfo is the immutable lifecycle envelope persisted with a report.
type RunInfo struct {
	RunID            string                 `json:"run_id"`
	Epoch            uint64                 `json:"epoch"`
	Validity         string                 `json:"validity"`
	Trigger          string                 `json:"trigger,omitempty"`
	Collectors       []RunCollectorBoundary `json:"collectors,omitempty"`
	GenerationWindow BoundaryWindow         `json:"generation_window"`
	BoundaryWindow   BoundaryWindow         `json:"boundary_window"`
	StartedAt        time.Time              `json:"started_at,omitzero"`
	FinishedAt       time.Time              `json:"finished_at,omitzero"`
}

// RunSnapshot is the provider-side form of a completed run. Sections remain
// top-level in the report while Info is persisted under Meta.Run.
type RunSnapshot struct {
	Info     RunInfo
	Sections map[string]any
}

// Meta identifies when, on which host, and from which revision a snapshot
// was taken. Generation increments on every reset so runs are comparable.
type Meta struct {
	SchemaVersion int    `json:"schema_version"`
	Time          string `json:"time"`
	Generation    int64  `json:"generation"`
	Revision      string `json:"revision"`
	Dirty         bool   `json:"dirty"`
	// BuildSource says how Revision was obtained (vcs, ldflags, env, unknown).
	// ProvenanceValid is false for an unknown or dirty build, where a snapshot
	// cannot be reproduced from Revision alone.
	BuildSource     string `json:"build_source"`
	ProvenanceValid bool   `json:"provenance_valid"`
	// Score is the benchmark score supplied via POST /save?score=; persisted
	// snapshots always carry it so every report is attributable to a result.
	Score   string         `json:"score,omitempty"`
	Host    sysinfo.Info   `json:"host"`
	Partial bool           `json:"partial"`
	Health  []health.Entry `json:"health,omitempty"`
	Run     *RunInfo       `json:"run,omitempty"`
	// Profiles is the run's runtime-profile record: every capture attempted at
	// either boundary and the pairs that can be differenced. It is filled when
	// a run is persisted, because that is the first moment both halves exist;
	// a live report has no closing half yet and omits the field. Additive and
	// omitempty, so a v1.0 reader of this JSON is unaffected.
	Profiles *ProfileManifest `json:"profiles,omitempty"`
}

// Snapshot is the complete state of all measurements at one point in time.
type Snapshot struct {
	Meta        Meta                    `json:"meta"`
	DB          *dbinspect.Schema       `json:"db,omitempty"`
	Advisor     []advisor.Check         `json:"advisor,omitempty"`
	Counters    []counters.Entry        `json:"counters,omitempty"`
	Connections *httpstats.ConnSnapshot `json:"connections,omitempty"`
	SQL         []agg.Entry             `json:"sql"`
	HTTP        httpstats.Snapshot      `json:"http,omitempty"`
	AccessLog   *accesslog.Snapshot     `json:"accesslog,omitempty"`
	Proc        *procstats.Snapshot     `json:"proc,omitempty"`

	// The sections below come from the run coordinator's baseline collectors
	// rather than from a live collector read, so they describe the interval
	// between two run boundaries and are absent until a run has completed.
	// Every one of them is additive and omitempty: a v1.0 reader of this JSON
	// is unaffected by their presence.
	//
	// The JSON keys are the collector names the coordinator registers under,
	// so a section can be traced from the snapshot back to the collector that
	// filled it without a translation table.
	Host    *hoststats.Section     `json:"hoststats,omitempty"`
	Network *netstats.NetworkStats `json:"network,omitempty"`
	SQLRows *sqlrows.Section       `json:"sqlrows,omitempty"`
	DBPool  []dbpool.Entry         `json:"dbpool,omitempty"`
	// QueryPlan holds the EXPLAIN output captured in the run's enrich phase.
	// It is filled from the same section map as the others rather than from a
	// live read, which is what keeps a dashboard refresh from putting EXPLAIN
	// statements on the measured database.
	QueryPlan *queryplan.Section `json:"queryplan,omitempty"`
}

// applyRunSections copies a completed run's baseline collector sections into
// the snapshot and forwards the health notes those sections carry.
//
// A section whose type does not match is skipped rather than reported: this
// is a display path, and the run record already carries the collector's own
// verdict on its data. Dropping one malformed section must not cost the
// reader the rest of the report.
func applyRunSections(snap *Snapshot, sections map[string]any) {
	if snap == nil || len(sections) == 0 {
		return
	}
	if section, ok := sections[hoststats.CollectorName].(*hoststats.Section); ok {
		snap.Host = section
	}
	if section, ok := sections[netstats.Default.Name()].(*netstats.NetworkStats); ok {
		snap.Network = section
	}
	if section, ok := sections[sqlrows.Name].(*sqlrows.Section); ok {
		snap.SQLRows = section
	}
	if entries, ok := sections[dbpool.Name].([]dbpool.Entry); ok {
		snap.DBPool = entries
	}
	if section, ok := sections[queryplan.Name].(*queryplan.Section); ok {
		snap.QueryPlan = section
	}
	// The collectors record why a section is missing or incomplete inside the
	// section value itself, because Collect has to stay pure. Forwarding those
	// notes here is what turns "the hoststats section is absent" into "the
	// hoststats section is absent because this host has no procfs".
	for _, entry := range SectionHealth(sections) {
		mergeHealth(&snap.Meta, entry)
		if entry.Status != health.StatusOK {
			snap.Meta.Partial = true
		}
	}
}

// applyRunIntervalSections overlays the generation collectors' frozen output
// onto a snapshot.
//
// A generation boundary hands the run's data over and leaves the live
// collector empty, so a report rendered after a run was finished would
// otherwise show the fresh, empty generation instead of the interval that was
// just measured. Presence, not cardinality, is authoritative: an explicitly
// empty frozen section means the run measured zero events, and must erase any
// post-boundary values already accumulated by the live collector.
func applyRunIntervalSections(snap *Snapshot, sections map[string]any) {
	if snap == nil || len(sections) == 0 {
		return
	}
	if frozen, ok := sections[sqlstats.SectionName].(sqlstats.Frozen); ok {
		snap.SQL = frozen.Entries
	}
	if result, ok := sections[httpstats.CollectorName].(httpstats.Result); ok {
		snap.HTTP = result.HTTP
		conns := result.Connections
		snap.Connections = &conns
	}
	if value, ok := sections[accesslog.SectionName].(accesslog.Snapshot); ok {
		section := value
		snap.AccessLog = &section
		applyAccessLogHealth(&snap.Meta, section.Health)
	}
	if frozen, ok := sections[counters.SectionName].(counters.Frozen); ok {
		snap.Counters = frozen.Entries
		if frozen.Dropped > 0 {
			snap.Meta.Partial = true
			upsertHealth(&snap.Meta, health.Entry{
				Collector: "counters", Status: health.StatusDegraded,
				Message: "name limit exceeded; identities merged into (other)", Dropped: frozen.Dropped,
			})
		}
	}
	applyOverflowHealth(snap)
}

// SectionHealth derives the health entries a completed run's sections report,
// one per collector, sorted by collector name.
//
// The baseline collectors carry their notes inside the section value instead
// of pushing them into a registry, because Collect must be pure. Somebody has
// to forward them; this is that step, and it is exported so the process-wide
// registry and this package's renderer forward exactly the same set.
func SectionHealth(sections map[string]any) []health.Entry {
	if len(sections) == 0 {
		return nil
	}
	var entries []health.Entry
	add := func(collector string, messages []string) {
		if len(messages) == 0 {
			return
		}
		entries = append(entries, health.Entry{
			Collector: collector,
			Status:    health.StatusDegraded,
			Message:   strings.Join(messages, "; "),
		})
	}
	if section, ok := sections[hoststats.CollectorName].(*hoststats.Section); ok {
		add(hoststats.CollectorName, hostSectionMessages(section))
	}
	if section, ok := sections[netstats.Default.Name()].(*netstats.NetworkStats); ok && section != nil {
		messages := make([]string, 0, len(section.Health))
		for _, note := range section.Health {
			messages = append(messages, joinNote(note.Key, note.Detail))
		}
		add(netstats.Default.Name(), messages)
	}
	if section, ok := sections[sqlrows.Name].(*sqlrows.Section); ok {
		add(sqlrows.Name, sqlRowsSectionMessages(section))
	}
	if pools, ok := sections[dbpool.Name].([]dbpool.Entry); ok {
		messages := make([]string, 0, len(pools))
		for _, entry := range pools {
			if entry.Partial || entry.Code != "" {
				messages = append(messages, joinNote(entry.TargetID, entry.Code))
			}
		}
		add(dbpool.Name, messages)
	}
	if section, ok := sections[queryplan.Name].(*queryplan.Section); ok {
		add(queryplan.Name, queryPlanSectionMessages(section))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Collector < entries[j].Collector })
	return entries
}

// hostSectionMessages renders the host section's notes, falling back to its
// codes when Collect produced no note of its own — a partial section with no
// explanation is exactly the thing this path exists to prevent.
func hostSectionMessages(section *hoststats.Section) []string {
	if section == nil {
		return nil
	}
	notes := section.HealthNotes()
	messages := make([]string, 0, len(notes)+1)
	for _, note := range notes {
		messages = append(messages, joinNote(note.Key, note.Message))
	}
	if len(messages) == 0 && section.Partial {
		messages = append(messages, joinNote("partial", strings.Join(section.Codes, ", ")))
	}
	return messages
}

// sqlRowsSectionMessages renders the sqlrows notes plus the per-target reason
// for every target that produced no numbers.
func sqlRowsSectionMessages(section *sqlrows.Section) []string {
	if section == nil {
		return nil
	}
	messages := make([]string, 0, len(section.Health)+len(section.Targets))
	for _, note := range section.Health {
		messages = append(messages, joinNote(note.Key, note.Message))
	}
	for _, target := range section.Targets {
		if target.Usable {
			continue
		}
		messages = append(messages, joinNote(target.TargetID, strings.TrimSpace(target.Code+" "+target.Reason)))
	}
	return messages
}

// queryPlanSectionMessages renders the capture's grouped notes.
//
// The notes are already one line per reason across every target that hit it,
// and the reasons themselves come from a fixed table inside the capture, so
// nothing derived from a statement's text can reach health through this path.
func queryPlanSectionMessages(section *queryplan.Section) []string {
	if section == nil {
		return nil
	}
	messages := make([]string, 0, len(section.Health))
	for _, note := range section.Health {
		messages = append(messages, joinNote(note.Key, note.Message))
	}
	return messages
}

// joinNote renders "<key>: <detail>", or just the key when there is no detail.
func joinNote(key, detail string) string {
	if detail == "" {
		return key
	}
	return key + ": " + detail
}

type jsonPayload struct {
	Snapshot
	Prev *Snapshot `json:"prev,omitempty"`
}

type handler struct {
	p       Provider
	gen     atomic.Int64
	mu      sync.Mutex
	resetMu sync.Mutex
	prev    *Snapshot
	runSeq  atomic.Uint64
	// runEnded marks that the run this handler last opened has had its closing
	// boundary fixed. A flush after that point would read log lines the
	// boundary already excluded straight into the generation the coordinator
	// is about to cut, so /collect refuses until the next reset.
	runEnded   atomic.Bool
	operation  chan struct{}
	curDB      *dbinspect.Schema
	curAdvisor []advisor.Check
	// collectPause runs between /collect's fast-path boundary check and its
	// claim on the operation slot. Production leaves it nil; it is the seam a
	// test uses to hold a flush inside the window where a concurrent /finish
	// can fix the closing boundary, which is the interleaving the second check
	// under resetMu exists to refuse.
	collectPause func()
}

// NewHandler returns the report handler. Routes are relative:
// GET / (run index), GET /<run-id> (stored run detail), GET /live,
// GET /snapshot.html, GET /json, GET /files/<name>,
// POST /reset, POST /collect, POST /finish, POST /abort, POST /save.
func NewHandler(p Provider) http.Handler { return newHandler(p).routes() }

// newHandler builds the report handler without wiring its routes, so an
// in-package test can reach the state the mux hides.
func newHandler(p Provider) *handler {
	h := &handler{p: p, operation: make(chan struct{}, 1)}
	h.gen.Store(1)
	return h
}

// routes wires the handler's endpoints.
func (h *handler) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.root)
	mux.HandleFunc("/live", h.live)
	mux.HandleFunc("/diff", h.diff)
	mux.HandleFunc("/pprof/", pprofHandler)
	mux.HandleFunc("/snapshot.html", h.static)
	mux.HandleFunc("/json", h.json)
	mux.HandleFunc("/reset", h.reset)
	mux.HandleFunc("/collect", h.collect)
	mux.HandleFunc("/finish", h.finish)
	mux.HandleFunc("/abort", h.abort)
	mux.HandleFunc("/save", h.save)
	mux.HandleFunc("/files/", h.files)
	return mux
}

// captureDB refreshes the schema state for the current generation.
func (h *handler) captureDB() {
	var schema *dbinspect.Schema
	var checks []advisor.Check
	timeout := h.p.InspectionTimeout
	if timeout <= 0 {
		timeout = defaultInspectionTimeout
	}
	if h.p.DB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		schema = h.p.DB(ctx)
		cancel()
	}
	if h.p.Advisor != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		checks = h.p.Advisor(ctx)
		cancel()
	}
	if schema == nil && checks == nil {
		return
	}
	h.mu.Lock()
	if schema != nil {
		h.curDB = schema
	}
	if checks != nil {
		h.curAdvisor = checks
	}
	h.mu.Unlock()
}

func (h *handler) currentDB() *dbinspect.Schema {
	h.mu.Lock()
	if h.curDB == nil {
		h.mu.Unlock()
		h.captureDB()
		h.mu.Lock()
	}
	db := h.curDB
	h.mu.Unlock()
	return db
}

func (h *handler) currentAdvisor() []advisor.Check {
	h.mu.Lock()
	checks := h.curAdvisor
	h.mu.Unlock()
	return checks
}

func (h *handler) currentGeneration() int64 {
	if h.p.SQLGeneration != nil {
		return h.p.SQLGeneration()
	}
	return h.gen.Load()
}

// completedRun returns one consistent completed-run record. The legacy
// Sections callback is wrapped in an envelope without evidence so existing
// custom providers remain source-compatible.
func (h *handler) completedRun() *RunSnapshot {
	if h.p.RunSnapshot != nil {
		return h.p.RunSnapshot()
	}
	if h.p.Sections == nil {
		return nil
	}
	sections := h.p.Sections()
	if sections == nil {
		return nil
	}
	return &RunSnapshot{Sections: sections}
}

func (h *handler) makeSnapshot(generation int64, db *dbinspect.Schema, entries []agg.Entry) Snapshot {
	return h.snapshotWith(generation, db, entries, h.completedRun())
}

// snapshotWith builds a snapshot around an already-read section map, so a
// caller that needs the sections twice — once for the baseline sections here,
// once for the interval overlay after the live reads — observes one consistent
// set rather than two reads of a run that may have finished in between.
func (h *handler) snapshotWith(generation int64, db *dbinspect.Schema, entries []agg.Entry, run *RunSnapshot) Snapshot {
	healthEntries, partial := []health.Entry(nil), false
	if h.p.Health != nil {
		healthEntries, partial = h.p.Health.Snapshot()
	}

	bi := buildinfo.Get()
	snap := Snapshot{
		Meta: Meta{
			SchemaVersion:   schemaVersion,
			Time:            time.Now().In(reportTZ).Format(time.RFC3339),
			Generation:      generation,
			Revision:        bi.Short(),
			Dirty:           bi.Dirty,
			BuildSource:     bi.Source,
			ProvenanceValid: bi.Source != "unknown" && bi.Revision != "" && !bi.Dirty,
			Host:            sysinfo.Get(),
			Partial:         partial,
			Health:          healthEntries,
		},
		DB:      db,
		Advisor: h.currentAdvisor(),
		SQL:     entries,
	}
	var sections map[string]any
	if run != nil {
		sections = run.Sections
		applyRunInfo(&snap, run.Info)
	}
	if h.p.Counters != nil {
		snap.Counters = h.p.Counters.Snapshot()
		if dropped, ok := h.p.Counters.(interface{ Dropped() uint64 }); ok && dropped.Dropped() > 0 {
			snap.Meta.Partial = true
			upsertHealth(&snap.Meta, health.Entry{
				Collector: "counters", Status: health.StatusDegraded,
				Message: "name limit exceeded; identities merged into (other)", Dropped: dropped.Dropped(),
			})
		}
	}
	if hc, ok := h.p.HTTP.(interface{ Connections() httpstats.ConnSnapshot }); ok && h.p.HTTP != nil {
		conns := hc.Connections()
		snap.Connections = &conns
	}
	applyRunSections(&snap, sections)
	return snap
}

// applyRunInfo attaches the lifecycle evidence and derives run-specific health
// from the same immutable collector records.
func applyRunInfo(snap *Snapshot, info RunInfo) {
	if snap == nil || info.RunID == "" {
		return
	}
	copyInfo := info
	copyInfo.Collectors = append([]RunCollectorBoundary(nil), info.Collectors...)
	snap.Meta.Run = &copyInfo
	if info.Validity != "" && info.Validity != "valid" {
		snap.Meta.Partial = true
	}
	for _, boundary := range info.Collectors {
		if boundary.Code == "" {
			continue
		}
		status := health.StatusDegraded
		if boundary.Dropped {
			status = health.StatusFailed
		}
		message := strings.TrimSpace(boundary.Phase + " " + boundary.Code)
		if boundary.Err != "" {
			message += ": " + boundary.Err
		}
		mergeHealth(&snap.Meta, health.Entry{
			Collector: boundary.Name,
			Status:    status,
			Message:   message,
		})
		snap.Meta.Partial = true
	}
}

// readAccessLog reads the access log for display.
//
// Snapshot is not usable here. It flushes to end of file, and POST /finish
// answers 202 as soon as the closing boundary exists, draining in the
// background: a dashboard refresh landing in that window reads past the freeze
// point and hands the drain traffic the run was measured to exclude. GET /,
// /live, /json and /snapshot.html all arrive here holding neither resetMu nor
// the operation slot, so nothing else keeps them out of that window. Peek is
// the same read minus that one hazard.
//
// A collector too old to offer Peek keeps the previous behaviour rather than
// losing its section: it has no generation adapter either, so it has no freeze
// point to cross.
func (h *handler) readAccessLog() accesslog.Snapshot {
	if peeker, ok := h.p.AccessLog.(accessLogPeeker); ok {
		return peeker.Peek()
	}
	return h.p.AccessLog.Snapshot()
}

func (h *handler) take() Snapshot {
	run := h.completedRun()
	var sections map[string]any
	if run != nil {
		sections = run.Sections
	}
	var entries []agg.Entry
	if h.p.SQL != nil {
		entries = h.p.SQL.Snapshot()
	}
	snap := h.snapshotWith(h.currentGeneration(), h.currentDB(), entries, run)
	applyOverflowHealth(&snap)
	if h.p.HTTP != nil {
		snap.HTTP = h.p.HTTP.Snapshot()
		applyOverflowHealth(&snap)
	}
	if h.p.AccessLog != nil {
		value := h.readAccessLog()
		snap.AccessLog = &value
		applyAccessLogHealth(&snap.Meta, value.Health)
		applyOverflowHealth(&snap)
	}
	if h.p.Proc != nil {
		value := h.p.Proc.Snapshot()
		snap.Proc = &value
		applyProcHealth(&snap.Meta, value.Health)
	}
	// The overlay runs after the live reads, not before: a finished run's
	// frozen generation is the authority on the interval it measured, and the
	// live collectors were rotated past it the moment its boundary was taken.
	applyRunIntervalSections(&snap, sections)
	h.applyProtocolAdvice(&snap)
	h.applyQUICTelemetry(&snap)
	h.applyCacheTelemetry(&snap)
	applyQueryPlanAdvice(&snap)
	return snap
}

func applyProtocolAdvice(snap *Snapshot) {
	applyProtocolAdviceWithSource(snap, true)
}

func (h *handler) applyProtocolAdvice(snap *Snapshot) {
	clientFacing := true
	if h.p.ProtocolTrafficClientFacing != nil {
		clientFacing = *h.p.ProtocolTrafficClientFacing
	}
	applyProtocolAdviceWithSource(snap, clientFacing)
}

func applyProtocolAdviceWithSource(snap *Snapshot, clientFacing bool) {
	samples := []advisor.ProtocolSample(nil)
	source := ""
	if snap.AccessLog != nil && len(snap.AccessLog.Protocols) > 0 {
		if clientFacing {
			source = "proxy access log"
		} else {
			source = "origin proxy access log (edge declared)"
		}
		samples = make([]advisor.ProtocolSample, 0, len(snap.AccessLog.Protocols))
		for _, entry := range snap.AccessLog.Protocols {
			samples = append(samples, advisor.ProtocolSample{
				Protocol: entry.Protocol, Count: entry.Count,
				Errors: entry.Status5xx, P95: entry.RequestP95,
			})
		}
	} else if len(snap.HTTP) > 0 {
		source = "application middleware"
		samples = make([]advisor.ProtocolSample, 0, len(snap.HTTP))
		for _, entry := range snap.HTTP {
			errors := int64(0)
			if entry.Status >= 500 {
				errors = entry.Count
			}
			samples = append(samples, advisor.ProtocolSample{
				Protocol: entry.Protocol, Count: entry.Count,
				Errors: errors, P95: entry.P95,
			})
		}
	}
	snap.Advisor = advisor.WithProtocolTrafficEvidence(snap.Advisor, source, clientFacing, samples)
}

func (h *handler) applyQUICTelemetry(snap *Snapshot) {
	if h.p.QUICTelemetry == nil {
		return
	}
	telemetry, err := h.p.QUICTelemetry()
	applyQUICTelemetry(snap, telemetry, err)
}

func applyQUICTelemetry(snap *Snapshot, telemetry *advisor.QUICTelemetry, err error) {
	snap.Advisor = advisor.WithQUICTelemetry(snap.Advisor, telemetry, err)
}

func (h *handler) applyCacheTelemetry(snap *Snapshot) {
	if h.p.CacheTelemetry == nil {
		return
	}
	telemetry, err := h.p.CacheTelemetry()
	applyCacheTelemetry(snap, telemetry, err)
}

func applyCacheTelemetry(snap *Snapshot, telemetry *advisor.CacheTelemetry, err error) {
	snap.Advisor = advisor.WithCacheTelemetry(snap.Advisor, telemetry, err)
}

// applyQueryPlanAdvice replaces the advisor's placeholder plan checks with a
// verdict once a run has actually captured an EXPLAIN.
//
// Nothing happens without a captured section: the placeholders advisor.Collect
// already emitted say the capture never ran, which is exactly right for a
// process with the feature off, and re-deriving them here would only risk
// disagreeing with them.
func applyQueryPlanAdvice(snap *Snapshot) {
	if snap == nil || snap.QueryPlan == nil {
		return
	}
	snap.Advisor = advisor.WithQueryPlans(snap.Advisor, advisorQueryPlans(snap.QueryPlan), nil)
}

// advisorQueryPlans flattens the captured section into the advisor's input.
//
// Every field copied here is either a closed enum or schema text (a table or
// index name); the sample statement never left the capture callback, and this
// type has no field able to hold it either. The nullable EXPLAIN columns are
// copied by value into fresh pointers rather than aliased, so the snapshot and
// the advisor cannot observe each other's edits.
func advisorQueryPlans(section *queryplan.Section) []advisor.QueryPlan {
	plans := make([]advisor.QueryPlan, 0, len(section.Targets))
	for _, target := range section.Targets {
		for _, plan := range target.Plans {
			converted := advisor.QueryPlan{
				TargetID:    target.TargetID,
				Digest:      plan.Digest,
				Query:       plan.Query,
				Freshness:   advisor.PlanFreshness(plan.Freshness),
				FreshReason: string(plan.FreshReason),
				Rows:        advisorPlanRows(plan.Rows),
			}
			if plan.Err != nil {
				converted.ErrClass = string(plan.Err.Class)
			}
			plans = append(plans, converted)
		}
	}
	return plans
}

func advisorPlanRows(rows []queryplan.PlanRow) []advisor.QueryPlanRow {
	if len(rows) == 0 {
		return nil
	}
	out := make([]advisor.QueryPlanRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, advisor.QueryPlanRow{
			SelectType:   copyString(row.SelectType),
			Table:        copyString(row.Table),
			Type:         copyString(row.Type),
			Key:          copyString(row.Key),
			PossibleKeys: copyString(row.PossibleKeys),
			Rows:         copyInt64(row.Rows),
			Extra:        copyString(row.Extra),
		})
	}
	return out
}

// copyString and copyInt64 duplicate a nullable column, preserving "the server
// reported no value" as a nil rather than collapsing it to an empty string or
// a zero.
func copyString(v *string) *string {
	if v == nil {
		return nil
	}
	value := *v
	return &value
}

func copyInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	value := *v
	return &value
}

// runIDPattern is the timestamp id embedded at the start of persisted
// snapshot names. Old second-resolution IDs remain readable; new IDs include
// nanoseconds and a per-handler sequence to prevent overwrite collisions.
var runIDPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(?:\.[0-9]{9}-[0-9]{6,})?$`)

// root serves the run index at "/" and stored run details at "/<run-id>".
func (h *handler) root(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if r.URL.Path == "/" {
		h.index(w)
		return
	}
	id := strings.Trim(r.URL.Path, "/")
	if !runIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	matches := make([]string, 0, 1)
	for _, name := range h.listFiles() {
		if strings.HasPrefix(name, id+"_") {
			matches = append(matches, name)
		}
	}
	switch len(matches) {
	case 0:
		http.NotFound(w, r)
	case 1:
		http.ServeFile(w, r, filepath.Join(h.p.DataDir, matches[0]))
	default:
		http.Error(w, "run id is ambiguous; use a collision-free saved run", http.StatusConflict)
	}
}

func (h *handler) index(w http.ResponseWriter) {
	data := indexPage{Snapshot: h.take(), Runs: h.listRuns(), Profiles: h.listProfiles()}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		http.Error(w, "isutools: render failed", http.StatusInternalServerError)
	}
}

func (h *handler) live(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	h.render(w, page{Snapshot: h.take(), Sortable: true})
}

// runEntry is one persisted run parsed from its snapshot filename
// (<ts>_gen<G>_<rev>[_score<S>].html).
type runEntry struct {
	ID     string
	Label  string
	Gen    string
	Rev    string
	Score  string
	File   string
	JSON   string
	PrevID string
}

type indexPage struct {
	Snapshot Snapshot
	Runs     []runEntry
	Profiles []string
}

func (h *handler) listRuns() []runEntry {
	runs := []runEntry{}
	for _, name := range h.listFiles() {
		base := strings.TrimSuffix(name, ".html")
		parts := strings.Split(base, "_")
		if len(parts) == 0 || !runIDPattern.MatchString(parts[0]) {
			continue
		}
		run := runEntry{ID: parts[0], Label: parts[0], File: name, JSON: base + ".json"}
		if ts, err := time.Parse("20060102-150405", parts[0][:15]); err == nil {
			run.Label = ts.Format("2006-01-02 15:04:05")
		}
		for _, part := range parts[1:] {
			switch {
			case strings.HasPrefix(part, "gen"):
				run.Gen = strings.TrimPrefix(part, "gen")
			case strings.HasPrefix(part, "score"):
				run.Score = strings.TrimPrefix(part, "score")
			default:
				run.Rev = part
			}
		}
		runs = append(runs, run)
	}
	// newest first; each run diffs against the chronologically previous one
	for i := 0; i+1 < len(runs); i++ {
		runs[i].PrevID = runs[i+1].ID
	}
	return runs
}

func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	snap := h.take()
	name := fmt.Sprintf("isutools_%s_%s.html",
		time.Now().In(reportTZ).Format("20060102-150405"), fileSafeRevision(snap.Meta))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.render(w, page{Snapshot: snap, Sortable: true})
}

type page struct {
	Snapshot Snapshot
	Sortable bool
}

func (h *handler) render(w http.ResponseWriter, data page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := reportTmpl.Execute(w, data); err != nil {
		http.Error(w, "isutools: render failed", http.StatusInternalServerError)
	}
}

// listFiles returns persisted snapshot names, newest first (names start
// with a sortable timestamp).
func (h *handler) listFiles() []string {
	if h.p.DataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(h.p.DataDir)
	if err != nil {
		return nil
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".html") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

func (h *handler) save(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if h.p.DataDir == "" {
		http.Error(w, "ISUTOOLS_DATA_DIR is not configured", http.StatusBadRequest)
		return
	}
	if !h.beginOperation(w) {
		return
	}
	defer h.endOperation()
	// Use the same boundary as reset/collect so a persisted pair cannot mix
	// collector generations.
	h.resetMu.Lock()
	defer h.resetMu.Unlock()
	// Fix the boundary before waiting for the immutable snapshot. Runtime
	// profiles must close here, while the background drain is still in flight;
	// taking them after CompleteRun would include the collector's own teardown.
	var run RunFinish
	if h.p.FinishRun != nil {
		accepted, err := h.p.FinishRun(r.Context())
		if err != nil {
			if h.p.Health != nil {
				h.p.Health.Set("runctl", health.StatusDegraded, err.Error())
			}
			http.Error(w, "isutools: no run to save: "+err.Error(), http.StatusConflict)
			return
		}
		run = accepted
		h.captureCloseProfiles(run)
	}
	// End the run first. Everything below then renders the run's immutable
	// snapshot instead of the live collectors, which is the whole point of
	// having a closing boundary: a report saved after the boundary must
	// describe the interval, not the empty generation that replaced it.
	completed := h.completeRun(r.Context())
	if completed.RunID != "" {
		run = completed
	}
	// Legacy/custom providers may expose CompleteRun without FinishRun. Their
	// capture is necessarily late, but still produces a complete, explicit pair.
	h.captureCloseProfiles(run)
	if run.RunID != "" {
		h.runEnded.Store(true)
		w.Header().Set(runIDHeader, run.RunID)
	}
	snap := h.take()
	// The run's profile record is attached here rather than in take(): both
	// halves of a pair exist only once the run has been closed, and a live
	// report has no run id to look one up by.
	snap.Meta.Profiles = h.profileManifest(run.RunID)
	base := fmt.Sprintf("%s_gen%d_%s",
		h.nextRunID(), snap.Meta.Generation, fileSafeRevision(snap.Meta))
	if score := r.URL.Query().Get("score"); score != "" {
		snap.Meta.Score = sanitizeName(score)
		base += "_score" + snap.Meta.Score
	}
	if err := h.writeSnapshot(snap, base); err != nil {
		if errors.Is(err, errSnapshotTooLarge) {
			http.Error(w, "isutools: save failed: "+err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "isutools: save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"file": base + ".html"})
}

func (h *handler) nextRunID() string {
	stamp := time.Now().In(reportTZ).Format("20060102-150405.000000000")
	return fmt.Sprintf("%s-%06d", stamp, h.runSeq.Add(1))
}

// writeSnapshot persists html+json atomically (tmp + rename).
func (h *handler) writeSnapshot(snap Snapshot, base string) error {
	jsonBytes, err := json.MarshalIndent(jsonPayload{Snapshot: snap}, "", " ")
	if err != nil {
		return err
	}
	if len(jsonBytes) > maxSnapshotBytes {
		return errSnapshotTooLarge
	}
	var htmlBuf strings.Builder
	if err := reportTmpl.Execute(&htmlBuf, page{Snapshot: snap, Sortable: true}); err != nil {
		return err
	}
	if htmlBuf.Len() > maxSnapshotBytes {
		return errSnapshotTooLarge
	}
	// Prepare both files first, then publish JSON followed by HTML. The run
	// index only lists HTML, so it can never expose a run before its JSON pair.
	outputs := []struct {
		ext     string
		content []byte
	}{
		{ext: ".json", content: jsonBytes},
		{ext: ".html", content: []byte(htmlBuf.String())},
	}
	for _, output := range outputs {
		tmp := filepath.Join(h.p.DataDir, base+output.ext+".tmp")
		if err := os.WriteFile(tmp, output.content, 0o600); err != nil {
			return err
		}
	}
	defer func() {
		for _, output := range outputs {
			_ = os.Remove(filepath.Join(h.p.DataDir, base+output.ext+".tmp"))
		}
	}()
	for _, output := range outputs {
		ext := output.ext
		tmp := filepath.Join(h.p.DataDir, base+ext+".tmp")
		if err := os.Rename(tmp, filepath.Join(h.p.DataDir, base+ext)); err != nil {
			return err
		}
	}
	return nil
}

func (h *handler) files(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if h.p.DataDir == "" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/files/")
	if name != filepath.Base(name) || name == "" ||
		(!strings.HasSuffix(name, ".html") && !strings.HasSuffix(name, ".json") &&
			!strings.HasSuffix(name, ".pprof")) {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, filepath.Join(h.p.DataDir, name))
}

// sanitizeName keeps only characters safe for a filename component.
func sanitizeName(s string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, s)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func (h *handler) json(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	h.mu.Lock()
	prev := h.prev
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	if err := enc.Encode(jsonPayload{Snapshot: h.take(), Prev: prev}); err != nil {
		http.Error(w, "isutools: encode failed", http.StatusInternalServerError)
	}
}

func (h *handler) reset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !h.beginOperation(w) {
		return
	}
	defer h.endOperation()
	h.resetMu.Lock()
	defer h.resetMu.Unlock()

	var snap Snapshot
	if h.p.RunGenerationManaged || h.p.SQLGenerationManaged {
		// StartRun owns the rotation. This read is only the previous display;
		// rotating here as well would advance the same collector twice and put
		// the public generation ahead of the run coordinator's epoch.
		snap = h.take()
	} else if h.p.RotateSQL != nil {
		generation, entries := h.p.RotateSQL()
		snap = h.makeSnapshot(generation, h.currentDB(), entries)
	} else {
		snap = h.take()
		if resetter, ok := h.p.SQL.(interface{ Reset() }); ok {
			resetter.Reset()
		}
		h.gen.Add(1)
	}
	applyOverflowHealth(&snap)
	if h.p.HTTP != nil && !h.p.RunGenerationManaged && !h.p.HTTPGenerationManaged {
		before := h.httpResetsCutShort()
		snap.HTTP = h.p.HTTP.Reset()
		applyOverflowHealth(&snap)
		applyHTTPCutShort(&snap, h.httpResetsCutShort() > before)
	}
	if h.p.AccessLog != nil {
		value := h.readAccessLog()
		snap.AccessLog = &value
		applyAccessLogHealth(&snap.Meta, value.Health)
		applyOverflowHealth(&snap)
		h.rebaselineAccessLog()
	}
	if h.p.Proc != nil {
		value := h.p.Proc.Snapshot()
		snap.Proc = &value
		applyProcHealth(&snap.Meta, value.Health)
		if err := h.p.Proc.Reset(); err != nil && h.p.Health != nil {
			h.p.Health.Set("proc", health.StatusDegraded, err.Error())
		} else if h.p.Health != nil {
			h.p.Health.Set("proc", health.StatusOK, "")
		}
	}
	h.applyProtocolAdvice(&snap)
	h.applyQUICTelemetry(&snap)
	h.applyCacheTelemetry(&snap)
	applyQueryPlanAdvice(&snap)
	h.mu.Lock()
	h.prev = &snap
	h.mu.Unlock()
	if h.p.Health != nil {
		h.p.Health.ResetDropped()
	}
	if h.p.Counters != nil && !h.p.RunGenerationManaged && !h.p.CountersGenerationManaged {
		h.p.Counters.Reset()
	}
	// Refresh schema and advisor inputs before the measured boundary opens.
	// These callbacks issue their own SQL; running them after StartRun would put
	// isutools' information_schema probes into the application's sqlrows interval.
	h.captureDB()
	// Open the run here, immediately after the generations were rotated, and
	// after the schema inspection above: an observation that lands between
	// the rotation and the run boundary belongs to the new generation but to
	// no run, and would be silently lost.
	run, err := h.startRun(r.Context())
	if err != nil {
		http.Error(w, "isutools: run could not be started: "+err.Error(), http.StatusConflict)
		return
	}
	// The generations are fresh again, so a flush has somewhere to land even
	// when the run itself could not be opened.
	h.runEnded.Store(false)
	if run.RunID != "" {
		w.Header().Set(runIDHeader, run.RunID)
	}
	// Profile the fresh generation (i.e. the benchmark that follows).
	h.captureCPUProfile(h.currentGeneration())
	// The opening runtime profiles are taken here, after the boundary is fixed
	// and immediately before the response: the benchmarker starts loading when
	// it sees the response, so anything captured later would already contain
	// benchmark traffic.
	h.captureRuntimeProfiles(run, ProfilePointOpen, h.currentGeneration())
	w.WriteHeader(http.StatusNoContent)
}

// rebaselineAccessLog re-opens the access log at the current end of file so
// the generation this reset starts is empty.
//
// It is the legacy rotation, and it must not run once the run coordinator's
// generation adapter is registered. Reset drops the aggregate and zeroes the
// rotation and copytruncate counters that the drain's file-replacement guard
// compares against the freeze point it recorded. Applied in the window between
// a closing boundary and its drain — POST /finish answers 202 and drains in
// the background, so POST /reset can easily land there — the drain then finds
// nothing left to read, seals an empty aggregate, and the finished run reports
// an access-log section of zero rather than the traffic it measured. The
// adapter's BeginBoundary → Drain → Release cycle already cuts the aggregate
// at exactly the right offset, so there is nothing here to do.
func (h *handler) rebaselineAccessLog() {
	if h.p.AccessLogGenerationManaged {
		return
	}
	if err := h.p.AccessLog.Reset(); err != nil {
		if h.p.Health != nil {
			h.p.Health.Set("accesslog", health.StatusDegraded, err.Error())
		}
		return
	}
	if h.p.Health != nil {
		h.p.Health.Set("accesslog", health.StatusOK, "")
	}
}

// runIDHeader names the run a reset opened. Bench scripts read it to label
// the measurements they are about to produce.
const runIDHeader = "X-Isutools-Run-Id"

// startRun opens the run this reset measures. A coordinator failure degrades
// health and is returned to /reset, which fails closed: a benchmark must not
// begin after the full boundary failed to open.
func (h *handler) startRun(ctx context.Context) (RunStart, error) {
	if h.p.StartRun == nil {
		return RunStart{}, nil
	}
	run, err := h.p.StartRun(ctx)
	if err != nil {
		if h.p.Health != nil {
			h.p.Health.Set("runctl", health.StatusDegraded, err.Error())
		}
		return RunStart{}, err
	}
	if h.p.Health != nil {
		h.p.Health.Set("runctl", health.StatusOK, "")
	}
	return run, nil
}

// completeRun ends the run this save reports on and waits for its immutable
// snapshot.
//
// A failure is recorded as degraded health and otherwise ignored. /save must
// keep persisting a report for a process that never opened a run — that is the
// v1.0 flow — and for one whose run died: refusing to save would throw away
// measurements that do exist because the lifecycle bookkeeping around them
// does not.
func (h *handler) completeRun(ctx context.Context) RunFinish {
	if h.p.CompleteRun == nil {
		return RunFinish{}
	}
	run, err := h.p.CompleteRun(ctx)
	if err != nil {
		if h.p.Health != nil {
			h.p.Health.Set("runctl", health.StatusDegraded, err.Error())
		}
		return RunFinish{}
	}
	if h.p.Health != nil && run.RunID != "" {
		h.p.Health.Set("runctl", health.StatusOK, "")
	}
	return run
}

// finish fixes the closing boundary of the run in flight and answers 202.
//
// It is the terminal endpoint for a bench script that wants measurement to
// stop at a precise moment without also persisting a report. The snapshot is
// built in the background, so the response carries the accepted boundary
// rather than the result; GET /json and POST /save read the result once it
// exists.
func (h *handler) finish(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h.p.FinishRun == nil {
		http.Error(w, "isutools: this build has no run coordinator", http.StatusServiceUnavailable)
		return
	}
	if !h.beginOperation(w) {
		return
	}
	defer h.endOperation()
	// The same boundary lock as reset/collect/save: a freeze taken while the
	// access log is being flushed would cut the generation under the reader.
	h.resetMu.Lock()
	defer h.resetMu.Unlock()

	run, err := h.p.FinishRun(r.Context())
	if err != nil {
		if h.p.Health != nil {
			h.p.Health.Set("runctl", health.StatusDegraded, err.Error())
		}
		http.Error(w, "isutools: no run to finish: "+err.Error(), http.StatusConflict)
		return
	}
	// The closing half of the run's profile pair, taken on this line and not
	// one later. FinishRun returns the accepted boundary without waiting for
	// the drain, the collect or the snapshot build, so a capture taken after
	// any of that would contain isutools' own background work — which mutex
	// and block profiles would attribute to lock contention, the one thing
	// they are read for. Whatever did land between the freeze and here is
	// measured rather than assumed, and reported as the pair's tail excess.
	h.captureCloseProfiles(run)
	if h.p.Health != nil {
		h.p.Health.Set("runctl", health.StatusOK, "")
	}
	if run.RunID != "" {
		h.runEnded.Store(true)
		w.Header().Set(runIDHeader, run.RunID)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(run)
}

// abort abandons the run in flight without producing a snapshot. It uses the
// same operation and boundary locks as reset/finish/save so no flush or new
// boundary can cross the coordinator's epoch fence.
func (h *handler) abort(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h.p.AbortRun == nil {
		http.Error(w, "isutools: this build has no run coordinator", http.StatusServiceUnavailable)
		return
	}
	if !h.beginOperation(w) {
		return
	}
	defer h.endOperation()
	h.resetMu.Lock()
	defer h.resetMu.Unlock()

	run, err := h.p.AbortRun(r.Context())
	if err != nil {
		if h.p.Health != nil {
			h.p.Health.Set("runctl", health.StatusDegraded, err.Error())
		}
		http.Error(w, "isutools: run could not be aborted: "+err.Error(), http.StatusConflict)
		return
	}
	h.abortRunProfiles(run.RunID)
	if h.p.Health != nil {
		h.p.Health.Set("runctl", health.StatusOK, "")
	}
	if run.RunID != "" {
		h.runEnded.Store(true)
		w.Header().Set(runIDHeader, run.RunID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) collect(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if h.p.AccessLog == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// A flush reads the log to end of file. Once the run's closing boundary is
	// fixed those bytes belong to the next generation, and pulling them in
	// would silently pad the interval that was just measured. This first test
	// is only a fast path: it runs outside the region that makes /collect and
	// /finish mutually exclusive, so it answers a boundary that was already
	// fixed without paying for the operation slot.
	if h.refuseAfterBoundary(w) {
		return
	}
	if h.collectPause != nil {
		h.collectPause()
	}
	if !h.beginOperation(w) {
		return
	}
	defer h.endOperation()
	h.resetMu.Lock()
	defer h.resetMu.Unlock()
	// The authoritative test, taken inside that region. Between the fast path
	// above and this line a /finish can claim the slot, freeze the collectors
	// and mark the run ended; a flush resuming here would read past the freeze
	// point into the section the coordinator is still cutting, which is the
	// exact traffic the closing boundary was placed to exclude.
	if h.refuseAfterBoundary(w) {
		return
	}

	timeout := h.p.CollectTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var err error
	if stable, ok := h.p.AccessLog.(stableAccessLogCollector); ok && h.p.AccessLogQuiet > 0 {
		err = stable.CollectUntilStable(ctx, h.p.AccessLogQuiet, h.p.AccessLogPoll)
	} else if aware, ok := h.p.AccessLog.(contextAccessLogCollector); ok {
		err = aware.CollectContext(ctx)
	} else {
		err = h.p.AccessLog.Collect()
	}
	if err != nil {
		if h.p.Health != nil {
			h.p.Health.Set("accesslog", health.StatusDegraded, err.Error())
		}
		http.Error(w, "isutools: accesslog collect failed", http.StatusServiceUnavailable)
		return
	}
	if h.p.Health != nil {
		h.p.Health.Set("accesslog", health.StatusOK, "")
	}
	w.WriteHeader(http.StatusNoContent)
}

// refuseAfterBoundary answers 409 when the run's closing boundary is already
// fixed, and reports whether it did. Both of /collect's checks go through it so
// the fast path and the authoritative one can never disagree about the answer.
func (h *handler) refuseAfterBoundary(w http.ResponseWriter) bool {
	if !h.runEnded.Load() {
		return false
	}
	w.Header().Set("Retry-After", "1")
	http.Error(w, "the run's boundary is already fixed; POST /reset before collecting again", http.StatusConflict)
	return true
}

func (h *handler) beginOperation(w http.ResponseWriter) bool {
	select {
	case h.operation <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "another reset, collect, or save is already running", http.StatusConflict)
		return false
	}
}

func (h *handler) endOperation() { <-h.operation }

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	http.Error(w, method+" only", http.StatusMethodNotAllowed)
	return false
}

// httpResetsCutShort reads the HTTP collector's cut-short counter, or 0 from a
// collector that does not keep one.
func (h *handler) httpResetsCutShort() int64 {
	reporter, ok := h.p.HTTP.(httpCutShortReporter)
	if !ok {
		return 0
	}
	return reporter.ResetsCutShort()
}

// httpCutShortMessage explains a section that was cut off its own tail.
const httpCutShortMessage = "the generation was closed before every in-flight request finished; requests still running at the boundary are missing from this section"

// applyHTTPCutShort marks an HTTP section that a rotation gave up waiting for.
// The numbers it does hold are real — a late request only ever writes to its
// own sealed table — so this is a partial section, not a failed one, and the
// note is what stops a reader comparing it with a complete one as if the two
// were the same measurement.
func applyHTTPCutShort(snap *Snapshot, cutShort bool) {
	if snap == nil || !cutShort {
		return
	}
	snap.Meta.Partial = true
	// mergeHealth, not upsert: an overflowing key table and a cut-short
	// rotation are independent findings about the same section, and replacing
	// either would hide evidence the other reader needs.
	mergeHealth(&snap.Meta, health.Entry{
		Collector: "http", Status: health.StatusDegraded, Message: httpCutShortMessage,
	})
}

func applyAccessLogHealth(meta *Meta, state accesslog.Health) {
	status := health.StatusOK
	switch state.Status {
	case accesslog.StatusPartial:
		status = health.StatusDegraded
	case accesslog.StatusError:
		status = health.StatusFailed
	}
	message := state.LastError
	if message == "" {
		message = state.Message
	}
	dropped := uint64(0)
	if state.Dropped > 0 {
		dropped = uint64(state.Dropped)
	}
	upsertHealth(meta, health.Entry{Collector: "accesslog", Status: status, Message: message, Dropped: dropped})
	if status != health.StatusOK || dropped > 0 || state.Partial > 0 {
		meta.Partial = true
	}
}

func applyProcHealth(meta *Meta, state procstats.Health) {
	status := health.StatusOK
	switch state.Status {
	case procstats.StatusPartial:
		status = health.StatusDegraded
	case procstats.StatusUnavailable:
		status = health.StatusFailed
	}
	upsertHealth(meta, health.Entry{
		Collector: "proc",
		Status:    status,
		Message:   strings.Join(state.Errors, "; "),
		Dropped:   state.Dropped,
	})
	if state.Partial || status != health.StatusOK || state.Dropped > 0 {
		meta.Partial = true
	}
}

func applyOverflowHealth(snapshot *Snapshot) {
	for _, entry := range snapshot.SQL {
		if entry.Key == agg.OverflowKey {
			snapshot.Meta.Partial = true
			upsertHealth(&snapshot.Meta, health.Entry{Collector: "sql", Status: health.StatusDegraded, Message: "key limit exceeded; identities merged into (other)"})
			break
		}
	}
	for _, entry := range snapshot.HTTP {
		if entry.Path == httpstats.OverflowPath {
			snapshot.Meta.Partial = true
			upsertHealth(&snapshot.Meta, health.Entry{Collector: "http", Status: health.StatusDegraded, Message: "key limit exceeded; identities merged into (other)"})
			break
		}
	}
	if snapshot.AccessLog != nil {
		messages := make([]string, 0, 3)
		dropped := snapshot.AccessLog.StoryDropped + snapshot.AccessLog.FlowDropped
		if snapshot.AccessLog.StoryDropped > 0 {
			messages = append(messages, "scenario-story limit exceeded; sessions, pages, or steps were truncated")
		}
		if snapshot.AccessLog.FlowDropped > 0 {
			messages = append(messages, "user-flow limit exceeded; transitions were merged or skipped")
		}
		for _, entry := range snapshot.AccessLog.Entries {
			if entry.URI == accesslog.OverflowURI {
				messages = append(messages, "key limit exceeded; identities merged into (other)")
				break
			}
		}
		if len(messages) > 0 {
			snapshot.Meta.Partial = true
			mergeHealth(&snapshot.Meta, health.Entry{
				Collector: "accesslog", Status: health.StatusDegraded,
				Message: strings.Join(messages, "; "), Dropped: uint64(dropped),
			})
		}
	}
}

func upsertHealth(meta *Meta, update health.Entry) {
	for i := range meta.Health {
		if meta.Health[i].Collector != update.Collector {
			continue
		}
		if healthSeverity(meta.Health[i].Status) > healthSeverity(update.Status) {
			return
		}
		meta.Health[i] = update
		return
	}
	meta.Health = append(meta.Health, update)
	sort.Slice(meta.Health, func(i, j int) bool { return meta.Health[i].Collector < meta.Health[j].Collector })
}

// mergeHealth combines independent diagnostics from one collector. This is
// used when parser health and bounded-aggregation health are both relevant to
// the same snapshot; replacing either would hide partial-data evidence.
func mergeHealth(meta *Meta, update health.Entry) {
	for i := range meta.Health {
		if meta.Health[i].Collector != update.Collector {
			continue
		}
		current := &meta.Health[i]
		if healthSeverity(update.Status) > healthSeverity(current.Status) {
			current.Status = update.Status
		}
		if update.Message != "" && update.Message != current.Message {
			if current.Message == "" {
				current.Message = update.Message
			} else {
				current.Message += "; " + update.Message
			}
		}
		current.Dropped += update.Dropped
		return
	}
	upsertHealth(meta, update)
}

func healthSeverity(status health.Status) int {
	switch status {
	case health.StatusFailed:
		return 3
	case health.StatusDegraded:
		return 2
	case health.StatusOK:
		return 1
	default:
		return 0
	}
}

// fileSafeRevision turns "f4fdb31 (dirty)" into "f4fdb31-dirty" for filenames.
func fileSafeRevision(m Meta) string {
	rev := m.Revision
	if i := len("f4fdb31"); len(rev) > i {
		rev = rev[:i]
	}
	if m.Dirty {
		return rev + "-dirty"
	}
	return rev
}
