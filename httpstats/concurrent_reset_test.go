package httpstats

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConcurrentResetsAreSerialized(t *testing.T) {
	c := New()
	started := make(chan struct{})
	release := make(chan struct{})
	h := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			close(started)
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/old", nil))
	<-started
	c.mu.Lock()
	firstGeneration := c.current
	c.mu.Unlock()

	first := make(chan Snapshot, 1)
	go func() { first <- c.Reset() }()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		swapped := c.current != firstGeneration
		c.mu.Unlock()
		if swapped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first reset did not publish a new generation")
		}
		time.Sleep(time.Millisecond)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/new", nil))
	second := make(chan Snapshot, 1)
	go func() { second <- c.Reset() }()
	select {
	case got := <-second:
		t.Fatalf("second reset overtook the blocked first reset: %#v", got)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	old := <-first
	newer := <-second
	if len(old) != 1 || old[0].Path != "/old" {
		t.Fatalf("first reset = %#v", old)
	}
	if len(newer) != 1 || newer[0].Path != "/new" {
		t.Fatalf("second reset = %#v", newer)
	}
}
