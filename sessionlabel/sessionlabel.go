// Package sessionlabel provides a framework-neutral trusted edge adapter for
// pseudonymising an application session before nginx writes access logs.
package sessionlabel

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/httpstats"
	writercap "github.com/ekusiadadus/isutools/internal/httpwriter"
)

const (
	HeaderName                = "X-Isutools-Session"
	ScenarioHeaderName        = "X-Isutools-Scenario"
	TrustedSessionHeaderName  = "X-Isutools-Trusted-Session"
	TrustedScenarioHeaderName = "X-Isutools-Trusted-Scenario"
	EnvGlobalMode             = "ISUTOOLS"
	EnvFlowLabels             = "ISUTOOLS_FLOW_LABELS"
	EnvSourceCookie           = "ISUTOOLS_SESSION_COOKIE"
	EnvHMACKey                = "ISUTOOLS_SESSION_HMAC_KEY"
	EnvScenario               = "ISUTOOLS_SCENARIO"
	EnvTrustInbound           = "ISUTOOLS_TRUST_INBOUND_FLOW_LABELS"
	MinKeyBytes               = 32
	LabelBytes                = 18
	MaxSourceBytes            = 4096
	MaxScenarioBytes          = 64
	MaxTrustedSessionBytes    = 128
)

var cookieNamePattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]{1,128}$`)
var flowLabelPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// Health is bounded configuration state. It never includes a cookie, key, or
// raw environment value.
type Health struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

// Adapter is immutable and safe for concurrent requests.
type Adapter struct {
	cookie         string
	key            []byte
	staticScenario string
	trustInbound   bool
	sessionEnabled bool
	bypass         bool
	health         Health
	observer       Observer
}

// Observer receives only the HMAC pseudonym, bounded scenario, method, and
// registered route template after the application handler completes.
type Observer interface {
	Observe(session, scenario, method, route string)
}

// Observation is the richer, still secret-free flow event available to
// observers that want latency and status overlays. At is the request start;
// Session is already an HMAC pseudonym and Route is a registered template.
type Observation struct {
	Session  string
	Scenario string
	Method   string
	Route    string
	Status   int
	Duration time.Duration
	At       time.Time
}

// DetailedObserver is optional so existing Observer implementations remain
// source compatible. Middleware calls exactly one of ObserveRequest/Observe.
type DetailedObserver interface {
	ObserveRequest(Observation)
}

// WithObserver returns a shallow copy with one run-aligned flow sink.
func (a *Adapter) WithObserver(observer Observer) *Adapter {
	if a == nil {
		return a
	}
	copy := *a
	copy.observer = observer
	return &copy
}

// New validates the source cookie and HMAC key. Invalid configuration is a
// fail-closed adapter that strips spoofed labels but emits no replacement.
func New(cookieName string, key []byte) *Adapter {
	cookieName = strings.TrimSpace(cookieName)
	a := &Adapter{health: Health{Reason: "disabled"}}
	switch {
	case cookieName == "":
		a.health.Reason = "cookie-unset"
	case !cookieNamePattern.MatchString(cookieName):
		a.health.Reason = "cookie-invalid"
	case len(key) < MinKeyBytes:
		a.health.Reason = "key-invalid"
	default:
		a.cookie = cookieName
		a.key = append([]byte(nil), key...)
		a.sessionEnabled = true
		a.health = Health{Enabled: true, Reason: "enabled"}
	}
	return a
}

// FromEnv resolves the adapter without retaining the getenv callback.
func FromEnv(getenv func(string) string) *Adapter {
	if getenv == nil {
		return New("", nil)
	}
	if disabledValue(getenv(EnvGlobalMode)) {
		return &Adapter{bypass: true, health: Health{Reason: "global-off"}}
	}
	mode := strings.ToLower(strings.TrimSpace(getenv(EnvFlowLabels)))
	if disabledValue(mode) {
		return &Adapter{bypass: true, health: Health{Reason: "flow-labels-off"}}
	}
	cookie := getenv(EnvSourceCookie)
	key := getenv(EnvHMACKey)
	scenario := strings.TrimSpace(getenv(EnvScenario))
	trust := enabledValue(getenv(EnvTrustInbound))
	if (mode == "" || mode == "auto") && cookie == "" && key == "" && scenario == "" && !trust {
		return &Adapter{bypass: true, health: Health{Reason: "auto-unconfigured"}}
	}
	a := New(cookie, []byte(key))
	if mode != "" && mode != "auto" && !enabledValue(mode) {
		a.cookie = ""
		a.key = nil
		a.sessionEnabled = false
		a.staticScenario = ""
		a.trustInbound = false
		a.health = Health{Reason: "mode-invalid"}
		return a
	}
	if scenario != "" {
		if validScenario(scenario) {
			a.staticScenario = scenario
		} else {
			a.health.Reason = "scenario-invalid"
		}
	}
	a.trustInbound = trust
	if trust && !a.sessionEnabled {
		a.health = Health{Enabled: true, Reason: "trusted-inbound"}
	}
	return a
}

func (a *Adapter) Health() Health {
	if a == nil {
		return Health{Reason: "adapter-nil"}
	}
	return a.health
}

// Label returns a fixed-length URL-safe pseudonym. False means fail closed.
func (a *Adapter) Label(source string) (string, bool) {
	if a == nil || !a.sessionEnabled || source == "" || len(source) > MaxSourceBytes {
		return "", false
	}
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte(source))
	sum := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:LabelBytes]), true
}

// Middleware always removes an untrusted client label. When the configured
// source cookie is present, it writes only its pseudonym to the trusted
// upstream response header consumed by nginx.
func (a *Adapter) Middleware(next http.Handler) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	if a != nil && a.bypass {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Time{}
		if a != nil && a.observer != nil {
			if _, ok := a.observer.(DetailedObserver); ok {
				started = time.Now()
			}
		}
		trustedSession := r.Header.Get(TrustedSessionHeaderName)
		trustedScenario := r.Header.Get(TrustedScenarioHeaderName)
		r.Header.Del(HeaderName)
		r.Header.Del(ScenarioHeaderName)
		r.Header.Del(TrustedSessionHeaderName)
		r.Header.Del(TrustedScenarioHeaderName)
		label := ""
		if a != nil && a.sessionEnabled {
			if cookie, err := r.Cookie(a.cookie); err == nil {
				if trusted, ok := a.Label(cookie.Value); ok {
					label = trusted
				}
			}
		}
		state := &scenarioState{}
		if a != nil {
			state.label = a.staticScenario
			if a.trustInbound {
				if label == "" && validTrustedSession(trustedSession) {
					label = trustedSession
				}
				if validScenario(trustedScenario) {
					state.label = trustedScenario
				}
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), scenarioContextKey{}, state))
		writer := &trustedWriter{ResponseWriter: w, label: label, scenario: state}
		defer writer.commit()
		next.ServeHTTP(preserveOptionalInterfaces(writer), r)
		writer.commit()
		if a != nil && a.observer != nil && label != "" {
			observation := Observation{
				Session: label, Scenario: state.get(), Method: r.Method, Route: httpstats.RoutePattern(r),
				Status: writer.status, Duration: time.Since(started), At: started,
			}
			if detailed, ok := a.observer.(DetailedObserver); ok {
				observeDetailedSafely(detailed, observation)
			} else {
				observeSafely(a.observer, observation.Session, observation.Scenario, observation.Method, observation.Route)
			}
		}
	})
}

func observeSafely(observer Observer, session, scenario, method, route string) {
	defer func() { _ = recover() }()
	observer.Observe(session, scenario, method, route)
}

func observeDetailedSafely(observer DetailedObserver, observation Observation) {
	defer func() { _ = recover() }()
	observer.ObserveRequest(observation)
}

// SetScenario assigns a bounded, non-secret scenario to the current request.
// It succeeds only when the request is inside Adapter.Middleware. Invalid
// labels clear any static fallback so a bad value cannot be misclassified.
func SetScenario(r *http.Request, scenario string) bool {
	if r == nil {
		return false
	}
	state, ok := r.Context().Value(scenarioContextKey{}).(*scenarioState)
	if !ok || state == nil {
		return false
	}
	if !validScenario(scenario) {
		state.set("")
		return false
	}
	state.set(scenario)
	return true
}

// Scenario returns framework-neutral middleware for assigning one explicit
// scenario to a route or route group.
func Scenario(scenario string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if next == nil {
			next = http.NotFoundHandler()
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SetScenario(r, scenario)
			next.ServeHTTP(w, r)
		})
	}
}

type scenarioContextKey struct{}

type scenarioState struct {
	mu    sync.RWMutex
	label string
}

func (s *scenarioState) set(label string) {
	s.mu.Lock()
	s.label = label
	s.mu.Unlock()
}

func (s *scenarioState) get() string {
	s.mu.RLock()
	label := s.label
	s.mu.RUnlock()
	return label
}

func validScenario(value string) bool {
	return value != "" && len(value) <= MaxScenarioBytes && flowLabelPattern.MatchString(value)
}

func validTrustedSession(value string) bool {
	return value != "" && len(value) <= MaxTrustedSessionBytes && flowLabelPattern.MatchString(value)
}

func disabledValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "0", "false", "no", "disabled":
		return true
	default:
		return false
	}
}

func enabledValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "1", "true", "yes", "enabled":
		return true
	default:
		return false
	}
}

// trustedWriter applies the trusted value at the commit boundary, after the
// application has had a chance to mutate headers. Unwrap keeps optional
// ResponseController operations available to streaming handlers.
type trustedWriter struct {
	http.ResponseWriter
	label    string
	scenario *scenarioState
	wrote    bool
	status   int
}

func (w *trustedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trustedWriter) commit() {
	if w.wrote {
		return
	}
	w.wrote = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.apply()
}

func (w *trustedWriter) apply() {
	w.Header().Del(HeaderName)
	w.Header().Del(ScenarioHeaderName)
	if w.label != "" {
		w.Header().Set(HeaderName, w.label)
	}
	if w.scenario != nil {
		if scenario := w.scenario.get(); scenario != "" {
			w.Header().Set(ScenarioHeaderName, scenario)
		}
	}
}

func (w *trustedWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.apply()
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.commit()
	w.ResponseWriter.WriteHeader(status)
}

func (w *trustedWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

type flushFeature struct {
	w *trustedWriter
	f http.Flusher
}

func (f flushFeature) Flush() {
	f.w.commit()
	f.f.Flush()
}

type hijackFeature struct {
	w *trustedWriter
	h http.Hijacker
}

func (h hijackFeature) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.w.commit()
	return h.h.Hijack()
}

type pushFeature struct{ p http.Pusher }

func (p pushFeature) Push(target string, options *http.PushOptions) error {
	return p.p.Push(target, options)
}

type readFromFeature struct {
	w  *trustedWriter
	rf io.ReaderFrom
}

type closeNotifyFeature struct{ c writercap.CloseNotifier }

func (c closeNotifyFeature) CloseNotify() <-chan bool { return c.c.CloseNotify() }

func (r readFromFeature) ReadFrom(reader io.Reader) (int64, error) {
	r.w.commit()
	return r.rf.ReadFrom(reader)
}

// preserveOptionalInterfaces exposes exactly the capabilities of the real
// writer. Framework feature detection must not be changed by instrumentation.
func preserveOptionalInterfaces(w *trustedWriter) http.ResponseWriter {
	features := writercap.Features{}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		features.Flusher = flushFeature{w: w, f: f}
	}
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		features.Hijacker = hijackFeature{w: w, h: h}
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
