package httpstats

import (
	"bufio"
	"io"
	"net"
	"net/http"

	writercap "github.com/ekusiadadus/isutools/internal/httpwriter"
)

// responseWriter captures status and byte counts. The middleware must hand the
// application a writer that exposes exactly the optional interfaces the real
// one has — no more, so feature detection stays honest, and no fewer, so
// streaming, hijacking and sendfile keep working (preserveOptionalInterfaces).
type responseWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	onCommit func(int)
	onHijack func(net.Conn) net.Conn
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	if w.onCommit != nil {
		w.onCommit(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.ensureStatus()
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *responseWriter) ensureStatus() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
}

type flushFeature struct {
	w *responseWriter
	f http.Flusher
}

func (f flushFeature) Flush() {
	f.w.ensureStatus()
	f.f.Flush()
}

type hijackFeature struct {
	h *responseWriter
	u http.Hijacker
}

func (h hijackFeature) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := h.u.Hijack()
	if err == nil && h.h.onHijack != nil {
		conn = h.h.onHijack(conn)
	}
	return conn, rw, err
}

type pushFeature struct{ p http.Pusher }

func (p pushFeature) Push(target string, opts *http.PushOptions) error { return p.p.Push(target, opts) }

type readFromFeature struct {
	w  *responseWriter
	rf io.ReaderFrom
}

type closeNotifyFeature struct{ c writercap.CloseNotifier }

func (c closeNotifyFeature) CloseNotify() <-chan bool { return c.c.CloseNotify() }

func (r readFromFeature) ReadFrom(src io.Reader) (int64, error) {
	r.w.ensureStatus()
	n, err := r.rf.ReadFrom(src)
	r.w.bytes += n
	return n, err
}

func preserveOptionalInterfaces(w *responseWriter) http.ResponseWriter {
	features := writercap.Features{}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		features.Flusher = flushFeature{w: w, f: f}
	}
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		features.Hijacker = hijackFeature{h: w, u: h}
	}
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		features.Pusher = pushFeature{p: p}
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		features.ReaderFrom = readFromFeature{w: w, rf: rf}
	}
	if c, ok := w.ResponseWriter.(writercap.CloseNotifier); ok {
		features.CloseNotifier = closeNotifyFeature{c: c}
	}
	return writercap.Preserve(w, w, features)
}
