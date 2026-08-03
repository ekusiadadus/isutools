package httpstats

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMiddlewareRecordsRoutePatternWithoutQuery(t *testing.T) {
	c := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello")
	})

	req := httptest.NewRequest(http.MethodGet, "https://example.test/users/123?token=secret", nil)
	req.Proto = "HTTP/2.0"
	req.ProtoMajor = 2
	req.ProtoMinor = 0
	rr := httptest.NewRecorder()
	c.Middleware(mux).ServeHTTP(rr, req)

	got := c.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() len = %d, want 1: %#v", len(got), got)
	}
	e := got[0]
	if e.Method != http.MethodGet || e.Path != "/users/{id}" || e.Protocol != "HTTP/2.0" || e.Status != http.StatusCreated {
		t.Errorf("identity = (%q, %q, %q, %d), want (GET, /users/{id}, HTTP/2.0, 201)", e.Method, e.Path, e.Protocol, e.Status)
	}
	if strings.Contains(e.Path, "token") || strings.Contains(e.Path, "secret") || strings.Contains(e.Key, "token") {
		t.Errorf("snapshot leaked query string: %#v", e)
	}
	if e.Count != 1 || e.TotalBytes != 5 || e.AvgBytes != 5 {
		t.Errorf("measurements = count %d, total bytes %d, avg bytes %d; want 1, 5, 5", e.Count, e.TotalBytes, e.AvgBytes)
	}
	if e.Total <= 0 || e.Avg <= 0 || e.Max <= 0 || e.P95 <= 0 {
		t.Errorf("durations must be recorded: %#v", e)
	}
}

func TestMiddlewareNormalizesFallbackPathAndAggregates(t *testing.T) {
	c := New()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("body"))
	})
	h := c.Middleware(next)

	for _, tc := range []struct {
		path string
		body string
	}{{"/image/123.jpg?x=one", "abc"}, {"/image/456.jpg?x=two", "defg"}} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("body", tc.body)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	got := c.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot() len = %d, want 1: %#v", len(got), got)
	}
	if got[0].Path != "/image/*.jpg" || got[0].Count != 2 || got[0].TotalBytes != 7 || got[0].AvgBytes != 3 {
		t.Errorf("entry = %#v, want normalized aggregate with 2 calls and 7 bytes", got[0])
	}
}

func TestWithPathRulesOverridesDefaultNormalization(t *testing.T) {
	c := New(WithPathRules([]Rule{{
		Pattern:     regexp.MustCompile(`/tenant/[^/]+`),
		Replacement: "/tenant/:tenant",
	}}))
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/tenant/acme/widgets/123", nil))

	got := c.Snapshot()
	if len(got) != 1 || got[0].Path != "/tenant/:tenant/widgets/123" {
		t.Fatalf("Snapshot() = %#v, want custom path rule to override defaults", got)
	}
}

