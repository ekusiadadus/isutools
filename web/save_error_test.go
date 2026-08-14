package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ekusiadadus/isutools/internal/agg"
)

func decodeAdminError(t *testing.T, recorder *httptest.ResponseRecorder) AdminErrorResponse {
	t.Helper()
	var response AdminErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode admin error %q: %v", recorder.Body.String(), err)
	}
	return response
}

func TestSaveErrorsExposeStableReasonCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		provider Provider
		method   string
		path     string
		status   int
		reason   string
	}{
		{name: "method", provider: Provider{DataDir: t.TempDir()}, method: http.MethodGet, path: "/save?secret=do-not-echo", status: http.StatusMethodNotAllowed, reason: SaveReasonMethodNotAllowed},
		{name: "data dir", provider: Provider{}, method: http.MethodPost, path: "/save", status: http.StatusBadRequest, reason: SaveReasonDataDirUnset},
		{name: "invalid pass", provider: Provider{DataDir: t.TempDir()}, method: http.MethodPost, path: "/save?pass=secret-value", status: http.StatusBadRequest, reason: SaveReasonInvalidPass},
		{name: "run inactive", provider: Provider{DataDir: t.TempDir(), FinishRun: func(context.Context) (RunFinish, error) { return RunFinish{}, errors.New("secret database token") }}, method: http.MethodPost, path: "/save", status: http.StatusConflict, reason: SaveReasonRunNotActive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var audits []AdminAudit
			test.provider.AdminAudit = func(event AdminAudit) { audits = append(audits, event) }
			handler := NewHandler(test.provider)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			response := decodeAdminError(t, recorder)
			if response.Reason != test.reason || recorder.Header().Get(AdminReasonHeader) != test.reason {
				t.Fatalf("reason body=%q header=%q, want %q", response.Reason, recorder.Header().Get(AdminReasonHeader), test.reason)
			}
			if len(audits) != 1 || audits[0].Reason != test.reason || audits[0].Status != test.status {
				t.Fatalf("audit = %#v", audits)
			}
			joined, _ := json.Marshal(struct {
				Response AdminErrorResponse
				Audits   []AdminAudit
			}{response, audits})
			for _, forbidden := range []string{"secret", "do-not-echo", "secret-value", "database token"} {
				if strings.Contains(string(joined), forbidden) {
					t.Fatalf("error/audit leaked %q: %s", forbidden, joined)
				}
			}
		})
	}
}

func TestSavePersistenceFailuresHaveCodesAndPublishNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		provider func(t *testing.T) Provider
		status   int
		reason   string
	}{
		{
			name: "snapshot too large",
			provider: func(t *testing.T) Provider {
				table := agg.NewTable(agg.DefaultMaxKeys)
				table.Observe(strings.Repeat("x", 33<<20), 1)
				return Provider{SQL: table, DataDir: t.TempDir()}
			},
			status: http.StatusRequestEntityTooLarge, reason: SaveReasonSnapshotTooLarge,
		},
		{
			name: "persist failed",
			provider: func(t *testing.T) Provider {
				root := t.TempDir()
				file := filepath.Join(root, "not-a-directory")
				if err := os.WriteFile(file, []byte("secret-path-marker"), 0o600); err != nil {
					t.Fatal(err)
				}
				return Provider{DataDir: file}
			},
			status: http.StatusInternalServerError, reason: SaveReasonPersistFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := test.provider(t)
			var audit AdminAudit
			provider.AdminAudit = func(event AdminAudit) { audit = event }
			handler := NewHandler(provider)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/save", nil))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			response := decodeAdminError(t, recorder)
			if response.Reason != test.reason || audit.Reason != test.reason {
				t.Fatalf("response=%#v audit=%#v", response, audit)
			}
			if strings.Contains(recorder.Body.String(), provider.DataDir) || strings.Contains(recorder.Body.String(), "secret-path-marker") {
				t.Fatalf("response leaked persistence detail: %s", recorder.Body.String())
			}
		})
	}
}

func TestSaveBusyUsesMachineReadableReason(t *testing.T) {
	t.Parallel()
	h := newHandler(Provider{DataDir: t.TempDir()})
	h.operation <- struct{}{}
	defer func() { <-h.operation }()
	recorder := httptest.NewRecorder()
	h.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/save", nil))
	if recorder.Code != http.StatusConflict || decodeAdminError(t, recorder).Reason != SaveReasonMutationBusy {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCoordinatedRunCannotPublishTwoBenchmarkOutcomes(t *testing.T) {
	dir := t.TempDir()
	starter := &fakeRunStarter{id: "run-save-once"}
	terminator := &fakeRunTerminator{id: starter.id}
	h := NewHandler(Provider{
		SQL:         agg.NewTable(agg.DefaultMaxKeys),
		DataDir:     dir,
		StartRun:    starter.start,
		FinishRun:   terminator.finish,
		CompleteRun: terminator.complete,
		Sections:    terminator.sections,
	})

	reset := httptest.NewRecorder()
	h.ServeHTTP(reset, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if reset.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d: %s", reset.Code, reset.Body.String())
	}
	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/save?score=12941&pass=true", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first save status = %d: %s", first.Code, first.Body.String())
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, httptest.NewRequest(http.MethodPost, "/save?score=1710&pass=true", nil))
	if replay.Code != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409: %s", replay.Code, replay.Body.String())
	}
	response := decodeAdminError(t, replay)
	if response.Reason != SaveReasonRunAlreadySaved || replay.Header().Get(AdminReasonHeader) != SaveReasonRunAlreadySaved {
		t.Fatalf("replay reason body=%q header=%q", response.Reason, replay.Header().Get(AdminReasonHeader))
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("replay published files: before=%d after=%d", len(before), len(after))
	}
	if terminator.finishes != 1 || terminator.completes != 1 {
		t.Fatalf("replay touched coordinator: finish=%d complete=%d", terminator.finishes, terminator.completes)
	}

	nextReset := httptest.NewRecorder()
	h.ServeHTTP(nextReset, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if nextReset.Code != http.StatusNoContent {
		t.Fatalf("next reset status = %d: %s", nextReset.Code, nextReset.Body.String())
	}
	nextSave := httptest.NewRecorder()
	h.ServeHTTP(nextSave, httptest.NewRequest(http.MethodPost, "/save?score=13000&pass=true", nil))
	if nextSave.Code != http.StatusOK {
		t.Fatalf("next run save status = %d: %s", nextSave.Code, nextSave.Body.String())
	}
}
