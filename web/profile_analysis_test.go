package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

var (
	analysisSidecarBody  = []byte("{\"phase\":\"initial\"}\n")
	analysisCoverageBody = []byte("{\"phase\":\"coverage\"}\n")
)

const (
	analysisSidecarName  = "cpu_fixture.meta.json"
	analysisCoverageName = "cpu_fixture.coverage.json"
)

func TestProfileAnalysisRouteIsDefaultOff(t *testing.T) {
	handler := NewHandler(Provider{DataDir: t.TempDir()})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/profile-analysis?snapshot=x", strings.NewReader(`{}`)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestProfileAnalysisCapabilitiesAreReadOnlyAndFailClosedInputsAreExplicit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := newHandler(Provider{
		DataDir: dir, ProfileAnalysis: true, RuntimeProfiles: []string{"mutex", "block"}, CPUProfileMode: "run",
	})
	request := httptest.NewRequest(http.MethodGet, "/profile-analysis-capabilities", nil)
	response := httptest.NewRecorder()
	h.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("capabilities status=%d body=%s", response.Code, response.Body.String())
	}
	var capabilities ProfileAnalysisCapabilities
	if err := json.Unmarshal(response.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.Schema != profileAnalysisCapabilitiesSchema || !capabilities.StrongAtomicVisibility ||
		!capabilities.DataDirAvailableKnown || capabilities.DataDirAvailableBytes == 0 ||
		capabilities.RetentionRuns != profileRetentionRuns || capabilities.RetentionBytes != profileRetentionBytes ||
		capabilities.ExpectedProfileFilesPerRun != 5 || capabilities.CurrentGeneration != 1 ||
		capabilities.PerRunCeilingBytes == 0 || capabilities.CPUCoverageMaxBytes != profileSidecarMaxBytes {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if strings.Contains(response.Body.String(), dir) {
		t.Fatal("capabilities leaked DataDir path")
	}

	response = httptest.NewRecorder()
	h.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/profile-analysis-capabilities", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST capabilities status=%d", response.Code)
	}
}

func TestProfileAnalysisPublishCASAndImmutableOriginal(t *testing.T) {
	dir := t.TempDir()
	h := newHandler(Provider{DataDir: dir, ProfileAnalysis: true})
	base, snapshotHash, profileHash := writeAnalysisFixture(t, h, dir)
	original, err := os.ReadFile(filepath.Join(dir, base+".html"))
	if err != nil {
		t.Fatal(err)
	}

	first := validWebAnalysis(t, base, snapshotHash, profileHash, 10)
	firstResponse := publishAnalysis(t, h.routes(), base, "none", first)
	if firstResponse.Code != http.StatusCreated && firstResponse.Code != http.StatusAccepted {
		t.Fatalf("first publish = %d: %s", firstResponse.Code, firstResponse.Body.String())
	}
	var firstResult ProfileAnalysisPublishResponse
	if err := json.Unmarshal(firstResponse.Body.Bytes(), &firstResult); err != nil {
		t.Fatal(err)
	}
	if firstResult.ArtifactID == "" || firstResult.CommitSequence != 1 || firstResult.CurrentArtifactID != firstResult.ArtifactID {
		t.Fatalf("first result = %#v", firstResult)
	}

	second := validWebAnalysis(t, base, snapshotHash, profileHash, 20)
	secondResponse := publishAnalysis(t, h.routes(), base, firstResult.ArtifactID, second)
	if secondResponse.Code != http.StatusCreated && secondResponse.Code != http.StatusAccepted {
		t.Fatalf("second publish = %d: %s", secondResponse.Code, secondResponse.Body.String())
	}
	var secondResult ProfileAnalysisPublishResponse
	if err := json.Unmarshal(secondResponse.Body.Bytes(), &secondResult); err != nil {
		t.Fatal(err)
	}
	if secondResult.CommitSequence != 2 || secondResult.ArtifactID == firstResult.ArtifactID {
		t.Fatalf("second result = %#v", secondResult)
	}

	stale := publishAnalysis(t, h.routes(), base, firstResult.ArtifactID, first)
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), secondResult.ArtifactID) {
		t.Fatalf("stale retry = %d: %s", stale.Code, stale.Body.String())
	}
	idempotent := publishAnalysis(t, h.routes(), base, "none", second)
	if idempotent.Code != http.StatusOK {
		t.Fatalf("idempotent retry = %d: %s", idempotent.Code, idempotent.Body.String())
	}

	after, err := os.ReadFile(filepath.Join(dir, base+".html"))
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("original snapshot changed: err=%v", err)
	}
	marker, err := os.ReadFile(filepath.Join(dir, base+".profile.current.json"))
	if err != nil || !bytes.Contains(marker, []byte(secondResult.ArtifactID)) {
		t.Fatalf("current marker = %q, %v", marker, err)
	}
	detail := httptest.NewRecorder()
	h.routes().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/20260806-120000.000000000-000001", nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "Profile Analysis") {
		t.Fatalf("derived detail = %d: %s", detail.Code, detail.Body.String())
	}
}

