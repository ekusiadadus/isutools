package sessionlabel

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestTrustedSessionLabelsAreStableDistinctAndSafe(t *testing.T) {
	a := New("isucon_session", []byte(strings.Repeat("k", MinKeyBytes)))
	one, ok := a.Label("raw-session-one@example.invalid")
	if !ok {
		t.Fatal("label disabled")
	}
	again, _ := a.Label("raw-session-one@example.invalid")
	two, _ := a.Label("raw-session-two@example.invalid")
	if one != again || one == two {
		t.Fatalf("one=%q again=%q two=%q", one, again, two)
	}
	if len(one) != 24 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(one) {
		t.Fatalf("unsafe label %q", one)
	}
}

func TestMiddlewareOverwritesSpoofedClientHeader(t *testing.T) {
	a := New("session", []byte(strings.Repeat("s", MinKeyBytes)))
	var requestHeader string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHeader = r.Header.Get(HeaderName)
		w.Header().Set(HeaderName, "malicious-app-overwrite")
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, "attacker-label")
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw-source-secret"})
	rec := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rec, req)
	if requestHeader != "" {
		t.Fatalf("downstream saw spoofed header %q", requestHeader)
	}
	got := rec.Header().Get(HeaderName)
	if got == "" || got == "attacker-label" || got == "malicious-app-overwrite" || strings.Contains(got, "raw-source") {
		t.Fatalf("trusted response label = %q", got)
	}
}

func TestMissingKeyFailsClosedWithBoundedHealth(t *testing.T) {
	a := New("session", []byte("raw-secret-too-short"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, "attacker-label")
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw-cookie-secret"})
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderName) != "" {
			t.Error("spoofed header survived")
		}
	})).ServeHTTP(rec, req)
	if rec.Header().Get(HeaderName) != "" || a.Health() != (Health{Reason: "key-invalid"}) {
		t.Fatalf("header=%q health=%#v", rec.Header().Get(HeaderName), a.Health())
	}
}

func TestOversizedSourceFailsClosed(t *testing.T) {
	a := New("session", []byte(strings.Repeat("k", MinKeyBytes)))
	if label, ok := a.Label(strings.Repeat("x", MaxSourceBytes+1)); ok || label != "" {
		t.Fatalf("oversized label=%q ok=%v", label, ok)
	}
}

func TestFromEnvGlobalAndFlowOffReturnOriginalHandler(t *testing.T) {
	for _, values := range []map[string]string{
		{EnvGlobalMode: "off", EnvFlowLabels: "on"},
		{EnvGlobalMode: "on", EnvFlowLabels: "off"},
		{EnvGlobalMode: "on"},
	} {
		a := FromEnv(mapEnv(values))
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
		got := a.Middleware(next)
		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(next).Pointer() {
			t.Fatalf("values=%v added a request wrapper while disabled: health=%+v", values, a.Health())
		}
	}
}

func TestFromEnvEmitsTrustedSessionAndStaticScenario(t *testing.T) {
	a := FromEnv(mapEnv(map[string]string{
		EnvGlobalMode:   "on",
		EnvFlowLabels:   "on",
		EnvSourceCookie: "SESSIONID",
		EnvHMACKey:      strings.Repeat("k", MinKeyBytes),
		EnvScenario:     "isucon13_official",
	}))
	var requestSession, requestScenario string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSession = r.Header.Get(HeaderName)
		requestScenario = r.Header.Get(ScenarioHeaderName)
		w.Header().Set(HeaderName, "malicious-app-session")
		w.Header().Set(ScenarioHeaderName, "malicious app scenario")
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, "spoofed-session")
	req.Header.Set(ScenarioHeaderName, "spoofed-scenario")
	req.AddCookie(&http.Cookie{Name: "SESSIONID", Value: "raw-cookie-secret"})
	rec := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rec, req)

	if requestSession != "" || requestScenario != "" {
		t.Fatalf("downstream saw untrusted headers session=%q scenario=%q", requestSession, requestScenario)
	}
	session := rec.Header().Get(HeaderName)
	if session == "" || session == "spoofed-session" || strings.Contains(session, "raw-cookie") {
		t.Fatalf("response session label = %q", session)
	}
	if got := rec.Header().Get(ScenarioHeaderName); got != "isucon13_official" {
		t.Fatalf("response scenario = %q", got)
	}
}

func TestRequestScenarioOverridesStaticScenario(t *testing.T) {
	a := FromEnv(mapEnv(map[string]string{
		EnvFlowLabels:   "on",
		EnvSourceCookie: "session",
		EnvHMACKey:      strings.Repeat("s", MinKeyBytes),
		EnvScenario:     "static",
	}))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !SetScenario(r, "viewer") {
			t.Error("SetScenario did not find flow middleware state")
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw"})
	rec := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rec, req)
	if got := rec.Header().Get(ScenarioHeaderName); got != "viewer" {
		t.Fatalf("scenario = %q, want request override", got)
	}
}

