package web

import (
	"fmt"
	"log"
	"net/http"
	netpprof "net/http/pprof"
	"os"
	"path/filepath"
	rpprof "runtime/pprof"
	"strings"
	"sync/atomic"
	"time"
)

// pprofHandler exposes the process profiles under /pprof/ on the admin
// server (the admin server runs inside the instrumented process, so these
// are the application's profiles).
func pprofHandler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/pprof/")
	switch name {
	case "profile":
		netpprof.Profile(w, r)
	case "trace":
		netpprof.Trace(w, r)
	case "symbol":
		netpprof.Symbol(w, r)
	case "cmdline":
		netpprof.Cmdline(w, r)
	default:
		// Index serves "" and named runtime profiles (heap, goroutine, ...)
		// but expects the /debug/pprof/ path prefix.
		r.URL.Path = "/debug/pprof/" + name
		netpprof.Index(w, r)
	}
}

// cpuCaptureActive guards the process-wide CPU profiler (only one capture
// can run at a time).
var cpuCaptureActive atomic.Bool

// captureCPUProfile records a CPU profile for the given duration into
// DataDir, named after the generation it measures. Failures are logged and
// otherwise ignored: profiling must never break measurement or the app.
func (h *handler) captureCPUProfile(generation int64) {
	if h.p.PprofDuration <= 0 || h.p.DataDir == "" {
		return
	}
	if !cpuCaptureActive.CompareAndSwap(false, true) {
		log.Print("isutools: CPU capture already running; skipping")
		return
	}
	name := fmt.Sprintf("%s_gen%d_cpu.pprof",
		time.Now().In(reportTZ).Format("20060102-150405"), generation)
	path := filepath.Join(h.p.DataDir, name)
	f, err := os.Create(path)
	if err != nil {
		cpuCaptureActive.Store(false)
		log.Printf("isutools: CPU profile create failed: %v", err)
		return
	}
	if err := rpprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		cpuCaptureActive.Store(false)
		log.Printf("isutools: CPU profile start failed: %v", err)
		return
	}
	go func() {
		defer cpuCaptureActive.Store(false)
		time.Sleep(h.p.PprofDuration)
		rpprof.StopCPUProfile()
		if err := f.Close(); err != nil {
			log.Printf("isutools: CPU profile close failed: %v", err)
			return
		}
		log.Printf("isutools: CPU profile saved: %s", name)
	}()
}

// ProfilePoint names the run boundary a runtime profile was captured at. A
// mutex, block or heap profile is process-wide and cumulative, so a single
// file says nothing about a run: only the difference between the two
// boundaries does, and the point is what tells them apart.
type ProfilePoint string

const (
	// ProfilePointOpen is the capture taken as the run opens.
	ProfilePointOpen ProfilePoint = "open"
	// ProfilePointClose is the capture taken as the run's boundary is frozen.
	ProfilePointClose ProfilePoint = "close"
)

// profileCaptureLease bounds a whole boundary capture. runtime/pprof's WriteTo
// takes no context and cannot be interrupted, so the lease is enforced as a
// gate between profile kinds rather than as a timeout: whatever has started
// runs to completion, and the kinds after it are skipped.
const profileCaptureLease = 3 * time.Second

// captureRuntimeProfiles writes the enabled runtime profiles for one run
// boundary into DataDir and returns the artifact names it published.
//
// It runs synchronously in the caller's goroutine. Capturing asynchronously
// would make the captured moment depend on the scheduler, which is precisely
// the thing a boundary artifact is supposed to pin down.
//
// Nothing here is fatal: a failed capture is logged and the remaining kinds
// are still attempted, because losing a profile must never cost the run.
func (h *handler) captureRuntimeProfiles(run RunStart, point ProfilePoint, generation int64) []string {
	if len(h.p.RuntimeProfiles) == 0 || h.p.DataDir == "" {
		return nil
	}
	deadline := time.Now().Add(profileCaptureLease)
	var written []string
	for _, kind := range h.p.RuntimeProfiles {
		if time.Now().After(deadline) {
			log.Printf("isutools: %s %s profile skipped: capture lease exceeded", kind, point)
			break
		}
		name, err := h.writeRuntimeProfile(kind, run, point, generation)
		if err != nil {
			log.Printf("isutools: %s %s profile capture failed: %v", kind, point, err)
			continue
		}
		written = append(written, name)
	}
	return written
}

// writeRuntimeProfile publishes one profile atomically.
//
// The profile is written 0600 to a ".pprof.tmp" file and renamed only after a
// successful Close. /files/ serves neither that suffix nor a partial file, so
// a half-written profile is structurally impossible to download.
func (h *handler) writeRuntimeProfile(kind string, run RunStart, point ProfilePoint, generation int64) (string, error) {
	profile := rpprof.Lookup(kind)
	if profile == nil {
		return "", fmt.Errorf("unknown runtime profile %q", kind)
	}
	name := profileArtifactName(run, point, generation, kind)
	final := filepath.Join(h.p.DataDir, name)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", tmp, err)
	}
	if err := profile.WriteTo(f, 0); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("publish %s: %w", name, err)
	}
	return name, nil
}

// profileArtifactName builds "<ts>_gen<N>_<runid8>_<kind>_<point>.pprof".
//
// The timestamp is the run's opening boundary at both points, never the
// moment of the write: an opening and a closing artifact of the same run must
// share a filename prefix, or the pair that the difference is taken over
// cannot be reassembled from a directory listing.
func profileArtifactName(run RunStart, point ProfilePoint, generation int64, kind string) string {
	stamp := run.StartedAt
	if stamp.IsZero() {
		stamp = time.Now()
	}
	return fmt.Sprintf("%s_gen%d_%s_%s_%s.pprof",
		stamp.In(reportTZ).Format("20060102-150405"),
		generation, runIDPrefix(run.RunID), sanitizeName(kind), point)
}

// runIDPrefix shortens a run id to the leading 8 characters used in artifact
// names. Uniqueness comes from the whole prefix (timestamp, generation and
// id together), so the short form only has to make two runs of one second
// distinguishable.
func runIDPrefix(runID string) string {
	name := sanitizeName(runID)
	if name == "" {
		return "norun"
	}
	if len(name) > 8 {
		name = name[:8]
	}
	return name
}

// listProfiles returns captured .pprof names, newest first.
func (h *handler) listProfiles() []string {
	if h.p.DataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(h.p.DataDir)
	if err != nil {
		return nil
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pprof") {
			names = append(names, e.Name())
		}
	}
	for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
		names[i], names[j] = names[j], names[i]
	}
	return names
}