func TestMiddlewareRecordsThenRepanics(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("application panic")
	}))

	func() {
		defer func() {
			if got := recover(); got != "application panic" {
				t.Fatalf("recovered panic = %#v, want application panic", got)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/panic", nil))
	}()

	got := c.Snapshot()
	if len(got) != 1 || got[0].Path != "/panic" || got[0].Status != http.StatusInternalServerError || got[0].Count != 1 {
		t.Fatalf("Snapshot() after panic = %#v, want one status 500 observation", got)
	}
}

func TestMiddlewarePreservesOptionalInterfaces(t *testing.T) {
	c := New()
	underlying := &allFeaturesWriter{plainWriter: plainWriter{header: make(http.Header)}}
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapped writer lost http.Flusher")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("wrapped writer lost http.Hijacker")
		}
		if _, ok := w.(http.Pusher); !ok {
			t.Error("wrapped writer lost http.Pusher")
		}
		rf, ok := w.(io.ReaderFrom)
		if !ok {
			t.Fatal("wrapped writer lost io.ReaderFrom")
		}
		if uw, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok || uw.Unwrap() != underlying {
			t.Errorf("Unwrap() = (%v, %v), want underlying writer", uw, ok)
		}
		w.(http.Flusher).Flush()
		_ = w.(http.Pusher).Push("/asset", nil)
		_, _ = rf.ReadFrom(readerOnly{Reader: strings.NewReader("stream")})
	}))
	h.ServeHTTP(underlying, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if !underlying.flushed || !underlying.pushed {
		t.Errorf("optional calls were not delegated: %#v", underlying)
	}
	got := c.Snapshot()
	if len(got) != 1 || got[0].TotalBytes != int64(len("stream")) {
		t.Fatalf("Snapshot() = %#v, want ReaderFrom bytes", got)
	}

	plain := &plainWriter{header: make(http.Header)}
	c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); ok {
			t.Error("wrapper invented http.Flusher")
		}
		if _, ok := w.(http.Hijacker); ok {
			t.Error("wrapper invented http.Hijacker")
		}
		if _, ok := w.(http.Pusher); ok {
			t.Error("wrapper invented http.Pusher")
		}
		if _, ok := w.(io.ReaderFrom); ok {
			t.Error("wrapper invented io.ReaderFrom")
		}
	})).ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/plain", nil))
}

func TestKeyCapUsesSingleOverflowEntryAndResetRestoresBudget(t *testing.T) {
	c := New(WithMaxKeys(2))
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, path := range []string{"/a", "/b", "/c", "/d"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	got := c.Snapshot()
	if len(got) != 3 {
		t.Fatalf("Snapshot() len = %d, want two keys plus overflow: %#v", len(got), got)
	}
	var overflow Entry
	for _, e := range got {
		if e.Path == OverflowPath {
			overflow = e
		}
	}
	if overflow.Count != 2 {
		t.Fatalf("overflow = %#v, want count 2", overflow)
	}

	c.Reset()
	if got := c.Snapshot(); len(got) != 0 {
		t.Fatalf("Snapshot() after Reset = %#v, want empty", got)
	}
	for _, path := range []string{"/x", "/y"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	for _, e := range c.Snapshot() {
		if e.Path == OverflowPath {
			t.Fatalf("fresh keys overflowed after Reset: %#v", c.Snapshot())
		}
	}
}

func TestSnapshotSortsByTotalDuration(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(2 * time.Millisecond)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/fast", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))

	got := c.Snapshot()
	if len(got) != 2 || got[0].Path != "/slow" {
		t.Fatalf("Snapshot() = %#v, want total duration descending", got)
	}
}

func TestResetWaitsForInflightRequestAndReturnsOldGeneration(t *testing.T) {
	c := New()
	started := make(chan struct{})
	release := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/old", nil))
	}()
	<-started

	resetDone := make(chan Snapshot, 1)
	go func() { resetDone <- c.Reset() }()
	select {
	case got := <-resetDone:
		t.Fatalf("Reset returned before the in-flight request completed: %#v", got)
	case <-time.After(10 * time.Millisecond):
	}

	close(release)
	<-requestDone
	old := <-resetDone
	if len(old) != 1 || old[0].Path != "/old" || old[0].Count != 1 {
		t.Fatalf("Reset() = %#v, want completed old generation", old)
	}
	if current := c.Snapshot(); len(current) != 0 {
		t.Fatalf("current Snapshot() = %#v, want a fresh generation", current)
	}
}

type plainWriter struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *plainWriter) Header() http.Header    { return w.header }
func (w *plainWriter) WriteHeader(status int) { w.status = status }
func (w *plainWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

type allFeaturesWriter struct {
	plainWriter
	flushed bool
	pushed  bool
}

func (w *allFeaturesWriter) Flush() { w.flushed = true }
func (w *allFeaturesWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}
func (w *allFeaturesWriter) Push(string, *http.PushOptions) error {
	w.pushed = true
	return nil
}
func (w *allFeaturesWriter) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(&w.plainWriter.body, r)
}

type readerOnly struct{ io.Reader }
