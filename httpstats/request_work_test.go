package httpstats

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/requestwork"
)

func TestRequestWorkOptInAndFreeze(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		c := New(WithRequestWork(enabled))
		var ctx context.Context
		h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx = r.Context()
			now := time.Now()
			requestwork.CompletedExternal(ctx, requestwork.External{Label: "shipping", Start: now, End: now.Add(time.Nanosecond)})
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/orders/123", nil))
		e := c.Snapshot()[0]
		if !enabled {
			if e.Work != nil {
				t.Fatal("disabled timing recorded")
			}
			continue
		}
		if e.Work.TrackedRequests != 1 || e.Work.CompleteRequests != 1 || e.Work.ExternalCount != 1 {
			t.Fatalf("work=%+v", e.Work)
		}
		if got := e.Work.SQLOnly + e.Work.ExternalOnly + e.Work.Overlap + e.Work.Other; got != e.Total {
			t.Fatalf("partition=%v total=%v", got, e.Total)
		}
		now := time.Now()
		requestwork.CompletedExternal(ctx, requestwork.External{Label: "late", Start: now, End: now.Add(time.Second)})
		e.Work.Calls[0].Label = "mutation"
		if got := c.Snapshot()[0].Work; got.ExternalCount != 1 || got.Calls[0].Label != "shipping" {
			t.Fatalf("snapshot mutated: %+v", got)
		}
	}
}

func TestRequestWorkTruncationAndRunLabelBound(t *testing.T) {
	table := newTable(1)
	id := identity{method: "GET", path: "/a"}
	for i := 0; i < maxWorkKeys+4; i++ {
		s := requestwork.Summary{ExternalCount: 1, ExternalTotal: time.Millisecond, Other: time.Millisecond, Calls: []requestwork.Call{{Label: fmt.Sprint(i), Count: 1, Total: time.Millisecond}}}
		table.observeRequestWork(id, time.Millisecond, 0, 0, true, nil, &s)
	}
	s := requestwork.Summary{Truncated: true, ExternalCount: 300, ExternalTotal: time.Second}
	table.observeRequestWork(id, time.Millisecond, 0, 0, true, nil, &s)
	e := table.snapshot()[0]
	if len(e.Work.Calls) != maxWorkKeys+1 || e.Work.TruncatedRequests != 1 || e.Work.CompleteRequests != maxWorkKeys+4 {
		t.Fatalf("bad bounded work: calls=%d work=%+v", len(e.Work.Calls), e.Work)
	}
	if e.Work.CompleteTotal != e.Work.Other {
		t.Fatal("truncated sample entered wall breakdown")
	}
	var overflow int64
	for _, c := range e.Work.Calls {
		if c.Label == OverflowDependency {
			overflow = c.Count
		}
	}
	if overflow != 4 {
		t.Fatalf("overflow=%d", overflow)
	}
	// New HTTP identities also retain measurements in the HTTP overflow row.
	table.observeRequestWork(identity{path: "/overflow"}, time.Millisecond, 0, 0, true, nil, &s)
	if len(table.snapshot()) != 2 {
		t.Fatal("HTTP overflow missing")
	}
}

func TestRequestWorkPanicAndStreaming(t *testing.T) {
	c := New(WithRequestWork(true))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/panic", nil))
	}()
	if e := c.Snapshot()[0]; e.Status != 500 || e.Work.TrackedRequests != 1 {
		t.Fatalf("panic=%+v", e)
	}
	c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream", nil))
	if len(c.Snapshot()) != 1 {
		t.Fatal("stream entered latency table")
	}
}

func TestRequestWorkStaysWithGenerationAcrossReset(t *testing.T) {
	c := New(WithRequestWork(true))
	started := make(chan struct{})
	release := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestwork.CompletedSQL(r.Context(), time.Now().Add(-time.Microsecond), time.Now())
		if r.URL.Path == "/old" {
			close(started)
			<-release
			requestwork.CompletedSQL(r.Context(), time.Now().Add(-time.Microsecond), time.Now())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/old", nil))
	}()
	<-started
	oldGeneration := currentGeneration(c)
	first := make(chan Snapshot, 1)
	go func() { first <- c.Reset() }()
	deadline := time.Now().Add(time.Second)
	for {
		if currentGeneration(c) != oldGeneration {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Reset did not publish a new generation")
		}
		time.Sleep(time.Millisecond)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/new", nil))
	close(release)
	<-requestDone
	old := <-first
	if len(old) != 1 || old[0].Path != "/old" || old[0].Work.TrackedRequests != 1 || old[0].Work.SQLTotal < 2*time.Microsecond {
		t.Fatalf("old generation = %#v", old)
	}
	current := c.Snapshot()
	if len(current) != 1 || current[0].Path != "/new" || current[0].Work.TrackedRequests != 1 || current[0].Work.SQLTotal < time.Microsecond {
		t.Fatalf("new generation = %#v", current)
	}
}
