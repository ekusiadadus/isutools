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

type recordingEventObserver struct {
	starts, finishes, cancels int
	key                       string
	failed                    bool
}

func (o *recordingEventObserver) HTTPStart(time.Time) any { o.starts++; return "token" }
func (o *recordingEventObserver) HTTPFinish(_ time.Time, token any, key string, _ time.Duration, failed bool) {
	if token != "token" {
		panic("wrong token")
	}
	o.finishes++
	o.key, o.failed = key, failed
}
func (o *recordingEventObserver) HTTPCancel(_ time.Time, token any) {
	if token != "token" {
		panic("wrong token")
	}
	o.cancels++
}

type panickingEventObserver struct{}

func (panickingEventObserver) HTTPStart(time.Time) any { panic("observer") }
func (panickingEventObserver) HTTPFinish(time.Time, any, string, time.Duration, bool) {
	panic("observer")
}
func (panickingEventObserver) HTTPCancel(time.Time, any) { panic("observer") }

func TestEventObserverReceivesOnlySafeRouteIdentity(t *testing.T) {
	c := New()
	observer := &recordingEventObserver{}
	c.SetEventObserver(observer)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reset-password/{account}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "failed", http.StatusServiceUnavailable)
	})
	c.Middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(
		http.MethodPost, "/reset-password/alice@example.com?token=secret", nil))
	if observer.starts != 1 || observer.finishes != 1 || observer.cancels != 0 || !observer.failed {
		t.Fatalf("observer counts = %#v", observer)
	}
	if observer.key != "POST /reset-password/{account}" || strings.Contains(observer.key, "alice") || strings.Contains(observer.key, "secret") {
		t.Fatalf("observer key = %q", observer.key)
	}

	observer = &recordingEventObserver{}
	c.SetEventObserver(observer)
	c.Middleware(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/invite/secret-slug", nil))
	if observer.key != "GET (unmatched)" {
		t.Fatalf("heuristic fallback reached event observer: %q", observer.key)
	}

	rules, err := ParseSafeProfileRouteRules(`^/invite/[^/]+$=/invite/{token}`)
	if err != nil {
		t.Fatal(err)
	}
	observer = &recordingEventObserver{}
	c.SetEventRouteRules(rules)
	c.SetEventObserver(observer)
	c.Middleware(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/invite/secret-slug", nil))
	if observer.key != "GET /invite/{token}" || strings.Contains(observer.key, "secret") {
		t.Fatalf("safe constant event rule = %q", observer.key)
	}
}

func TestEventObserverPanicCannotBreakRequest(t *testing.T) {
	c := New()
	c.SetEventObserver(panickingEventObserver{})
	recorder := httptest.NewRecorder()
	c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })).ServeHTTP(
		recorder, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if recorder.Code != http.StatusCreated || len(c.Snapshot()) != 1 {
		t.Fatalf("response=%d snapshot=%#v", recorder.Code, c.Snapshot())
	}
}

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

func TestMiddlewareNormalizesULIDPathAndAggregates(t *testing.T) {
	c := New()
	h := c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for _, path := range []string{
		"/api/app/rides/01ARZ3NDEKTSV4RRFFQ69G5FAV/evaluation",
		"/api/app/rides/01BX5ZZKBKACTAV9WEVGEMMVRZ/evaluation",
		"/api/app/rides/01arz3ndektsv4rrffq69g5fav/evaluation",
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
	}

	got := c.Snapshot()
	if len(got) != 1 || got[0].Path != "/api/app/rides/*/evaluation" || got[0].Count != 3 {
		t.Fatalf("Snapshot() = %#v, want upper- and lower-case ULID routes aggregated under one path", got)
	}
}

func TestNormalizePathPreservesNonULIDLength26Segments(t *testing.T) {
	for _, path := range []string{
		"/objects/81ARZ3NDEKTSV4RRFFQ69G5FAV", // first character exceeds ULID range
		"/objects/01ARZ3NDEKTSV4RRFFQ69G5FAI", // I is not Crockford Base32
	} {
		if got := normalizePath(path); got != path {
			t.Errorf("normalizePath(%q) = %q, want path preserved", path, got)
		}
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

func TestP95SaturatedBucketUsesObservedMaximum(t *testing.T) {
	s := &stat{count: 1, max: int64(20 * time.Minute)}
	s.buckets[numBuckets-1] = 1
	if got := time.Duration(p95(s)); got != 20*time.Minute {
		t.Fatalf("saturated p95 = %v, want observed maximum", got)
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
	return io.Copy(&w.body, r)
}

type readerOnly struct{ io.Reader }
