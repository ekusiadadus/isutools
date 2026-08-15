package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/internal/pprofanalyze"
	"github.com/ekusiadadus/isutools/internal/profilecapture"
	"github.com/ekusiadadus/isutools/internal/profilemodel"
	"github.com/ekusiadadus/isutools/internal/safefs"
	"github.com/ekusiadadus/isutools/web"
)

var errTestWriter = errors.New("test writer failed")

type testErrorWriter struct{}

func (testErrorWriter) Write([]byte) (int, error) { return 0, errTestWriter }

func TestPreflightChecksEveryRetentionAndCapacityGate(t *testing.T) {
	capabilities := web.ProfileAnalysisCapabilities{
		Schema: "isutools.profile-analysis-capabilities/v1", StrongAtomicVisibility: true,
		RetentionRuns: 4, RetentionBytes: 1 << 30, PerRunCeilingBytes: 100 << 20,
		ProfileUsageKnown: true, DataDirAvailableKnown: true, DataDirAvailableBytes: 1 << 30,
		ExpectedProfileFilesPerRun: 1, CurrentGeneration: 9,
	}
	admin := installHTTPHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/profile-analysis-capabilities" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		_ = json.NewEncoder(writer).Encode(capabilities)
	}))
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"preflight", "--admin", admin, "--block-runs", "2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "generation=9") {
		t.Fatalf("preflight summary = %q", stdout.String())
	}
	if err := runPreflight([]string{"--admin", admin, "--block-runs", "2"}, testErrorWriter{}, &stderr); !errors.Is(err, errTestWriter) {
		t.Fatalf("preflight output failure = %v", err)
	}

	original := capabilities
	unsafeCases := []func(*web.ProfileAnalysisCapabilities){
		func(value *web.ProfileAnalysisCapabilities) { value.StrongAtomicVisibility = false },
		func(value *web.ProfileAnalysisCapabilities) {
			value.ExpectedProfileFilesPerRun, value.PerRunCeilingBytes = 0, 0
		},
		func(value *web.ProfileAnalysisCapabilities) { value.RetentionRuns = 1 },
		func(value *web.ProfileAnalysisCapabilities) { value.RetentionBytes = 1 },
		func(value *web.ProfileAnalysisCapabilities) { value.ProfileUsageKnown = false },
		func(value *web.ProfileAnalysisCapabilities) { value.DataDirAvailableKnown = false },
		func(value *web.ProfileAnalysisCapabilities) { value.PerRunCeilingBytes = ^uint64(0) },
	}
	for index, mutate := range unsafeCases {
		capabilities = original
		mutate(&capabilities)
		stdout.Reset()
		stderr.Reset()
		if code := runCLI([]string{"preflight", "--admin", admin, "--block-runs", "2"}, &stdout, &stderr); code != 3 {
			t.Fatalf("unsafe preflight %d code=%d, want 3; stderr=%s", index, code, stderr.String())
		}
	}
}

