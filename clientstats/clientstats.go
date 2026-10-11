// Package clientstats attributes outgoing HTTP transport attempts to the
// requestwork counter carried by a request context.
package clientstats

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/requestwork"
)

type transport struct {
	base  http.RoundTripper
	label string
}

// WrapTransport instruments each RoundTrip, including each redirect attempt.
// label must be a static, low-cardinality caller-supplied value. It never
// records URLs, headers, request bodies or error strings. Timing and errors
// freeze at the first EOF, Close or read error; a later Close error is not counted.
func WrapTransport(base http.RoundTripper, label string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &transport{base: base, label: label}
}

// CloseIdleConnections preserves http.Client.CloseIdleConnections behavior.
func (t *transport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if !requestwork.Enabled(ctx) {
		return t.base.RoundTrip(req)
	}
	r := &recorder{}
	start := time.Now()
	trace := &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { r.begin(&r.dns) },
		DNSDone:           func(httptrace.DNSDoneInfo) { r.end(&r.dns) },
		ConnectStart:      func(_, _ string) { r.begin(&r.connect) },
		ConnectDone:       func(_, _ string, _ error) { r.end(&r.connect) },
		TLSHandshakeStart: func() { r.begin(&r.tls) },
		TLSHandshakeDone:  func(_ tls.ConnectionState, _ error) { r.end(&r.tls) },
		GetConn:           func(_ string) { r.begin(&r.acquire) },
		GotConn: func(info httptrace.GotConnInfo) {
			r.end(&r.acquire)
			r.mu.Lock()
			r.reused = info.Reused
			r.mu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			r.mu.Lock()
			if r.wrote.IsZero() {
				r.wrote = time.Now()
			}
			r.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			r.mu.Lock()
			if r.firstByte.IsZero() {
				r.firstByte = time.Now()
			}
			r.mu.Unlock()
		},
	}
	// WithClientTrace composes callbacks with a trace already attached to req.
	response, err := t.base.RoundTrip(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
	headersEnd := time.Now()
	if err != nil {
		r.finish(ctx, t.label, start, headersEnd, headersEnd, true)
		return response, err
	}
	failed := response != nil && response.StatusCode >= 400
	if response == nil || response.Body == nil || response.Body == http.NoBody {
		r.finish(ctx, t.label, start, headersEnd, headersEnd, failed)
		return response, nil
	}
	response.Body = &body{ReadCloser: response.Body, recorder: r, ctx: ctx, label: t.label, start: start, headersEnd: headersEnd, failed: failed}
	return response, nil
}

// phase measures the union of potentially concurrent operations (for example
// Happy Eyeballs connection attempts), so overlapping attempts are not summed.
type phase struct {
	active int
	start  time.Time
	total  time.Duration
}

func (p *phase) elapsed(now time.Time) time.Duration {
	if p.active > 0 {
		return p.total + now.Sub(p.start)
	}
	return p.total
}

type recorder struct {
	mu                         sync.Mutex
	acquire, dns, connect, tls phase
	wrote, firstByte           time.Time
	reused                     bool
	once                       sync.Once
}

func (r *recorder) begin(p *phase) {
	r.mu.Lock()
	if p.active == 0 {
		p.start = time.Now()
	}
	p.active++
	r.mu.Unlock()
}
func (r *recorder) end(p *phase) {
	r.mu.Lock()
	if p.active > 0 {
		p.active--
		if p.active == 0 {
			p.total += time.Since(p.start)
			p.start = time.Time{}
		}
	}
	r.mu.Unlock()
}

func (r *recorder) finish(ctx context.Context, label string, start, headersEnd, end time.Time, failed bool) {
	r.once.Do(func() {
		r.mu.Lock()
		e := requestwork.External{Label: label, Start: start, End: end, Failed: failed, Reused: r.reused, Acquire: r.acquire.elapsed(end), DNS: r.dns.elapsed(end), Connect: r.connect.elapsed(end), TLS: r.tls.elapsed(end), Headers: headersEnd.Sub(start), Body: end.Sub(headersEnd)}
		if !r.wrote.IsZero() && !r.firstByte.IsZero() && r.firstByte.After(r.wrote) {
			e.Wait = r.firstByte.Sub(r.wrote)
		}
		r.mu.Unlock()
		requestwork.CompletedExternal(ctx, e)
	})
}

type body struct {
	io.ReadCloser
	recorder          *recorder
	ctx               context.Context
	label             string
	start, headersEnd time.Time
	failed            bool
}

func (b *body) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.recorder.finish(b.ctx, b.label, b.start, b.headersEnd, time.Now(), b.failed || err != io.EOF)
	}
	return n, err
}

func (b *body) Close() error {
	err := b.ReadCloser.Close()
	b.recorder.finish(b.ctx, b.label, b.start, b.headersEnd, time.Now(), b.failed || err != nil)
	return err
}
