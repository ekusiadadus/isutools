package httpstats

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/requestsql"
)

func TestMiddlewareAggregatesCompletedSQLPerRequest(t *testing.T) {
	c := New()
	late := make(chan struct{})
	lateDone := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/late":
			ctx := r.Context()
			go func() {
				defer close(lateDone)
				<-late
				requestsql.Completed(ctx)
			}()
		case "/read":
			for i := 0; i < int(r.Header.Get("X-SQL-Count")[0]-'0'); i++ {
				requestsql.Completed(r.Context())
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	serve := func(path, count string) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if count != "" {
			r.Header.Set("X-SQL-Count", count)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	serve("/read", "0")
	serve("/read", "1")
	serve("/read", "3")
	serve("/late", "")
	close(late)
	<-lateDone

	entries := c.Snapshot()
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	for _, e := range entries {
		switch e.Path {
		case "/read":
			if e.Count != 3 || e.SQLTrackedRequests != 3 || e.SQLCount != 4 || e.SQLMaxPerRequest != 3 {
				t.Errorf("read entry = %#v", e)
			}
		case "/late":
			if e.SQLTrackedRequests != 1 || e.SQLCount != 0 {
				t.Errorf("late entry = %#v", e)
			}
		default:
			t.Errorf("unexpected entry = %#v", e)
		}
	}
	// A reset freezes the same request counts and clears the live generation.
	frozen := c.Reset()
	if len(frozen) != 2 || len(c.Snapshot()) != 0 {
		t.Fatalf("Reset = %#v, live = %#v", frozen, c.Snapshot())
	}
}

func TestConcurrentRequestsKeepSQLCountsSeparate(t *testing.T) {
	c := New()
	started := make(chan struct{})
	release := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsql.Completed(r.Context())
		if r.URL.Path == "/slow" {
			close(started)
			<-release
			requestsql.Completed(r.Context())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	}()
	<-started
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/fast", nil))
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow request did not finish")
	}
	got := map[string]int64{}
	for _, e := range c.Snapshot() {
		got[e.Path] = e.SQLCount
	}
	if got["/fast"] != 1 || got["/slow"] != 2 {
		t.Fatalf("per-request counts = %#v", got)
	}
}

func TestDirectObservationHasNoRequestSQLCoverage(t *testing.T) {
	table := newTable(1)
	table.observe(identity{method: "GET", path: "/direct"}, time.Millisecond, 0)
	e := table.snapshot()[0]
	if e.Count != 1 || e.SQLTrackedRequests != 0 || e.SQLCount != 0 {
		t.Fatalf("direct observation = %#v", e)
	}
}

func TestRequestSQLCountStaysWithGenerationAcrossReset(t *testing.T) {
	c := New()
	started := make(chan struct{})
	release := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsql.Completed(r.Context())
		if r.URL.Path == "/old" {
			close(started)
			<-release
			requestsql.Completed(r.Context())
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
	if len(old) != 1 || old[0].Path != "/old" || old[0].SQLCount != 2 || old[0].SQLTrackedRequests != 1 {
		t.Fatalf("old generation = %#v", old)
	}
	current := c.Snapshot()
	if len(current) != 1 || current[0].Path != "/new" || current[0].SQLCount != 1 || current[0].SQLTrackedRequests != 1 {
		t.Fatalf("new generation = %#v", current)
	}
}