func TestProfileAnalysisRetentionKeepsLatestThreeActivations(t *testing.T) {
	dir := t.TempDir()
	h := newHandler(Provider{DataDir: dir, ProfileAnalysis: true})
	base, snapshotHash, profileHash := writeAnalysisFixture(t, h, dir)
	expected := "none"
	for value := int64(1); value <= 5; value++ {
		response := publishAnalysis(t, h.routes(), base, expected, validWebAnalysis(t, base, snapshotHash, profileHash, value))
		if response.Code != http.StatusCreated && response.Code != http.StatusAccepted {
			t.Fatalf("publish %d = %d: %s", value, response.Code, response.Body.String())
		}
		var published ProfileAnalysisPublishResponse
		if err := json.Unmarshal(response.Body.Bytes(), &published); err != nil {
			t.Fatal(err)
		}
		expected = published.ArtifactID
	}
	for pattern, want := range map[string]int{
		base + ".profile.commit.*.json":   3,
		base + ".profile.analysis.*.json": 3,
		base + ".profile.render.*.html":   3,
	} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil || len(matches) != want {
			t.Fatalf("%s matches = %v err=%v, want %d", pattern, matches, err, want)
		}
	}
}

func TestProfileAnalysisReaderRejectsTamperedActivationCommit(t *testing.T) {
	dir := t.TempDir()
	h := newHandler(Provider{DataDir: dir, ProfileAnalysis: true})
	base, snapshotHash, profileHash := writeAnalysisFixture(t, h, dir)
	response := publishAnalysis(t, h.routes(), base, "none", validWebAnalysis(t, base, snapshotHash, profileHash, 1))
	if response.Code != http.StatusCreated && response.Code != http.StatusAccepted {
		t.Fatalf("publish = %d: %s", response.Code, response.Body.String())
	}
	var published ProfileAnalysisPublishResponse
	if err := json.Unmarshal(response.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.profile.commit.%020d.json", base, published.CommitSequence)), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	detail := httptest.NewRecorder()
	h.routes().ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/20260806-120000.000000000-000001", nil))
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), "Profile Analysis") {
		t.Fatalf("tampered commit served derived detail = %d: %s", detail.Code, detail.Body.String())
	}
}

func TestProfileAnalysisPublishRejectsMutatedCPUCompletionEvidence(t *testing.T) {
	dir := t.TempDir()
	h := newHandler(Provider{DataDir: dir, ProfileAnalysis: true})
	base, snapshotHash, profileHash := writeAnalysisFixture(t, h, dir)
	if err := os.WriteFile(filepath.Join(dir, analysisSidecarName), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := publishAnalysis(t, h.routes(), base, "none", validWebAnalysis(t, base, snapshotHash, profileHash, 1))
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "sidecar") {
		t.Fatalf("mutated sidecar publish = %d: %s", response.Code, response.Body.String())
	}
}

