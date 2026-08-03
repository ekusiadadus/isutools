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
