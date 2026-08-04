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
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/health"
	"github.com/ekusiadadus/isutools/internal/sysinfo"
	"github.com/ekusiadadus/isutools/procstats"
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

type accessLogCollector interface {
	Collect() error
	Snapshot() accesslog.Snapshot
	Reset() error
}

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
	SQLGeneration  func() int64
	RotateSQL      func() (generation int64, entries []agg.Entry)
	Health         *health.Registry
	HTTP           httpCollector
	AccessLog      accessLogCollector
	AccessLogQuiet time.Duration
	AccessLogPoll  time.Duration
	CollectTimeout time.Duration
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
}

// Meta identifies when, on which host, and from which revision a snapshot
// was taken. Generation increments on every reset so runs are comparable.
type Meta struct {
	SchemaVersion int    `json:"schema_version"`
	Time          string `json:"time"`
	Generation    int64  `json:"generation"`
	Revision      string `json:"revision"`
	Dirty         bool   `json:"dirty"`
	// Score is the benchmark score supplied via POST /save?score=; persisted
	// snapshots always carry it so every report is attributable to a result.
	Score   string         `json:"score,omitempty"`
	Host    sysinfo.Info   `json:"host"`
	Partial bool           `json:"partial"`
	Health  []health.Entry `json:"health,omitempty"`
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
}

type jsonPayload struct {
	Snapshot
	Prev *Snapshot `json:"prev,omitempty"`
}

type handler struct {
	p          Provider
	gen        atomic.Int64
	mu         sync.Mutex
	resetMu    sync.Mutex
	prev       *Snapshot
	runSeq     atomic.Uint64
	operation  chan struct{}
	curDB      *dbinspect.Schema
	curAdvisor []advisor.Check
}

// NewHandler returns the report handler. Routes are relative:
// GET / (run index), GET /<run-id> (stored run detail), GET /live,
// GET /snapshot.html, GET /json, GET /files/<name>,
// POST /reset, POST /collect, POST /save.
func NewHandler(p Provider) http.Handler {
	h := &handler{p: p, operation: make(chan struct{}, 1)}
	h.gen.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.root)
	mux.HandleFunc("/live", h.live)
	mux.HandleFunc("/diff", h.diff)
	mux.HandleFunc("/pprof/", pprofHandler)
	mux.HandleFunc("/snapshot.html", h.static)
	mux.HandleFunc("/json", h.json)
	mux.HandleFunc("/reset", h.reset)
	mux.HandleFunc("/collect", h.collect)
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

func (h *handler) makeSnapshot(generation int64, db *dbinspect.Schema, entries []agg.Entry) Snapshot {
	healthEntries, partial := []health.Entry(nil), false
	if h.p.Health != nil {
		healthEntries, partial = h.p.Health.Snapshot()
	}

	bi := buildinfo.Get()
	snap := Snapshot{
		Meta: Meta{
			SchemaVersion: schemaVersion,
			Time:          time.Now().In(reportTZ).Format(time.RFC3339),
			Generation:    generation,
			Revision:      bi.Short(),
			Dirty:         bi.Dirty,
			Host:          sysinfo.Get(),
			Partial:       partial,
			Health:        healthEntries,
		},
		DB:      db,
		Advisor: h.currentAdvisor(),
		SQL:     entries,
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
	return snap
}

func (h *handler) take() Snapshot {
	var entries []agg.Entry
	if h.p.SQL != nil {
		entries = h.p.SQL.Snapshot()
	}
	snap := h.makeSnapshot(h.currentGeneration(), h.currentDB(), entries)
	applyOverflowHealth(&snap)
	if h.p.HTTP != nil {
		snap.HTTP = h.p.HTTP.Snapshot()
		applyOverflowHealth(&snap)
	}
	if h.p.AccessLog != nil {
		value := h.p.AccessLog.Snapshot()
		snap.AccessLog = &value
		applyAccessLogHealth(&snap.Meta, value.Health)
		applyOverflowHealth(&snap)
	}
	if h.p.Proc != nil {
		value := h.p.Proc.Snapshot()
		snap.Proc = &value
		applyProcHealth(&snap.Meta, value.Health)
	}
	h.applyProtocolAdvice(&snap)
	h.applyQUICTelemetry(&snap)
	h.applyCacheTelemetry(&snap)
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
	snap := h.take()
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
	if h.p.RotateSQL != nil {
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
	if h.p.HTTP != nil {
		snap.HTTP = h.p.HTTP.Reset()
		applyOverflowHealth(&snap)
	}
	if h.p.AccessLog != nil {
		value := h.p.AccessLog.Snapshot()
		snap.AccessLog = &value
		applyAccessLogHealth(&snap.Meta, value.Health)
		applyOverflowHealth(&snap)
		if err := h.p.AccessLog.Reset(); err != nil && h.p.Health != nil {
			h.p.Health.Set("accesslog", health.StatusDegraded, err.Error())
		} else if h.p.Health != nil {
			h.p.Health.Set("accesslog", health.StatusOK, "")
		}
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
	h.mu.Lock()
	h.prev = &snap
	h.mu.Unlock()
	if h.p.Health != nil {
		h.p.Health.ResetDropped()
	}
	if h.p.Counters != nil {
		h.p.Counters.Reset()
	}
	// Re-capture the schema so the new generation records its pre-run state.
	h.captureDB()
	// Profile the fresh generation (i.e. the benchmark that follows).
	h.captureCPUProfile(h.currentGeneration())
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
	if !h.beginOperation(w) {
		return
	}
	defer h.endOperation()
	h.resetMu.Lock()
	defer h.resetMu.Unlock()

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