func TestProfileAnalysisDerivedHTMLRendersCompleteEvidenceAndEscapesData(t *testing.T) {
	dir := t.TempDir()
	h := newHandler(Provider{DataDir: dir, ProfileAnalysis: true})
	base, snapshotHash, profileHash := writeAnalysisFixture(t, h, dir)
	analysis := validWebAnalysis(t, base, snapshotHash, profileHash, 10)
	attempt := &analysis.Attempts[0]
	analysis.Diagnostics = []profilemodel.Diagnostic{{Level: profilemodel.DiagnosticWarn, Code: profilemodel.DiagnosticProvenanceUnavailable, Message: "test provenance warning"}}
	attempt.Status = profilemodel.AnalysisStatusPartial
	attempt.Diagnostics = []profilemodel.Diagnostic{{Level: profilemodel.DiagnosticWarn, Code: profilemodel.DiagnosticSourcePathRedacted, Message: "outside root <redacted>"}}
	summary := &attempt.Summaries[0]
	summary.Reports[0].TopCumulative = []profilemodel.ProfileNode{{Function: `<script>alert("x")</script>`, Value: 5}}
	summary.Reports = append(summary.Reports, profilemodel.ProfileReport{
		Granularity: profilemodel.GranularityLines,
		TopFlat: []profilemodel.ProfileNode{{
			Function: "main.appGetNotification", File: "app_handlers.go", Line: 711, Value: 4,
		}},
		TopCumulative: []profilemodel.ProfileNode{{
			Function: "main.appGetNotification", File: "app_handlers.go", Line: 670, Value: 7,
		}},
	})
	summary.Labels = []profilemodel.LabelBreakdown{{Key: "http.route", Values: []profilemodel.LabelValue{{Value: "/users/{id}", Total: 5}}}}
	analysis.Status = profilemodel.AnalysisStatusPartial
	analysis.AnalysisID = ""
	var err error
	analysis, err = profilemodel.SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	response := publishAnalysis(t, h.routes(), base, "none", analysis)
	if response.Code != http.StatusCreated && response.Code != http.StatusAccepted {
		t.Fatalf("publish = %d: %s", response.Code, response.Body.String())
	}
	var published ProfileAnalysisPublishResponse
	if err := json.Unmarshal(response.Body.Bytes(), &published); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, published.HTMLFile))
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"次に見るソース行", "binary一致を検証済み", "app_handlers.go:711", "main.appGetNotification", "flat", "cumulative",
		"Analysis diagnostics", "Diagnostics", "coverage:", "Top cumulative", "Labels", "50.00%", "/users/{id}", "&lt;script&gt;", analysisSidecarName, analysisCoverageName,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("derived HTML missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, `<script>alert`) {
		t.Fatalf("derived HTML contains executable injected markup: %s", html)
	}
	preview := httptest.NewRecorder()
	h.routes().ServeHTTP(preview, httptest.NewRequest(http.MethodGet, "/20260806-120000.000000000-000001?view=current", nil))
	if preview.Code != http.StatusOK || preview.Header().Get("X-Isutools-View") != "current-renderer" ||
		preview.Header().Get("X-Isutools-Profile-Analysis") != "current" {
		t.Fatalf("current analysis preview = %d headers=%v: %s", preview.Code, preview.Header(), preview.Body.String())
	}
	for _, want := range []string{"結論: 次に修正する場所", "次に見るソース行", "app_handlers.go:711"} {
		if !strings.Contains(preview.Body.String(), want) {
			t.Errorf("current analysis preview missing %q: %s", want, preview.Body.String())
		}
	}
}