func TestFetchAnalyzeObservedProfileUsesVerifiedWorkerAndBinaryIdentity(t *testing.T) {
	base := "20260806-130000_gen2_deadbeef"
	profileName := "cpu_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.pprof"
	profileBody := []byte("untrusted profile fixture")
	sidecarName := "cpu_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.meta.json"
	coverageName := "cpu_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.coverage.000001.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json"
	sidecarBody := []byte("{\"phase\":\"initial\"}\n")
	coverageBody := []byte("{\"phase\":\"coverage\"}\n")
	targetBody := []byte("exact target binary")
	targetHash := hashBytes(targetBody)
	executable := buildinfo.ExecutableIdentity{SHA256: targetHash, Source: buildinfo.SourceProcSelfExe, Status: buildinfo.ExecutableStatusCaptured}
	labelScope := profilecapture.NewStandaloneLabelScope(strings.Repeat("b", 32))
	labelScope.Seal()
	labelDictionary := labelScope.Dictionary("run-2", 2)
	snapshot := struct {
		web.Snapshot
		Prev *web.Snapshot `json:"prev,omitempty"`
	}{Snapshot: web.Snapshot{Meta: web.Meta{
		SchemaVersion: 3, Run: &web.RunInfo{RunID: "run-2", Epoch: 2, Validity: "valid"},
		Profiles: &web.ProfileManifest{
			RunID: "run-2", Epoch: 2, Validity: "valid", Executable: &executable,
			Expected: []web.ProfileExpectation{{Kind: "cpu", Mode: profilemodel.ProfileModeInterval, Inputs: []web.ProfileExpectedInput{{Kind: "cpu", Point: "interval", File: profileName}}}},
			CPU: &web.CPUIntervalCapture{
				RunID: "run-2", Epoch: 2, CaptureID: strings.Repeat("b", 32), ExpectedFile: profileName,
				File: profileName, SHA256: hashBytes(profileBody), Bytes: int64(len(profileBody)),
				Sidecar: sidecarName, SidecarSHA256: hashBytes(sidecarBody), CoverageFile: coverageName, CoverageSHA256: hashBytes(coverageBody),
				Status: "published", RunSpanNs: int64(time.Second), CaptureSpanNs: int64(time.Second), StopReason: "finish-accepted", Complete: true,
			},
			CPULabelDictionary: &web.CPULabelDictionary{RunID: labelDictionary.RunID, Epoch: labelDictionary.Epoch, CaptureID: labelDictionary.CaptureID, Sealed: labelDictionary.Sealed, SHA256: labelDictionary.SHA256},
		},
	}}}
	snapshotBody, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	admin := installHTTPHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/files/" + base + ".json":
			_, _ = writer.Write(snapshotBody)
		case "/files/" + profileName:
			_, _ = writer.Write(profileBody)
		case "/files/" + sidecarName:
			_, _ = writer.Write(sidecarBody)
		case "/files/" + coverageName:
			_, _ = writer.Write(coverageBody)
		default:
			http.NotFound(writer, request)
		}
	}))
	bundleDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	fetchArgs := []string{"--admin", admin, "--snapshot-base", base, "--snapshot-sha256", hashBytes(snapshotBody), "--bundle-dir", bundleDir}
	if code := runCLI(append([]string{"fetch"}, fetchArgs...), &stdout, &stderr); code != 0 {
		t.Fatalf("fetch code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if err := runFetch(fetchArgs, testErrorWriter{}, &stderr); !errors.Is(err, errTestWriter) {
		t.Fatalf("fetch output failure = %v", err)
	}
	for name, body := range map[string][]byte{sidecarName: sidecarBody, coverageName: coverageBody} {
		fetched, err := os.ReadFile(bundleDir + "/" + name)
		if err != nil || !bytes.Equal(fetched, body) {
			t.Fatalf("fetched attachment %s = %q err=%v", name, fetched, err)
		}
	}
	previousWorker := launchAnalysisWorker
	launchAnalysisWorker = func(_ context.Context, _ string, job pprofanalyze.WorkerJob, _ pprofanalyze.WorkerOptions) (pprofanalyze.WorkerResult, error) {
		if job.Mode != profilemodel.ProfileModeInterval || len(job.Profiles) != 1 || job.Dictionary == nil || job.Dictionary.SHA256 != labelDictionary.SHA256 {
			t.Fatalf("worker job = %#v", job)
		}
		return pprofanalyze.WorkerResult{
			Isolation: profilemodel.WorkerIsolation{Mode: profilemodel.IsolationLinuxCgroupV2, Bootstrap: profilemodel.BootstrapCgroupFDSIGSTOP, MemoryMaxBytes: profilemodel.MaxWorkerMemoryBytes, AddressSpaceMaxBytes: profilemodel.MaxWorkerAddressBytes, HardLimitVerified: true, StoppedVerified: true, MembershipVerified: true},
			Summaries: []profilemodel.ProfileSummary{{SampleType: "cpu", Unit: "nanoseconds", NetTotal: 10, PositiveTotal: 10, PercentDenominator: 10, DenominatorMode: profilemodel.DenominatorNet}},
		}, nil
	}
	t.Cleanup(func() { launchAnalysisWorker = previousWorker })
	target := t.TempDir() + "/target"
	if err := os.WriteFile(target, targetBody, 0o700); err != nil {
		t.Fatal(err)
	}
	output := bundleDir + "/analysis-success.json"
	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"analyze", "--bundle-dir", bundleDir, "--binary", target, "--output", output, "--top", "10"}, &stdout, &stderr); code != 0 {
		t.Fatalf("analyze code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := profilemodel.Decode(file)
	_ = file.Close()
	if err != nil || analysis.Status != profilemodel.AnalysisStatusOK || analysis.Binary.Match != profilemodel.BinaryMatchVerified || len(analysis.Attempts[0].Summaries) != 1 ||
		analysis.Attempts[0].ObservedInputs[0].Sidecar != sidecarName || analysis.Attempts[0].ObservedInputs[0].SidecarSHA256 != hashBytes(sidecarBody) ||
		analysis.Attempts[0].ObservedInputs[0].CoverageFile != coverageName || analysis.Attempts[0].ObservedInputs[0].CoverageSHA256 != hashBytes(coverageBody) {
		t.Fatalf("analysis=%#v err=%v", analysis, err)
	}
	if _, err := runAnalyze([]string{"--bundle-dir", bundleDir, "--binary", target, "--output", bundleDir + "/analysis-output-error.json", "--top", "10"}, testErrorWriter{}, &stderr); !errors.Is(err, errTestWriter) {
		t.Fatalf("analysis output failure = %v", err)
	}
	if err := os.WriteFile(bundleDir+"/"+sidecarName, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"analyze", "--bundle-dir", bundleDir, "--output", bundleDir + "/tampered-sidecar.json"}, &stdout, &stderr); code != 4 || !strings.Contains(stderr.String(), "sidecar") {
		t.Fatalf("tampered sidecar code=%d stderr=%s", code, stderr.String())
	}
}

