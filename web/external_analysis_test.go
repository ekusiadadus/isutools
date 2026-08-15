package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/analysisartifact"
	"github.com/ekusiadadus/isutools/internal/safefs"
)

func TestExternalAnalysisIndexUsesVerifiedPortableVisibility(t *testing.T) {
	directory := t.TempDir()
	snapshot := []byte(`{"meta":{"schema_version":3,"run":{"run_id":"run-external"}}}`)
	if err := os.WriteFile(directory+"/snapshot-external.json", snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := safefs.Open(directory, safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	store := analysisartifact.NewStore(root)
	portable, err := store.PublishContent("run-external", analysisartifact.KindMySQLSlowLog, analysisartifact.Content{
		Role: "normalized-summary", Extension: "json", MediaType: "application/json", Visibility: analysisartifact.VisibilityPortable,
		Body: []byte("{\"classes\":[]}"), MaxBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := store.PublishContent("run-external", analysisartifact.KindMySQLSlowLog, analysisartifact.Content{
		Role: "pt-query-digest-report", Extension: "txt", MediaType: "text/plain", Visibility: analysisartifact.VisibilityRestricted,
		Body: []byte("restricted SQL report"), MaxBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := analysisartifact.SetArtifactID(analysisartifact.Manifest{
		Schema: analysisartifact.SchemaV1, Kind: analysisartifact.KindMySQLSlowLog, GeneratedAt: time.Now(), Status: analysisartifact.StatusReady,
		Analyzer: analysisartifact.Analyzer{Name: "test-analyzer", Version: "v1"},
		Run:      &analysisartifact.RunBinding{RunID: "run-external", SnapshotBase: "snapshot-external", SnapshotSHA256: externalHash(snapshot), SnapshotSchemaVersion: 3},
		Inputs:   []analysisartifact.FileRef{{Role: "mysql-slowlog", Name: "mysql-slow.log", SHA256: strings.Repeat("a", 64), Bytes: 1, MediaType: "text/plain", Visibility: analysisartifact.VisibilityRestricted}},
		Outputs:  []analysisartifact.FileRef{portable, restricted}, Coverage: analysisartifact.Coverage{Complete: true}, Budget: analysisartifact.ResourceBudget{MaxInputBytes: 1024, MaxOutputBytes: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish("run-external", manifest, analysisartifact.NoCurrentArtifact); err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		t.Fatal(err)
	}
	_ = root.Close()

	handler := NewHandler(Provider{DataDir: directory})
	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/external-analysis", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), portable.Name) || strings.Contains(index.Body.String(), restricted.Name) {
		t.Fatalf("index status=%d body=%s", index.Code, index.Body.String())
	}
	portableRequest := httptest.NewRecorder()
	handler.ServeHTTP(portableRequest, httptest.NewRequest(http.MethodGet, "/files/"+portable.Name, nil))
	if portableRequest.Code != http.StatusOK || portableRequest.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("portable status=%d headers=%v", portableRequest.Code, portableRequest.Header())
	}
	restrictedRequest := httptest.NewRecorder()
	handler.ServeHTTP(restrictedRequest, httptest.NewRequest(http.MethodGet, "/files/"+restricted.Name, nil))
	if restrictedRequest.Code != http.StatusForbidden || strings.Contains(restrictedRequest.Body.String(), "restricted SQL report") {
		t.Fatalf("restricted status=%d body=%q", restrictedRequest.Code, restrictedRequest.Body.String())
	}
}

func externalHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
