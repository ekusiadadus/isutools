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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/dbinspect"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/sysinfo"
)

// schemaVersion identifies the Snapshot JSON layout for downstream tooling.
const schemaVersion = 2

// Provider supplies the collectors to render. Nil fields are skipped.
type Provider struct {
	SQL *agg.Table
	// DB captures the database schema (tables/indexes). Called at handler
	// startup and on every reset so each generation records the pre-run state.
	DB func(context.Context) *dbinspect.Schema
	// DataDir persists snapshots for the dashboard history ("" = disabled).
	DataDir string
}

// Meta identifies when, on which host, and from which revision a snapshot
// was taken. Generation increments on every reset so runs are comparable.
type Meta struct {
	SchemaVersion int          `json:"schema_version"`
	Time          string       `json:"time"`
	Generation    int64        `json:"generation"`
	Revision      string       `json:"revision"`
	Dirty         bool         `json:"dirty"`
	Host          sysinfo.Info `json:"host"`
}

// Snapshot is the complete state of all measurements at one point in time.
type Snapshot struct {
	Meta Meta              `json:"meta"`
	DB   *dbinspect.Schema `json:"db,omitempty"`
	SQL  []agg.Entry       `json:"sql"`
}

type jsonPayload struct {
	Snapshot
	Prev *Snapshot `json:"prev,omitempty"`
}

type handler struct {
	p     Provider
	gen   atomic.Int64
	mu    sync.Mutex
	prev  *Snapshot
	curDB *dbinspect.Schema
}

// NewHandler returns the report handler. Routes are relative:
// GET / (dashboard), GET /snapshot.html, GET /json, GET /files/<name>,
// POST /reset, POST /save.
func NewHandler(p Provider) http.Handler {
	h := &handler{p: p}
	h.gen.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.dashboard)
	mux.HandleFunc("/snapshot.html", h.static)
	mux.HandleFunc("/json", h.json)
	mux.HandleFunc("/reset", h.reset)
	mux.HandleFunc("/save", h.save)
	mux.HandleFunc("/files/", h.files)
	return mux
}

// captureDB refreshes the schema state for the current generation.
func (h *handler) captureDB() {
	if h.p.DB == nil {
		return
	}
	schema := h.p.DB(context.Background())
	h.mu.Lock()
	h.curDB = schema
	h.mu.Unlock()
}

func (h *handler) take() Snapshot {
	h.mu.Lock()
	if h.curDB == nil {
		h.mu.Unlock()
		h.captureDB()
		h.mu.Lock()
	}
	db := h.curDB
	h.mu.Unlock()

	bi := buildinfo.Get()
	snap := Snapshot{
		Meta: Meta{
			SchemaVersion: schemaVersion,
			Time:          time.Now().Format(time.RFC3339),
			Generation:    h.gen.Load(),
			Revision:      bi.Short(),
			Dirty:         bi.Dirty,
			Host:          sysinfo.Get(),
		},
		DB: db,
	}
	if h.p.SQL != nil {
		snap.SQL = h.p.SQL.Snapshot()
	}
	return snap
}

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h.render(w, page{Snapshot: h.take(), Sortable: true, Dashboard: true, Files: h.listFiles()})
}

func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	snap := h.take()
	name := fmt.Sprintf("isutools_%s_%s.html",
		time.Now().Format("20060102-150405"), fileSafeRevision(snap.Meta))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.render(w, page{Snapshot: snap, Sortable: true})
}

type page struct {
	Snapshot  Snapshot
	Sortable  bool
	Dashboard bool
	Files     []string
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
		time.Now().Format("20060102-150405"), snap.Meta.Generation, fileSafeRevision(snap.Meta))
	if score := r.URL.Query().Get("score"); score != "" {
		base += "_score" + sanitizeName(score)
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
	if h.p.DataDir == "" {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/files/")
	if name != filepath.Base(name) || name == "" ||
		!(strings.HasSuffix(name, ".html") || strings.HasSuffix(name, ".json")) {
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

func (h *handler) json(w http.ResponseWriter, _ *http.Request) {
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
	snap := h.take()
	h.mu.Lock()
	h.prev = &snap
	h.mu.Unlock()
	if h.p.SQL != nil {
		h.p.SQL.Reset()
	}
	h.gen.Add(1)
	// Re-capture the schema so the new generation records its pre-run state.
	h.captureDB()
	w.WriteHeader(http.StatusNoContent)
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