func TestInvalidRequestScenarioFailsClosed(t *testing.T) {
	a := FromEnv(mapEnv(map[string]string{
		EnvFlowLabels:   "on",
		EnvSourceCookie: "session",
		EnvHMACKey:      strings.Repeat("s", MinKeyBytes),
		EnvScenario:     "static",
	}))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SetScenario(r, "raw bearer token with spaces") {
			t.Error("unsafe scenario was accepted")
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw"})
	rec := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(rec, req)
	if got := rec.Header().Get(ScenarioHeaderName); got != "" {
		t.Fatalf("unsafe scenario fell back to %q", got)
	}
}

func TestInformationalResponseCannotBypassFinalTrustedHeaders(t *testing.T) {
	a := FromEnv(mapEnv(map[string]string{
		EnvFlowLabels:   "on",
		EnvSourceCookie: "session",
		EnvHMACKey:      strings.Repeat("i", MinKeyBytes),
		EnvScenario:     "initial",
	}))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderName, "app-before-early-hints")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set(HeaderName, "app-before-final")
		if !SetScenario(r, "final") {
			t.Error("SetScenario failed after informational response")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw"})
	rec := &informationalWriter{header: make(http.Header)}
	a.Middleware(next).ServeHTTP(rec, req)
	if len(rec.responses) != 2 {
		t.Fatalf("responses=%v, want informational and final", rec.responses)
	}
	for _, response := range rec.responses {
		if got := response.header.Get(HeaderName); got == "" || strings.HasPrefix(got, "app-") {
			t.Fatalf("status=%d session=%q", response.status, got)
		}
	}
	if got := rec.responses[1].header.Get(ScenarioHeaderName); got != "final" {
		t.Fatalf("final scenario=%q", got)
	}
}

func TestTrustedInboundLabelsRequireExplicitOptIn(t *testing.T) {
	request := func(trust string) http.Header {
		a := FromEnv(mapEnv(map[string]string{
			EnvFlowLabels:   "on",
			EnvTrustInbound: trust,
		}))
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, name := range []string{HeaderName, ScenarioHeaderName, TrustedSessionHeaderName, TrustedScenarioHeaderName} {
				if got := r.Header.Get(name); got != "" {
					t.Errorf("downstream saw %s=%q", name, got)
				}
			}
			w.WriteHeader(http.StatusOK)
		})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(HeaderName, "public-spoof")
		req.Header.Set(ScenarioHeaderName, "public-spoof")
		req.Header.Set(TrustedSessionHeaderName, "k6-vu-1-iter-2")
		req.Header.Set(TrustedScenarioHeaderName, "login_and_browse")
		rec := httptest.NewRecorder()
		a.Middleware(next).ServeHTTP(rec, req)
		return rec.Header()
	}

	withoutTrust := request("")
	if withoutTrust.Get(HeaderName) != "" || withoutTrust.Get(ScenarioHeaderName) != "" {
		t.Fatalf("trusted-edge labels accepted without opt-in: %v", withoutTrust)
	}
	withTrust := request("1")
	if withTrust.Get(HeaderName) != "k6-vu-1-iter-2" || withTrust.Get(ScenarioHeaderName) != "login_and_browse" {
		t.Fatalf("trusted-edge labels = %v", withTrust)
	}
}

func TestUnknownFlowModeFailsClosed(t *testing.T) {
	a := FromEnv(mapEnv(map[string]string{
		EnvFlowLabels:   "maybe",
		EnvSourceCookie: "session",
		EnvHMACKey:      strings.Repeat("k", MinKeyBytes),
		EnvScenario:     "static",
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw"})
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)
	if rec.Header().Get(HeaderName) != "" || rec.Header().Get(ScenarioHeaderName) != "" {
		t.Fatalf("unknown mode emitted flow labels: %v", rec.Header())
	}
	if got := a.Health(); got.Enabled || got.Reason != "mode-invalid" {
		t.Fatalf("health=%+v", got)
	}
}

func TestMiddlewarePreservesOnlyUnderlyingOptionalInterfaces(t *testing.T) {
	a := New("session", []byte(strings.Repeat("k", MinKeyBytes)))
	plain := &plainWriter{header: make(http.Header)}
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
		if _, ok := w.(interface{ CloseNotify() <-chan bool }); ok {
			t.Error("wrapper invented http.CloseNotifier")
		}
	})).ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/", nil))

	featured := &featureWriter{plainWriter: plainWriter{header: make(http.Header)}}
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapper lost http.Flusher")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("wrapper lost http.Hijacker")
		}
		if _, ok := w.(http.Pusher); !ok {
			t.Error("wrapper lost http.Pusher")
		}
		if _, ok := w.(io.ReaderFrom); !ok {
			t.Error("wrapper lost io.ReaderFrom")
		}
		if _, ok := w.(interface{ CloseNotify() <-chan bool }); !ok {
			t.Error("wrapper lost http.CloseNotifier")
		}
	})).ServeHTTP(featured, httptest.NewRequest(http.MethodGet, "/", nil))
}

type plainWriter struct {
	header http.Header
	status int
}

func (w *plainWriter) Header() http.Header         { return w.header }
func (w *plainWriter) WriteHeader(status int)      { w.status = status }
func (w *plainWriter) Write(p []byte) (int, error) { return len(p), nil }

type featureWriter struct{ plainWriter }

func (*featureWriter) Flush() {}
func (*featureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}
func (*featureWriter) Push(string, *http.PushOptions) error { return nil }
func (*featureWriter) CloseNotify() <-chan bool             { return nil }
func (*featureWriter) ReadFrom(r io.Reader) (int64, error)  { return io.Copy(io.Discard, r) }

type informationalWriter struct {
	header    http.Header
	responses []recordedResponse
}

type recordedResponse struct {
	status int
	header http.Header
}

func (w *informationalWriter) Header() http.Header { return w.header }
func (w *informationalWriter) WriteHeader(status int) {
	w.responses = append(w.responses, recordedResponse{status: status, header: w.header.Clone()})
}
func (w *informationalWriter) Write(p []byte) (int, error) { return len(p), nil }

func mapEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}
