// Package web renders isutools measurements: a live report, a self-contained
// downloadable snapshot.html, machine-readable JSON, and a reset endpoint.
package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/internal/agg"
	"github.com/ekusiadadus/isutools/internal/sysinfo"
)

// schemaVersion identifies the Snapshot JSON layout for downstream tooling.
const schemaVersion = 1

// Provider supplies the aggregation tables to render. Nil tables are skipped.
type Provider struct {
	SQL *agg.Table
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
	Meta Meta        `json:"meta"`
	SQL  []agg.Entry `json:"sql"`
}

type jsonPayload struct {
	Snapshot
	Prev *Snapshot `json:"prev,omitempty"`
}

type handler struct {
	p    Provider
	gen  atomic.Int64
	mu   sync.Mutex
	prev *Snapshot
}

// NewHandler returns the report handler. Mount it under any prefix
// (e.g. chi's r.Mount("/debug/isutools", ...)); routes are relative:
// GET /, GET /snapshot.html, GET /json, POST /reset.
func NewHandler(p Provider) http.Handler {
	h := &handler{p: p}
	h.gen.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.live)
	mux.HandleFunc("/snapshot.html", h.static)
	mux.HandleFunc("/json", h.json)
	mux.HandleFunc("/reset", h.reset)
	return mux
}

func (h *handler) take() Snapshot {
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
	}
	if h.p.SQL != nil {
		snap.SQL = h.p.SQL.Snapshot()
	}
	return snap
}

func (h *handler) live(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	h.render(w, false)
}

func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	snap := h.take()
	name := fmt.Sprintf("isutools_%s_%s.html",
		time.Now().Format("20060102-150405"), fileSafeRevision(snap.Meta))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.render(w, true)
}

func (h *handler) render(w http.ResponseWriter, sortable bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := struct {
		Snapshot Snapshot
		Sortable bool
	}{h.take(), sortable}
	if err := reportTmpl.Execute(w, data); err != nil {
		http.Error(w, "isutools: render failed", http.StatusInternalServerError)
	}
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
