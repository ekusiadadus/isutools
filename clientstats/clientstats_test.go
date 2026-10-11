package clientstats

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/requestwork"
)

func TestHTTPCompletionTraceAndReuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()
	var original atomic.Int64
	ctx, c := requestwork.WithCounter(httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { original.Add(1) }}))
	client := &http.Client{Transport: WrapTransport(http.DefaultTransport, "static-api")}
	start := time.Now()
	for i := 0; i < 2; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			_, err = io.Copy(io.Discard, resp.Body)
		} else {
			err = resp.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	s := c.Close(start, time.Now())
	if original.Load() != 2 || s.ExternalCount != 2 || s.ExternalErrors != 2 || len(s.Calls) != 1 || s.Calls[0].Count != 2 || s.Calls[0].Reused < 1 || s.Calls[0].Headers <= 0 || s.Calls[0].Body < 0 {
		t.Fatalf("trace/completion: %+v, original=%d", s, original.Load())
	}
}

type errorTransport struct{}

func (errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("private transport failure")
}

func TestTransportErrorAndNoCounter(t *testing.T) {
	client := &http.Client{Transport: WrapTransport(errorTransport{}, "api")}
	ctx, c := requestwork.WithCounter(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected transport error")
	}
	s := c.Close(time.Now().Add(-time.Second), time.Now())
	if s.ExternalCount != 1 || s.ExternalErrors != 1 || strings.Contains(s.Calls[0].Label, "example") {
		t.Fatalf("error attribution: %+v", s)
	}
	req, err = http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected passthrough error")
	}
}

func TestTLSAndRedirectAttempts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/done", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	ctx, c := requestwork.WithCounter(context.Background())
	client := srv.Client()
	client.Transport = WrapTransport(client.Transport, "tls-service")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	s := c.Close(start, time.Now())
	if s.ExternalCount != 2 || s.Calls[0].TLS <= 0 || s.Calls[0].Count != 2 || s.ExternalErrors != 0 {
		t.Fatalf("redirect/TLS: %+v", s)
	}
}

type readErrorBody struct{}

func (readErrorBody) Read([]byte) (int, error) { return 0, errors.New("private read error") }
func (readErrorBody) Close() error             { return nil }

type readErrorTransport struct{}

func (readErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: readErrorBody{}, Header: make(http.Header)}, nil
}

func TestReadErrorFinalizesOnce(t *testing.T) {
	ctx, c := requestwork.WithCounter(context.Background())
	client := &http.Client{Transport: WrapTransport(readErrorTransport{}, "reader")}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("expected read error")
	}
	_ = resp.Body.Close()
	s := c.Close(start, time.Now())
	if s.ExternalCount != 1 || s.ExternalErrors != 1 {
		t.Fatalf("read error: %+v", s)
	}
}

type tracedTransport struct{}

func (tracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	trace := httptrace.ContextClientTrace(req.Context())
	trace.GetConn("static-host")
	time.Sleep(time.Millisecond)
	trace.GotConn(httptrace.GotConnInfo{})
	trace.ConnectStart("tcp", "one")
	trace.ConnectStart("tcp", "two")
	time.Sleep(5 * time.Millisecond)
	trace.ConnectDone("tcp", "one", nil)
	time.Sleep(5 * time.Millisecond)
	trace.ConnectDone("tcp", "two", nil)
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
}

func TestConcurrentConnectUnionAcquireAndNoBody(t *testing.T) {
	ctx, c := requestwork.WithCounter(context.Background())
	client := &http.Client{Transport: WrapTransport(tracedTransport{}, "connector")}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Body != http.NoBody {
		t.Fatal("NoBody was wrapped")
	}
	s := c.Close(start, time.Now())
	if s.ExternalCount != 1 || len(s.Calls) != 1 || s.Calls[0].Acquire <= 0 || s.Calls[0].Connect <= 0 || s.Calls[0].Connect > s.Calls[0].Headers {
		t.Fatalf("phase union or NoBody: %+v", s)
	}
}

type idleTransport struct{ closed atomic.Int64 }

func (*idleTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused")
}
func (t *idleTransport) CloseIdleConnections() { t.closed.Add(1) }
func TestCloseIdleConnectionsForwarded(t *testing.T) {
	base := &idleTransport{}
	client := &http.Client{Transport: WrapTransport(base, "api")}
	client.CloseIdleConnections()
	if base.closed.Load() != 1 {
		t.Fatal("idle close not forwarded")
	}
}

func TestCanceledResponseBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	tracked, c := requestwork.WithCounter(context.Background())
	ctx, cancel := context.WithCancel(tracked)
	client := &http.Client{Transport: WrapTransport(http.DefaultTransport, "cancelable")}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("expected body cancellation")
	}
	_ = resp.Body.Close()
	s := c.Close(start, time.Now())
	if s.ExternalCount != 1 || s.ExternalErrors != 1 {
		t.Fatalf("cancellation: %+v", s)
	}
}

// An EOF is terminal even when the caller subsequently sees a Close error.
func TestErrorAfterEOFDoesNotChangeCompletedCall(t *testing.T) {
	ctx, c := requestwork.WithCounter(context.Background())
	start := time.Now()
	b := &body{ReadCloser: closeErrorBody{}, recorder: &recorder{}, ctx: ctx, label: "api", start: start, headersEnd: start}
	if _, err := b.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
	if err := b.Close(); err == nil {
		t.Fatal("underlying Close error not preserved")
	}
	s := c.Close(start, time.Now())
	if s.ExternalCount != 1 || s.ExternalErrors != 0 {
		t.Fatalf("completion changed: %+v", s)
	}
}

type closeErrorBody struct{}

func (closeErrorBody) Read([]byte) (int, error) { return 0, io.EOF }
func (closeErrorBody) Close() error             { return errors.New("close failed") }
