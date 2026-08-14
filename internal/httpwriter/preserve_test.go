package httpwriter

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
)

func TestPreserveExposesExactlySelectedFeatures(t *testing.T) {
	base := &testWriter{header: make(http.Header)}
	all := &allFeatures{}
	for mask := 0; mask < 32; mask++ {
		features := Features{}
		if mask&1 != 0 {
			features.Flusher = all
		}
		if mask&2 != 0 {
			features.Hijacker = all
		}
		if mask&4 != 0 {
			features.Pusher = all
		}
		if mask&8 != 0 {
			features.ReaderFrom = all
		}
		if mask&16 != 0 {
			features.CloseNotifier = all
		}
		got := Preserve(base, base, features)
		assertFeature(t, mask, 1, "Flusher", func() bool { _, ok := got.(http.Flusher); return ok })
		assertFeature(t, mask, 2, "Hijacker", func() bool { _, ok := got.(http.Hijacker); return ok })
		assertFeature(t, mask, 4, "Pusher", func() bool { _, ok := got.(http.Pusher); return ok })
		assertFeature(t, mask, 8, "ReaderFrom", func() bool { _, ok := got.(io.ReaderFrom); return ok })
		assertFeature(t, mask, 16, "CloseNotifier", func() bool { _, ok := got.(CloseNotifier); return ok })
		if unwrapped, ok := got.(Unwrapper); !ok || unwrapped.Unwrap() != base {
			t.Fatalf("mask=%d Unwrap()=(%v,%v), want base", mask, unwrapped, ok)
		}
	}
}

func assertFeature(t *testing.T, mask, bit int, name string, present func() bool) {
	t.Helper()
	if got, want := present(), mask&bit != 0; got != want {
		t.Fatalf("mask=%d %s present=%v, want %v", mask, name, got, want)
	}
}

type testWriter struct{ header http.Header }

func (w *testWriter) Header() http.Header         { return w.header }
func (*testWriter) WriteHeader(int)               {}
func (*testWriter) Write(p []byte) (int, error)   { return len(p), nil }
func (w *testWriter) Unwrap() http.ResponseWriter { return w }

type allFeatures struct{}

func (*allFeatures) Flush() {}
func (*allFeatures) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}
func (*allFeatures) Push(string, *http.PushOptions) error { return nil }
func (*allFeatures) ReadFrom(r io.Reader) (int64, error)  { return io.Copy(io.Discard, r) }
func (*allFeatures) CloseNotify() <-chan bool             { return nil }