func writeAnalysisFixture(t *testing.T, h *handler, dir string) (string, string, string) {
	t.Helper()
	profileBytes := []byte("bounded profile fixture")
	profileSum := sha256.Sum256(profileBytes)
	profileHash := hex.EncodeToString(profileSum[:])
	if err := os.WriteFile(filepath.Join(dir, "cpu_fixture.pprof"), profileBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{analysisSidecarName: analysisSidecarBody, analysisCoverageName: analysisCoverageBody} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := "20260806-120000.000000000-000001_gen1_deadbee"
	snapshot := Snapshot{Meta: Meta{
		SchemaVersion: schemaVersion,
		Run:           &RunInfo{RunID: "run-analysis", Epoch: 1, Validity: "valid"},
		Profiles: &ProfileManifest{
			RunID: "run-analysis", Epoch: 1, Validity: "valid",
			Executable: &buildinfo.ExecutableIdentity{SHA256: strings.Repeat("d", 64), Source: buildinfo.SourceProcSelfExe, Status: buildinfo.ExecutableStatusCaptured},
			Expected:   []ProfileExpectation{{Kind: "cpu", Mode: "interval", Inputs: []ProfileExpectedInput{{Kind: "cpu", Point: "interval", File: "cpu_fixture.pprof"}}}},
			CPU: &CPUIntervalCapture{
				RunID: "run-analysis", Epoch: 1, CaptureID: strings.Repeat("1", 32),
				ExpectedFile: "cpu_fixture.pprof", File: "cpu_fixture.pprof", SHA256: profileHash,
				Bytes: int64(len(profileBytes)), Sidecar: analysisSidecarName, SidecarSHA256: hashBytes(analysisSidecarBody),
				CoverageFile: analysisCoverageName, CoverageSHA256: hashBytes(analysisCoverageBody), Status: "published",
			},
		},
	}}
	publication, err := h.writeSnapshot(snapshot, base)
	if err != nil {
		t.Fatal(err)
	}
	return base, publication.SnapshotSHA256, profileHash
}

func validWebAnalysis(t *testing.T, base, snapshotHash, profileHash string, value int64) profilemodel.ProfileAnalysisV1 {
	t.Helper()
	analysis := profilemodel.ProfileAnalysisV1{
		SchemaVersion: profilemodel.SchemaVersionV1, SnapshotBase: base,
		SnapshotSHA256: snapshotHash, SnapshotSchemaVersion: schemaVersion,
		RunID: "run-analysis", GeneratedAt: time.Date(2026, 8, 6, 12, 0, int(value), 0, time.UTC),
		Analyzer: profilemodel.AnalyzerProvenance{
			Version: "test", Executable: profilemodel.ExecutableIdentity{SHA256: strings.Repeat("c", 64), Source: profilemodel.ExecutableSourceProcSelfExe, Status: profilemodel.ExecutableStatusCaptured},
			Isolation: profilemodel.WorkerIsolation{Mode: profilemodel.IsolationLinuxCgroupV2, Bootstrap: profilemodel.BootstrapCgroupFDSIGSTOP, MemoryMaxBytes: 512 << 20, AddressSpaceMaxBytes: 1 << 30, HardLimitVerified: true, StoppedVerified: true, MembershipVerified: true},
		},
		Binary: profilemodel.BinaryProvenance{
			Captured: profilemodel.ExecutableIdentity{SHA256: strings.Repeat("d", 64), Source: profilemodel.ExecutableSourceProcSelfExe, Status: profilemodel.ExecutableStatusCaptured},
			Analyzed: &profilemodel.ExecutableIdentity{SHA256: strings.Repeat("d", 64), Source: profilemodel.ExecutableSourceInputFile, Status: profilemodel.ExecutableStatusCaptured},
			Match:    profilemodel.BinaryMatchVerified,
		},
		Status: profilemodel.AnalysisStatusOK,
		Attempts: []profilemodel.ProfileAttempt{{
			Kind: "cpu", Mode: profilemodel.ProfileModeInterval, Status: profilemodel.AnalysisStatusOK,
			Coverage:       profilemodel.ProfileCoverage{Complete: true},
			ExpectedInputs: []profilemodel.ExpectedProfileInput{{Kind: "cpu", Point: "interval", File: "cpu_fixture.pprof"}},
			ObservedInputs: []profilemodel.ObservedProfileInput{{
				ExpectedFile: "cpu_fixture.pprof", File: "cpu_fixture.pprof", SHA256: profileHash,
				Sidecar: analysisSidecarName, SidecarSHA256: hashBytes(analysisSidecarBody),
				CoverageFile: analysisCoverageName, CoverageSHA256: hashBytes(analysisCoverageBody),
				Bytes: int64(len("bounded profile fixture")), Symbolized: true,
			}},
			Summaries: []profilemodel.ProfileSummary{{SampleType: "cpu", Unit: "nanoseconds", NetTotal: value, PositiveTotal: value, PercentDenominator: value, DenominatorMode: profilemodel.DenominatorNet, Reports: []profilemodel.ProfileReport{{Granularity: profilemodel.GranularityFunctions, TopFlat: []profilemodel.ProfileNode{{Function: "main.work", Value: value}}}}}},
		}},
	}
	signed, err := profilemodel.SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func publishAnalysis(t *testing.T, handler http.Handler, base, expected string, analysis profilemodel.ProfileAnalysisV1) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(ProfileAnalysisPublishRequest{ExpectedCurrentArtifactID: expected, Analysis: analysis})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/profile-analysis?snapshot="+base, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