func TestDataDirFetchCumulativeAnalysisHardFailureAndTamperDetection(t *testing.T) {
	base := "20260806-150000_gen4_deadbeef"
	openName := "20260806-150000_gen4_deadbeef_mutex_open.pprof"
	closeName := "20260806-150000_gen4_deadbeef_mutex_close.pprof"
	openBody, closeBody := []byte("open profile"), []byte("close profile")
	executable := buildIdentityFixture
	snapshot := struct {
		web.Snapshot
		Prev *web.Snapshot `json:"prev,omitempty"`
	}{Snapshot: web.Snapshot{Meta: web.Meta{
		SchemaVersion: 3, Run: &web.RunInfo{RunID: "run-4", Epoch: 4, Validity: "valid"},
		Profiles: &web.ProfileManifest{
			RunID: "run-4", Epoch: 4, Validity: "valid", Executable: &executable,
			Expected: []web.ProfileExpectation{{Kind: "mutex", Mode: profilemodel.ProfileModeCumulativeDelta, Inputs: []web.ProfileExpectedInput{{Kind: "mutex", Point: "open", File: openName}, {Kind: "mutex", Point: "close", File: closeName}}}},
			Captures: []web.ProfileCapture{
				{RunID: "run-4", Epoch: 4, Kind: "mutex", Point: web.ProfilePointOpen, File: openName, SHA256: hashBytes(openBody), Bytes: int64(len(openBody)), Status: "ok"},
				{RunID: "run-4", Epoch: 4, Kind: "mutex", Point: web.ProfilePointClose, File: closeName, SHA256: hashBytes(closeBody), Bytes: int64(len(closeBody)), Status: "ok"},
			},
			Pairs: []web.ProfilePair{{Kind: "mutex", OpenFile: openName, CloseFile: closeName, OpenSHA256: hashBytes(openBody), CloseSHA256: hashBytes(closeBody), RunSpanNs: int64(2 * time.Second)}},
		},
	}}}
	snapshotBody, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir, bundleDir := t.TempDir(), t.TempDir()
	for name, body := range map[string][]byte{base + ".json": snapshotBody, openName: openBody, closeName: closeBody} {
		if err := os.WriteFile(sourceDir+"/"+name, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"fetch", "--data-dir", sourceDir, "--snapshot-base", base, "--snapshot-sha256", hashBytes(snapshotBody), "--bundle-dir", bundleDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("fetch code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	previousWorker := launchAnalysisWorker
	t.Cleanup(func() { launchAnalysisWorker = previousWorker })
	launchAnalysisWorker = func(_ context.Context, _ string, job pprofanalyze.WorkerJob, _ pprofanalyze.WorkerOptions) (pprofanalyze.WorkerResult, error) {
		if job.Mode != profilemodel.ProfileModeCumulativeDelta || len(job.Profiles) != 2 {
			t.Fatalf("worker job = %#v", job)
		}
		return pprofanalyze.WorkerResult{
			Isolation: profilemodel.WorkerIsolation{Mode: profilemodel.IsolationLinuxCgroupV2, Bootstrap: profilemodel.BootstrapCgroupFDSIGSTOP, MemoryMaxBytes: profilemodel.MaxWorkerMemoryBytes, AddressSpaceMaxBytes: profilemodel.MaxWorkerAddressBytes, HardLimitVerified: true, StoppedVerified: true, MembershipVerified: true},
			Summaries: []profilemodel.ProfileSummary{{SampleType: "contentions", Unit: "count", NetTotal: 5, PositiveTotal: 5, PercentDenominator: 5, DenominatorMode: profilemodel.DenominatorAbsoluteAddress}},
		}, nil
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"analyze", "--bundle-dir", bundleDir, "--output", "-"}, &stdout, &stderr); code != 0 {
		t.Fatalf("analyze code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	analysis, err := profilemodel.Decode(strings.NewReader(stdout.String()))
	if err != nil || analysis.Status != profilemodel.AnalysisStatusPartial || len(analysis.Attempts[0].Summaries) != 1 {
		t.Fatalf("analysis=%#v err=%v", analysis, err)
	}
	if _, err := runAnalyze([]string{"--bundle-dir", bundleDir, "--output", "-"}, testErrorWriter{}, &stderr); !errors.Is(err, errTestWriter) {
		t.Fatalf("analysis stdout failure = %v", err)
	}

	launchAnalysisWorker = func(context.Context, string, pprofanalyze.WorkerJob, pprofanalyze.WorkerOptions) (pprofanalyze.WorkerResult, error) {
		return pprofanalyze.WorkerResult{}, pprofanalyze.ErrHardIsolationUnavailable
	}
	failedOutput := bundleDir + "/hard-failed.json"
	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"analyze", "--bundle-dir", bundleDir, "--output", failedOutput}, &stdout, &stderr); code != 4 {
		t.Fatalf("hard failure code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	file, err := os.Open(failedOutput)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := profilemodel.Decode(file)
	_ = file.Close()
	if err != nil || failed.Analyzer.Isolation.Mode != profilemodel.IsolationUnavailable || failed.Attempts[0].Diagnostics[0].Code != profilemodel.DiagnosticWorkerHardLimitUnavailable {
		t.Fatalf("failed analysis=%#v err=%v", failed, err)
	}

	if err := os.WriteFile(bundleDir+"/"+openName, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"analyze", "--bundle-dir", bundleDir, "--output", bundleDir + "/tampered.json"}, &stdout, &stderr); code != 4 || !strings.Contains(stderr.String(), "hash mismatch") {
		t.Fatalf("tampered code=%d stderr=%s", code, stderr.String())
	}
}

func TestPublishRequiresExplicitCASAndNeverRetriesConflict(t *testing.T) {
	analysis := missingAnalysisFixture(t)
	path := t.TempDir() + "/analysis.json"
	body, err := profilemodel.CanonicalJSON(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	conflict := false
	admin := installHTTPHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodPost || request.URL.Path != "/profile-analysis" || request.URL.Query().Get("snapshot") != analysis.SnapshotBase {
			t.Fatalf("publish request = %s %s", request.Method, request.URL)
		}
		if conflict {
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"current_artifact_id":"` + strings.Repeat("d", 64) + `"}`))
			return
		}
		var envelope web.ProfileAnalysisPublishRequest
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil || envelope.ExpectedCurrentArtifactID != "none" || envelope.Analysis.AnalysisID != analysis.AnalysisID {
			t.Fatalf("publish envelope=%#v err=%v", envelope, err)
		}
		_ = json.NewEncoder(writer).Encode(web.ProfileAnalysisPublishResponse{AnalysisID: analysis.AnalysisID, ArtifactID: strings.Repeat("c", 64), CommitSequence: 1, Durability: "durable"})
	}))
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"publish", "--admin", admin, "--analysis", path, "--expected-current", "none"}, &stdout, &stderr); code != 0 {
		t.Fatalf("publish code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if err := runPublish([]string{"--admin", admin, "--analysis", path, "--expected-current", "none"}, testErrorWriter{}, &stderr); !errors.Is(err, errTestWriter) {
		t.Fatalf("publish output failure = %v", err)
	}
	conflict = true
	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"publish", "--admin", admin, "--analysis", path, "--expected-current", "none"}, &stdout, &stderr); code != 5 || requests != 3 {
		t.Fatalf("conflict code=%d requests=%d stderr=%s", code, requests, stderr.String())
	}
	if code := runCLI([]string{"publish", "--admin", admin, "--analysis", path}, &stdout, &stderr); code != 2 || requests != 3 {
		t.Fatalf("missing CAS code=%d requests=%d", code, requests)
	}
}

func TestCLIUsageAndSecurityValidation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"preflight"},
		{"fetch", "--admin", "file:///tmp", "--data-dir", "/tmp", "--snapshot-base", "../bad"},
		{"analyze", "--bundle-dir", "/tmp", "--top", "51"},
		{"publish", "--admin", "http://user:secret@example.test", "--analysis", "x", "--expected-current", "none"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runCLI(args, &stdout, &stderr); code != 2 {
			t.Errorf("runCLI(%q)=%d stderr=%q", args, code, stderr.String())
		}
	}
	if _, err := validateAdminOrigin("https://example.test/path"); err == nil {
		t.Fatal("accepted admin URL with path")
	}
	if _, overflow := checkedMultiply(^uint64(0), 2); !overflow {
		t.Fatal("missed checked multiplication overflow")
	}
}

func TestBundlePublicationAndBoundedProtocolHelpers(t *testing.T) {
	dir := t.TempDir()
	root, err := safefs.Open(dir, safefs.Options{RequireStrongVisibility: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := publishBundleFile(root, "immutable.json", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := publishBundleFile(root, "immutable.json", []byte("first")); err != nil {
		t.Fatalf("idempotent immutable publish: %v", err)
	}
	if err := publishBundleFile(root, "immutable.json", []byte("different")); !errors.Is(err, safefs.ErrExists) {
		t.Fatalf("immutable replacement = %v", err)
	}
	if err := replaceBundleFile(root, "current.json", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := replaceBundleFile(root, "current.json", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if body, err := root.ReadFile("current.json", 16); err != nil || string(body) != "two" {
		t.Fatalf("current body=%q err=%v", body, err)
	}
	if _, err := loadBundle(root); err == nil {
		t.Fatal("accepted missing bundle marker")
	}
	if err := decodeStrict([]byte(`{} {}`), &struct{}{}); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	long := strings.Repeat("x", 700) + "\n"
	if got := boundedText([]byte(long)); len(got) != 512 || strings.Contains(got, "\n") {
		t.Fatalf("bounded text len=%d", len(got))
	}
	if got := diagnostic(profilemodel.DiagnosticError, profilemodel.DiagnosticProfileInvalid, long); len(got.Message) != 512 {
		t.Fatalf("bounded diagnostic len=%d", len(got.Message))
	}

	admin := installHTTPHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(bytes.Repeat([]byte("x"), 33))
	}))
	if _, _, err := httpRequest(context.Background(), http.MethodGet, admin, "", nil, 32); err == nil {
		t.Fatal("accepted oversized HTTP response")
	}
	if _, _, err := makeFetchSource(context.Background(), "file:///tmp", ""); err == nil {
		t.Fatal("accepted non-HTTP fetch origin")
	}
	if _, _, err := makeFetchSource(context.Background(), "", dir+"/missing"); err == nil {
		t.Fatal("accepted missing source DataDir")
	}
	if err := writePrivateFile("/", []byte("x")); err == nil {
		t.Fatal("accepted directory as output file")
	}
	if err := decodeStrict([]byte(`{"unknown":true}`), &struct{}{}); err == nil {
		t.Fatal("accepted unknown JSON field")
	}
}

func TestFetchedArtifactIdentityRejectsMalformedAndConflictingEvidence(t *testing.T) {
	files := make(map[string]fetchedArtifact)
	hashA, hashB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := addFetchedArtifact(files, "cpu_fixture.meta.json", hashA, profilecapture.MaxCompletionRecordBytes); err != nil {
		t.Fatal(err)
	}
	if err := addFetchedArtifact(files, "cpu_fixture.meta.json", hashA, profilecapture.MaxCompletionRecordBytes); err != nil {
		t.Fatalf("idempotent artifact identity: %v", err)
	}
	for _, input := range []struct {
		name  string
		hash  string
		limit int64
	}{
		{name: "../escape.json", hash: hashA, limit: 1},
		{name: "evidence.json", hash: "bad", limit: 1},
		{name: "evidence.json", hash: hashA, limit: 0},
		{name: "cpu_fixture.meta.json", hash: hashB, limit: profilecapture.MaxCompletionRecordBytes},
	} {
		if err := addFetchedArtifact(files, input.name, input.hash, input.limit); err == nil {
			t.Fatalf("accepted malformed/conflicting artifact: %#v", input)
		}
	}

	profileName := "cpu_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.pprof"
	manifest := &web.ProfileManifest{
		RunID: "run-1", Executable: &buildIdentityFixture,
		Expected: []web.ProfileExpectation{{Kind: "cpu", Mode: profilemodel.ProfileModeInterval, Inputs: []web.ProfileExpectedInput{{Kind: "cpu", Point: "interval", File: profileName}}}},
		CPU:      &web.CPUIntervalCapture{ExpectedFile: profileName, File: profileName, SHA256: hashA, Bytes: 1, Sidecar: "cpu.meta.json"},
	}
	if _, _, err := bundleFromManifest("20260806-150000_gen1_deadbeef", hashA, 3, manifest); err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("malformed sidecar pair err=%v", err)
	}
}

func TestTraceOnlyBundleAndHandoffRecipesAreHashBound(t *testing.T) {
	traceName := "trace_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.out"
	sidecarName := "trace_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.meta.json"
	traceBody := []byte("go trace fixture")
	sidecarBody := []byte("{\"state\":\"published\"}\n")
	manifest := &web.ProfileManifest{
		RunID: "run-trace", Epoch: 7, Executable: &buildIdentityFixture,
		Trace: &web.TraceIntervalCapture{
			RunID: "run-trace", Epoch: 7, CaptureID: strings.Repeat("a", 32), ExpectedFile: traceName,
			File: traceName, SHA256: hashBytes(traceBody), Bytes: int64(len(traceBody)), Sidecar: sidecarName,
			SidecarSHA256: hashBytes(sidecarBody), Status: "published", Complete: true,
		},
	}
	bundle, files, err := bundleFromManifest("20260815-120000_gen7_deadbeef", strings.Repeat("b", 64), 3, manifest)
	if err != nil || bundle.Trace == nil || len(bundle.Attempts) != 0 || len(files) != 2 {
		t.Fatalf("bundle=%#v files=%#v err=%v", bundle, files, err)
	}

	directory := t.TempDir()
	root, err := safefs.Open(directory, safefs.Options{RequireStrongVisibility: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	snapshotBody := []byte("{}")
	bundle.SnapshotSHA256 = hashBytes(snapshotBody)
	for name, body := range map[string][]byte{
		bundle.SnapshotFile: snapshotBody,
		traceName:           traceBody,
		sidecarName:         sidecarBody,
	} {
		if err := publishBundleFile(root, name, body); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := publishBundleManifest(root, &bundle); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := runRecipes([]string{"--bundle-dir", directory, "--output", "shell"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "'go' 'tool' 'trace' '"+traceName+"'") || strings.Contains(got, "pprof") {
		t.Fatalf("trace recipe=%q", got)
	}
	if err := os.WriteFile(directory+"/"+traceName, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runRecipes([]string{"--bundle-dir", directory}, &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "trace hash mismatch") {
		t.Fatalf("tampered trace err=%v", err)
	}
}

func TestRecipeValidationFailsClosedOnCorruptOrUnsupportedProfile(t *testing.T) {
	directory := t.TempDir()
	root, err := safefs.Open(directory, safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := publishBundleFile(root, "cpu.pprof", []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	attempt := bundleAttempt{Kind: "cpu", Mode: profilemodel.ProfileModeInterval, Observed: []profilemodel.ObservedProfileInput{{File: "cpu.pprof"}}}
	previous := launchAnalysisWorker
	t.Cleanup(func() { launchAnalysisWorker = previous })
	launchAnalysisWorker = func(context.Context, string, pprofanalyze.WorkerJob, pprofanalyze.WorkerOptions) (pprofanalyze.WorkerResult, error) {
		return pprofanalyze.WorkerResult{ErrorCode: profilemodel.DiagnosticProfileInvalid}, nil
	}
	if types, ready := validateRecipeAttempt(root, attempt, time.Second); ready || len(types) != 0 {
		t.Fatalf("corrupt profile types=%v ready=%v", types, ready)
	}
	launchAnalysisWorker = func(context.Context, string, pprofanalyze.WorkerJob, pprofanalyze.WorkerOptions) (pprofanalyze.WorkerResult, error) {
		return pprofanalyze.WorkerResult{Summaries: []profilemodel.ProfileSummary{{SampleType: "alloc_space", Unit: "bytes"}, {SampleType: "alloc_objects", Unit: "count"}}}, nil
	}
	types, ready := validateRecipeAttempt(root, attempt, time.Second)
	if !ready || len(types) != 2 || types[0].Type != "alloc_objects" || types[1].Unit != "bytes" {
		t.Fatalf("types=%v ready=%v", types, ready)
	}
}

func TestSourcePathsAreRelativeOrRedactedWithoutLeakingHostPaths(t *testing.T) {
	t.Parallel()
	summaries := []profilemodel.ProfileSummary{{Reports: []profilemodel.ProfileReport{{
		Granularity: profilemodel.GranularityLines,
		TopFlat: []profilemodel.ProfileNode{
			{Function: "no-file", Value: 3},
			{File: "/repo/pkg/main.go", Line: 10, Value: 2},
			{File: "/private/secret/token.go", Line: 20, Value: 1},
			{File: "relative/file.go", Line: 30, Value: 1},
			{File: "../escape.go", Line: 40, Value: 1},
		},
	}}}}
	if !sanitizeSourcePaths(summaries, "/repo") {
		t.Fatal("outside paths were not reported as redacted")
	}
	got := summaries[0].Reports[0].TopFlat
	if got[0].File != "" || got[1].File != "pkg/main.go" || got[2].File != "(redacted)" || got[3].File != "relative/file.go" || got[4].File != "(redacted)" {
		t.Fatalf("sanitized nodes = %#v", got)
	}
	withoutRoot := []profilemodel.ProfileSummary{{Reports: []profilemodel.ProfileReport{{TopFlat: []profilemodel.ProfileNode{{File: "/host/path.go", Value: 1}}}}}}
	if !sanitizeSourcePaths(withoutRoot, "") || withoutRoot[0].Reports[0].TopFlat[0].File != "(redacted)" {
		t.Fatalf("absolute path without root = %#v", withoutRoot)
	}
	encoded, _ := json.Marshal(summaries)
	if strings.Contains(string(encoded), "/private/secret") || strings.Contains(string(encoded), "../escape") {
		t.Fatalf("source path leaked: %s", encoded)
	}
}

func TestFetchThenAnalyzeMissingExpectedProfileWithoutReadingWorker(t *testing.T) {
	base := "20260806-120000_gen1_deadbeef"
	snapshot := struct {
		web.Snapshot
		Prev *web.Snapshot `json:"prev,omitempty"`
	}{Snapshot: web.Snapshot{Meta: web.Meta{
		SchemaVersion: 3,
		Run:           &web.RunInfo{RunID: "run-1", Epoch: 1, Validity: "valid"},
		Profiles: &web.ProfileManifest{
			RunID: "run-1", Epoch: 1, Validity: "valid",
			Expected:   []web.ProfileExpectation{{Kind: "cpu", Mode: profilemodel.ProfileModeInterval, Inputs: []web.ProfileExpectedInput{{Kind: "cpu", Point: "interval", File: "cpu_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.pprof"}}}},
			Executable: &buildIdentityFixture,
		},
	}}}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	admin := installHTTPHandler(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/files/"+base+".json" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(body)
	}))
	bundleDir := t.TempDir()
	snapshotHash := hashBytes(body)
	var stdout, stderr bytes.Buffer
	if code := runCLI([]string{"fetch", "--admin", admin, "--snapshot-base", base, "--snapshot-sha256", snapshotHash, "--bundle-dir", bundleDir}, &stdout, &stderr); code != 0 {
		t.Fatalf("fetch code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	output := bundleDir + "/analysis.json"
	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"analyze", "--bundle-dir", bundleDir, "--output", output}, &stdout, &stderr); code != 4 {
		t.Fatalf("missing-only analyze code=%d, want 4; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := profilemodel.Decode(file)
	_ = file.Close()
	if err != nil || analysis.Status != profilemodel.AnalysisStatusFailed || analysis.Analyzer.Isolation.Mode != profilemodel.IsolationNotRequired {
		t.Fatalf("analysis=%#v err=%v", analysis, err)
	}
}

var buildIdentityFixture = buildinfo.ExecutableIdentity{
	SHA256: strings.Repeat("a", 64), Source: buildinfo.SourceProcSelfExe, Status: buildinfo.ExecutableStatusCaptured,
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func installHTTPHandler(t *testing.T, handler http.Handler) string {
	t.Helper()
	previous := commandHTTPClient
	commandHTTPClient = &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			result := response.Result()
			result.Request = request
			return result, nil
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t.Cleanup(func() { commandHTTPClient = previous })
	return "http://admin.test"
}

func missingAnalysisFixture(t *testing.T) profilemodel.ProfileAnalysisV1 {
	t.Helper()
	analysis := profilemodel.ProfileAnalysisV1{
		SchemaVersion: 1, SnapshotBase: "20260806-140000_gen3_deadbeef", SnapshotSHA256: strings.Repeat("f", 64), SnapshotSchemaVersion: 3,
		RunID: "run-3", GeneratedAt: time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC), Status: profilemodel.AnalysisStatusFailed,
		Analyzer: profilemodel.AnalyzerProvenance{
			Version: "test", Executable: profilemodel.ExecutableIdentity{SHA256: strings.Repeat("e", 64), Source: profilemodel.ExecutableSourceInputFile, Status: profilemodel.ExecutableStatusCaptured},
			Isolation: profilemodel.WorkerIsolation{Mode: profilemodel.IsolationNotRequired, Bootstrap: profilemodel.BootstrapNotRequired},
		},
		Binary: profilemodel.BinaryProvenance{
			Captured: profilemodel.ExecutableIdentity{SHA256: strings.Repeat("a", 64), Source: profilemodel.ExecutableSourceProcSelfExe, Status: profilemodel.ExecutableStatusCaptured},
			Match:    profilemodel.BinaryMatchUnknown,
		},
		Attempts: []profilemodel.ProfileAttempt{{
			Kind: "cpu", Mode: profilemodel.ProfileModeInterval, Status: profilemodel.AnalysisStatusFailed,
			ExpectedInputs: []profilemodel.ExpectedProfileInput{{Kind: "cpu", Point: "interval", File: "cpu_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.pprof"}},
			Diagnostics:    []profilemodel.Diagnostic{{Level: profilemodel.DiagnosticError, Code: profilemodel.DiagnosticProfileMissing, Message: "profile was not captured"}},
		}},
	}
	signed, err := profilemodel.SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := profilemodel.Validate(signed); err != nil {
		t.Fatal(err)
	}
	return signed
}
