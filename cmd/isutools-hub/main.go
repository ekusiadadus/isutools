// Command isutools-hub coordinates loopback peers reached through SSH tunnels.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ekusiadadus/isutools/internal/hubconfig"
	"github.com/ekusiadadus/isutools/multihost"
)

type report struct {
	SchemaVersion int                    `json:"schema_version"`
	RunID         string                 `json:"run_id"`
	Validity      string                 `json:"validity"`
	StartedAt     time.Time              `json:"started_at"`
	FinishedAt    time.Time              `json:"finished_at,omitzero"`
	Peers         []multihost.PeerResult `json:"peers"`
	File          string                 `json:"file,omitempty"`
}

type server struct {
	hub     *multihost.Hub
	dataDir string
	gate    chan struct{}
	mu      sync.RWMutex
	active  *multihost.HubRun
	current *report
}

func main() {
	addr := flag.String("addr", "127.0.0.1:19193", "literal loopback listen address")
	peersFile := flag.String("peers", os.Getenv("ISUTOOLS_HUB_PEERS_FILE"), "owner-only peers JSON file")
	dataDir := flag.String("data-dir", defaultString(os.Getenv("ISUTOOLS_DATA_DIR"), "./isutools-hub-data"), "result directory")
	preempt := flag.Bool("preempt", false, "allow a new run to preempt an active peer run")
	flag.Parse()
	if !literalLoopback(*addr) {
		fatal("listen address must use a literal loopback IP")
	}
	peers, err := hubconfig.Load(*peersFile)
	if err != nil {
		fatal("peers file rejected")
	}
	hub, err := multihost.NewHub(multihost.HubConfig{Peers: peers, Preempt: *preempt})
	if err != nil {
		fatal("peer configuration invalid")
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fatal("result directory unavailable")
	}
	handler := newServer(hub, *dataDir)
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fatal("listen failed")
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, MaxHeaderBytes: 8 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	fmt.Fprintf(os.Stderr, "isutools-hub: listening on %s\n", listener.Addr())
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			fatal("server failed")
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
		<-done
	}
}

func newServer(hub *multihost.Hub, dataDir string) *server {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &server{hub: hub, dataDir: dataDir, gate: gate}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		s.render(w)
	case r.Method == http.MethodGet && r.URL.Path == "/json":
		s.mu.RLock()
		current := s.current
		s.mu.RUnlock()
		if current == nil {
			http.Error(w, `{"error":"no-result"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, current)
	case r.Method == http.MethodPost && r.URL.Path == "/reset":
		s.start(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/finish":
		s.finish(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/abort":
		s.abort(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) acquire(w http.ResponseWriter) bool {
	select {
	case <-s.gate:
		return true
	default:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "mutation-busy"})
		return false
	}
}

func (s *server) start(w http.ResponseWriter, r *http.Request) {
	if !s.acquire(w) {
		return
	}
	defer func() { s.gate <- struct{}{} }()
	s.mu.RLock()
	active := s.active
	s.mu.RUnlock()
	if active != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run-active"})
		return
	}
	runID, nonce, err := newID(), newID(), error(nil)
	if runID == "" || nonce == "" {
		err = errors.New("identity unavailable")
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "start-failed"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	run, startErr := s.hub.Start(ctx, runID, nonce)
	current := &report{SchemaVersion: multihost.SchemaVersion, RunID: runID, StartedAt: time.Now().UTC()}
	if run != nil {
		current.Validity, current.Peers = run.Validity, run.Peers
	}
	s.mu.Lock()
	s.current = current
	if startErr == nil {
		s.active = run
	}
	s.mu.Unlock()
	if startErr != nil {
		writeJSON(w, http.StatusServiceUnavailable, current)
		return
	}
	writeJSON(w, http.StatusCreated, current)
}

func (s *server) finish(w http.ResponseWriter, r *http.Request) {
	if !s.acquire(w) {
		return
	}
	defer func() { s.gate <- struct{}{} }()
	s.mu.RLock()
	active, published := s.active, s.current
	s.mu.RUnlock()
	if active == nil || published == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run-not-active"})
		return
	}
	copy := *published
	current := &copy
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	peers, validity := s.hub.Finish(ctx, active, 0)
	current.Peers, current.Validity, current.FinishedAt = peers, validity, time.Now().UTC()
	if file, err := persist(s.dataDir, current); err == nil {
		current.File = file
	} else {
		current.Validity = "invalid"
	}
	s.mu.Lock()
	s.active = nil
	s.current = current
	s.mu.Unlock()
	status := http.StatusOK
	if current.Validity == "invalid" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, current)
}

func (s *server) abort(w http.ResponseWriter, r *http.Request) {
	if !s.acquire(w) {
		return
	}
	defer func() { s.gate <- struct{}{} }()
	s.mu.RLock()
	active, published := s.active, s.current
	s.mu.RUnlock()
	if active == nil || published == nil {
		writeJSON(w, http.StatusOK, map[string]string{"state": "idle"})
		return
	}
	copy := *published
	current := &copy
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	current.Peers, current.Validity = s.hub.Abort(ctx, active)
	current.FinishedAt = time.Now().UTC()
	s.mu.Lock()
	s.active = nil
	s.current = current
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, current)
}

func persist(dir string, current *report) (string, error) {
	name := "multihost_" + current.StartedAt.UTC().Format("20060102T150405.000000000Z") + ".json"
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(current); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	return name, file.Close()
}

func (s *server) render(w http.ResponseWriter) {
	s.mu.RLock()
	current := s.current
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = hubPage.Execute(w, current)
}

var hubPage = template.Must(template.New("hub").Parse(`<!doctype html><meta charset="utf-8"><title>isutools multi-host</title><style>body{font:14px system-ui;margin:2rem}table{border-collapse:collapse;width:100%}td,th{border:1px solid #ddd;padding:.4rem;text-align:left}</style><h1>isutools multi-host</h1>{{if .}}<p>run {{.RunID}} / validity {{.Validity}} / file {{.File}}</p><table><tr><th>role</th><th>agent</th><th>form</th><th>required</th><th>state</th><th>sealed</th><th>failure</th></tr>{{range .Peers}}<tr><td>{{.Info.Role}}</td><td>{{.Info.AgentID}}</td><td>{{.Form}}</td><td>{{.Required}}</td><td>{{if .Status}}{{.Status.State}}{{end}}</td><td>{{.Sealed}}</td><td>{{if .Failure}}{{.Failure.Phase}}/{{.Failure.Code}}{{end}}</td></tr>{{end}}</table><p><a href="/json">JSON</a></p>{{else}}<p>no run yet</p>{{end}}`))

func newID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return ""
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func literalLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "isutools-hub:", message)
	os.Exit(1)
}
