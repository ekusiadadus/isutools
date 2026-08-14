package sessionlabel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
)

type observedFlow struct{ session, scenario, method, route string }

func (o *observedFlow) Observe(session, scenario, method, route string) {
	o.session, o.scenario, o.method, o.route = session, scenario, method, route
}

type panicObserver struct{}

func (panicObserver) Observe(string, string, string, string) { panic("observer failure") }

func TestObserverReceivesOnlyPseudonymAndRouteTemplate(t *testing.T) {
	observer := &observedFlow{}
	adapter := New("SESSION", []byte(strings.Repeat("k", MinKeyBytes))).WithObserver(observer)
	handler := httpstats.Middleware(adapter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpstats.SetRoutePattern(r, "/users/{id}")
		SetScenario(r, "profile")
		w.WriteHeader(204)
	})))
	req := httptest.NewRequest("GET", "/users/raw-secret", nil)
	req.AddCookie(&http.Cookie{Name: "SESSION", Value: "raw-cookie-secret"})
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if observer.session == "" || observer.session == "raw-cookie-secret" {
		t.Fatalf("session = %q", observer.session)
	}
	if observer.scenario != "profile" || observer.route != "/users/{id}" || observer.method != "GET" {
		t.Fatalf("observation = %#v", observer)
	}
}

func TestObserverPanicCannotBreakApplication(t *testing.T) {
	adapter := New("SESSION", []byte(strings.Repeat("k", MinKeyBytes))).WithObserver(panicObserver{})
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "SESSION", Value: "raw-cookie"})
	adapter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(httptest.NewRecorder(), req)
}
