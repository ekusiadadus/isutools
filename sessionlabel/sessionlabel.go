// Package sessionlabel provides a framework-neutral trusted edge adapter for
// pseudonymising an application session before nginx writes access logs.
package sessionlabel

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
)

const (
	HeaderName      = "X-Isutools-Session"
	EnvSourceCookie = "ISUTOOLS_SESSION_COOKIE"
	EnvHMACKey      = "ISUTOOLS_SESSION_HMAC_KEY"
	MinKeyBytes     = 32
	LabelBytes      = 18
	MaxSourceBytes  = 4096
)

var cookieNamePattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]{1,128}$`)

// Health is bounded configuration state. It never includes a cookie, key, or
// raw environment value.
type Health struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

// Adapter is immutable and safe for concurrent requests.
type Adapter struct {
	cookie string
	key    []byte
	health Health
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
		a.health = Health{Enabled: true, Reason: "enabled"}
	}
	return a
}

// FromEnv resolves the adapter without retaining the getenv callback.
func FromEnv(getenv func(string) string) *Adapter {
	if getenv == nil {
		return New("", nil)
	}
	return New(getenv(EnvSourceCookie), []byte(getenv(EnvHMACKey)))
}

func (a *Adapter) Health() Health {
	if a == nil {
		return Health{Reason: "adapter-nil"}
	}
	return a.health
}

// Label returns a fixed-length URL-safe pseudonym. False means fail closed.
func (a *Adapter) Label(source string) (string, bool) {
	if a == nil || !a.health.Enabled || source == "" || len(source) > MaxSourceBytes {
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(HeaderName)
		label := ""
		if a != nil && a.health.Enabled {
			if cookie, err := r.Cookie(a.cookie); err == nil {
				if trusted, ok := a.Label(cookie.Value); ok {
					label = trusted
				}
			}
		}
		writer := &trustedWriter{ResponseWriter: w, label: label}
		defer writer.commit()
		next.ServeHTTP(writer, r)
	})
}

// trustedWriter applies the trusted value at the commit boundary, after the
// application has had a chance to mutate headers. Unwrap keeps optional
// ResponseController operations available to streaming handlers.
type trustedWriter struct {
	http.ResponseWriter
	label string
	wrote bool
}

func (w *trustedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trustedWriter) commit() {
	if w.wrote {
		return
	}
	w.wrote = true
	w.Header().Del(HeaderName)
	if w.label != "" {
		w.Header().Set(HeaderName, w.label)
	}
}

func (w *trustedWriter) WriteHeader(status int) {
	w.commit()
	w.ResponseWriter.WriteHeader(status)
}

func (w *trustedWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *trustedWriter) ReadFrom(reader io.Reader) (int64, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if target, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return target.ReadFrom(reader)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, reader)
}

func (w *trustedWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *trustedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.commit()
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *trustedWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}
