// Package web renders isutools measurements: a live report, a self-contained
// downloadable snapshot.html, machine-readable JSON, and a reset endpoint.
package web

import (
	"context"
	"encoding/json"
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
	Proc           processCollector
	// DB captures the database schema (tables/indexes). Called at handler
	// startup and on every reset so each generation records the pre-run state.
	DB func(context.Context) *dbinspect.Schema
	// Advisor reports well-known settings that are not configured. Captured
	// alongside the DB schema at startup and on every reset.
	Advisor func(context.Context) []advisor.Check
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
	curDB      *dbinspect.Schema
	curAdvisor []advisor.Check
}

// NewHandler returns the report handler. Routes are relative:
// GET / (run index), GET /<run-id> (stored run detail), GET /live,
// GET /snapshot.html, GET /json, GET /files/<name>,
// POST /reset, POST /collect, POST /save.
func NewHandler(p Provider) http.Handler {
	h := &handler{p: p}
	h.gen.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.root)
	mux.HandleFunc("/live", h.live)
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
	if h.p.DB != nil {
		schema = h.p.DB(context.Background())
	}
	if h.p.Advisor != nil {
		checks = h.p.Advisor(context.Background())
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
	return snap
}

// runIDPattern is the timestamp id embedded at the start of persisted
// snapshot names (e.g. 20260803-140100).
var runIDPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

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
	for _, name := range h.listFiles() {
		if strings.HasPrefix(name, id+"_") {
			http.ServeFile(w, r, filepath.Join(h.p.DataDir, name))
			return
		}
	}
	http.NotFound(w, r)
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
	ID    string
	Label string
	Gen   string
	Rev   string
	Score string
	File  string
	JSON  string
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
		if ts, err := time.Parse("20060102-150405", parts[0]); err == nil {
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
	snap := h.take()
	base := fmt.Sprintf("%s_gen%d_%s",
		time.Now().In(reportTZ).Format("20060102-150405"), snap.Meta.Generation, fileSafeRevision(snap.Meta))
	if score := r.URL.Query().Get("score"); score != "" {
		snap.Meta.Score = sanitizeName(score)
		base += "_score" + snap.Meta.Score
	}
	if err := h.writeSnapshot(snap, base); err != nil {
		http.Error(w, "isutools: save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"file": base + ".html"})
}

// writeSnapshot persists html+json atomically (tmp + rename).
func (h *handler) writeSnapshot(snap Snapshot, base string) error {
	jsonBytes, err := json.MarshalIndent(jsonPayload{Snapshot: snap}, "", " ")
	if err != nil {
		return err
	}
	var htmlBuf strings.Builder
	if err := reportTmpl.Execute(&htmlBuf, page{Snapshot: snap, Sortable: true}); err != nil {
		return err
	}
	for ext, content := range map[string][]byte{
		".json": jsonBytes,
		".html": []byte(htmlBuf.String()),
	} {
		tmp := filepath.Join(h.p.DataDir, base+ext+".tmp")
		if err := os.WriteFile(tmp, content, 0o644); err != nil {
			return err
		}
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
		!(strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".json") ||
			strings.HasSuffix(name, ".pprof")) {
		http.Error(w, "invalid file name", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, filepath.Join(h.p.DataDir, name))
}

// sanitizeName keeps only characters safe for a filename component.
func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '_'
		}
	}, s)
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
		}
	}
	if h.p.Proc != nil {
		value := h.p.Proc.Snapshot()
		snap.Proc = &value
		applyProcHealth(&snap.Meta, value.Health)
		if err := h.p.Proc.Reset(); err != nil && h.p.Health != nil {
			h.p.Health.Set("proc", health.StatusDegraded, err.Error())
		}
	}
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
	w.WriteHeader(http.StatusNoContent)
}

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
		for _, entry := range snapshot.AccessLog.Entries {
			if entry.URI == accesslog.OverflowURI {
				snapshot.Meta.Partial = true
				upsertHealth(&snapshot.Meta, health.Entry{Collector: "accesslog", Status: health.StatusDegraded, Message: "key limit exceeded; identities merged into (other)"})
				break
			}
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
