package sqlstats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/requestsql"
	proxy "github.com/shogo82148/go-sql-proxy"
)

func TestContextQueriesIncrementRequestCounter(t *testing.T) {
	t.Cleanup(Default.Reset)
	if err := Register("isutoolsfake"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("isutoolsfake"+DriverSuffix, "request-sql")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, counter := requestsql.WithCounter(context.Background())
	if _, err := db.ExecContext(ctx, "UPDATE t SET c = 1"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if got := counter.Close(); got != 2 {
		t.Fatalf("request SQL count = %d, want 2", got)
	}
	if _, err := db.ExecContext(ctx, "UPDATE t SET c = 2"); err != nil {
		t.Fatal(err)
	}
	if got := counter.Close(); got != 2 {
		t.Fatalf("late query changed request count to %d", got)
	}
}

func TestContextSQLShapesMatchGlobalCountsAndFinalRoutes(t *testing.T) {
	Default.Reset()
	t.Cleanup(Default.Reset)
	if err := Register("isutoolsfake"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("isutoolsfake"+DriverSuffix, "shape-attribution")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	httpCollector := httpstats.New(httpstats.WithSQLShapeAttribution(true))
	mux := http.NewServeMux()
	const query = "UPDATE seats SET owner = 'alice-secret' WHERE id = 123"
	mux.HandleFunc("GET /search/{id}", func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 2; i++ {
			if _, err := db.ExecContext(r.Context(), query); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
	})
	mux.HandleFunc("POST /reserve/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.ExecContext(r.Context(), query); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	h := httpCollector.Middleware(mux)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/search/1?secret=token"}, {"GET", "/search/2"}, {"POST", "/reserve/3"},
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
	}
	global := Default.Snapshot()
	if len(global) != 1 || global[0].Count != 5 || strings.Contains(global[0].Key, "alice-secret") {
		t.Fatalf("global SQL = %+v", global)
	}
	observed := httpCollector.Snapshot()
	encoded, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "alice-secret") || strings.Contains(string(encoded), "token") {
		t.Fatalf("request or SQL literal leaked into JSON: %s", encoded)
	}
	var attributed int64
	for _, entry := range observed {
		if strings.Contains(entry.Path, "secret") || len(entry.SQLShapes) != 1 || entry.SQLShapes[0].Key != global[0].Key {
			t.Fatalf("route SQL shape = %+v", entry)
		}
		attributed += entry.SQLShapes[0].Count
		switch entry.Path {
		case "/search/{id}":
			if entry.SQLShapes[0].Count != 4 || entry.SQLShapes[0].MaxPerRequest != 2 {
				t.Fatalf("search = %+v", entry)
			}
		case "/reserve/{id}":
			if entry.SQLShapes[0].Count != 1 {
				t.Fatalf("reserve = %+v", entry)
			}
		default:
			t.Fatalf("unexpected route = %+v", entry)
		}
	}
	if attributed != global[0].Count {
		t.Fatalf("attributed=%d global=%d", attributed, global[0].Count)
	}
}

func TestHookCountsFailuresButNotErrSkip(t *testing.T) {
	t.Cleanup(Default.Reset)
	ctx, counter := requestsql.WithCounter(context.Background())
	h := hooks()
	stmt := &proxy.Stmt{QueryString: "SELECT 1"}
	pre, err := h.PreExec(ctx, stmt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PostExec(ctx, pre, stmt, nil, nil, driver.ErrSkip); err != nil {
		t.Fatal(err)
	}
	pre, err = h.PreExec(ctx, stmt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PostExec(ctx, pre, stmt, nil, nil, errors.New("database failed")); err != nil {
		t.Fatal(err)
	}
	pre, err = h.PreQuery(ctx, stmt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PostQuery(ctx, pre, stmt, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := counter.Close(); got != 2 {
		t.Fatalf("request SQL count = %d, want 2", got)
	}
	if entries := Default.Snapshot(); len(entries) != 1 || entries[0].Count != 2 || entries[0].ErrorCount != 1 {
		t.Fatalf("SQL aggregate = %#v", entries)
	}
}
