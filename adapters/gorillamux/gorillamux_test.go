package gorillamux

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/gorilla/mux"
)

func TestMiddlewareUsesRegisteredTemplate(t *testing.T) {
	router := mux.NewRouter()
	router.Use(MiddlewareEnabled(true))
	router.HandleFunc("/posts/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	httpstats.Default.Reset()
	httpstats.Middleware(router).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/posts/secret", nil))
	snap := httpstats.Default.Snapshot()
	if len(snap) != 1 || snap[0].Path != "/posts/{id}" {
		t.Fatalf("snapshot = %#v", snap)
	}
}
