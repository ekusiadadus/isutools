package sessionlabel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/httpstats"
)

type detailedObservedFlow struct {
	legacy int
	value  Observation
}

func (o *detailedObservedFlow) Observe(string, string, string, string) { o.legacy++ }
func (o *detailedObservedFlow) ObserveRequest(value Observation)       { o.value = value }

func TestDetailedObserverReceivesStatusLatencyAndRequestStart(t *testing.T) {
	observer := &detailedObservedFlow{}
	adapter := New("session", []byte("01234567890123456789012345678901")).WithObserver(observer)
	handler := adapter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetScenario(r, "checkout")
		httpstats.SetRoutePattern(r, "/checkout")
		time.Sleep(time.Millisecond)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	req := httptest.NewRequest(http.MethodPost, "/checkout?secret=x", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw-secret"})
	started := time.Now()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if observer.legacy != 0 {
		t.Fatalf("legacy observer called %d times", observer.legacy)
	}
	got := observer.value
	if got.Session == "" || got.Session == "raw-secret" || got.Scenario != "checkout" || got.Method != http.MethodPost ||
		got.Route != "/checkout" || got.Status != http.StatusServiceUnavailable || got.Duration < time.Millisecond || got.At.Before(started) {
		t.Fatalf("observation = %#v", got)
	}
}

func TestDetailedObserverDefaultsEmptySuccessfulHandlerTo200(t *testing.T) {
	observer := &detailedObservedFlow{}
	adapter := New("session", []byte("01234567890123456789012345678901")).WithObserver(observer)
	handler := adapter.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "raw-secret"})
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if observer.value.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", observer.value.Status)
	}
}
