package httprouter

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
	router "github.com/julienschmidt/httprouter"
)

func TestHandleUsesRegistrationPattern(t *testing.T) {
	r := router.New()
	r.GET("/orders/:id", Handle("/orders/:id", func(w http.ResponseWriter, _ *http.Request, _ router.Params) { w.WriteHeader(204) }))
	httpstats.Default.Reset()
	httpstats.Middleware(r).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/orders/secret", nil))
	snap := httpstats.Default.Snapshot()
	if len(snap) != 1 || snap[0].Path != "/orders/:id" {
		t.Fatalf("snapshot = %#v", snap)
	}
}
