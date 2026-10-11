package sqlstats

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/clientstats"
	"github.com/ekusiadadus/isutools/httpstats"
	"github.com/ekusiadadus/isutools/internal/requestwork"
	proxy "github.com/shogo82148/go-sql-proxy"
)

func TestRequestWorkIntegratesSQLAndOutboundHTTP(t *testing.T) {
	Default.Reset()
	t.Cleanup(Default.Reset)
	if err := Register("isutoolsfake"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("isutoolsfake"+DriverSuffix, "work-integration")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const secret = "outbound-secret-7f3a"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		time.Sleep(3 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	client := &http.Client{Transport: clientstats.WrapTransport(http.DefaultTransport, "payment-gateway")}
	defer client.CloseIdleConnections()

	collector := httpstats.New(httpstats.WithRequestWork(true))
	handler := collector.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := db.ExecContext(r.Context(), "UPDATE accounts SET token = 'sql-secret' WHERE id = 1"); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for i := 0; i < 3; i++ {
			ctx := r.Context()
			if i == 2 {
				ctx = context.Background()
			} // detached work must not enter the request counter
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/"+secret+"?token="+secret, strings.NewReader(secret))
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			req.Header.Set("X-Secret", secret)
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, readErr := io.Copy(io.Discard, resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Errorf("upstream body: %v / %v", readErr, closeErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		_, _ = io.WriteString(w, "done")
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/checkout/123", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("handler returned %d", recorder.Code)
	}

	entries := collector.Snapshot()
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	e := entries[0]
	if e.Work == nil {
		t.Fatal("missing request work")
	}
	work := e.Work
	if e.SQLCount != 1 || work.TrackedRequests != 1 || work.CompleteRequests != 1 || work.TruncatedRequests != 0 || work.SQLTotal <= 0 || work.ExternalCount != 2 || work.ExternalErrors != 0 || work.ExternalOnly <= 0 || work.SQLOnly <= 0 || len(work.Calls) != 1 || work.Calls[0].Label != "payment-gateway" || work.Calls[0].Count != 2 {
		t.Fatalf("unexpected request work: entry=%+v work=%+v", e, work)
	}
	if got := work.SQLOnly + work.ExternalOnly + work.Overlap + work.Other; got != e.Total || work.CompleteTotal != e.Total {
		t.Fatalf("wall partition=%s complete=%s entry=%s", got, work.CompleteTotal, e.Total)
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{secret, "sql-secret", "/checkout/123"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private value %q in JSON: %s", private, encoded)
		}
	}
	var decoded httpstats.Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Work == nil || len(decoded[0].Work.Calls) != 1 || decoded[0].Work.Calls[0].Label != "payment-gateway" || decoded[0].Work.ExternalCount != 2 {
		t.Fatalf("JSON round trip: %+v", decoded)
	}
}

func TestRequestWorkErrSkipDoesNotRecordSQLTime(t *testing.T) {
	Default.Reset()
	t.Cleanup(Default.Reset)
	ctx, c := requestwork.WithCounter(context.Background())
	h := hooks()
	stmt := &proxy.Stmt{QueryString: "SELECT 1"}
	pre, err := h.PreExec(ctx, stmt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.PostExec(ctx, pre, stmt, nil, nil, driver.ErrSkip); err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Second)
	if s := c.Close(start, time.Now()); s.SQLTotal != 0 || s.SQLOnly != 0 {
		t.Fatalf("ErrSkip counted as SQL work: %+v", s)
	}
}
