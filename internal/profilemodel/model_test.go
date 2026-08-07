package profilemodel

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func validAnalysis() ProfileAnalysisV1 {
	return ProfileAnalysisV1{
		SchemaVersion:         SchemaVersionV1,
		SnapshotBase:          "20260806-130000.000000000-000001_gen1_deadbee",
		SnapshotSHA256:        strings.Repeat("a", SHA256HexLength),
		SnapshotSchemaVersion: 3,
		RunID:                 "run-0123456789abcdef",
		GeneratedAt:           time.Date(2026, 8, 6, 4, 0, 0, 123, time.UTC),
		Analyzer: AnalyzerProvenance{
			Version:    "test",
			Executable: ExecutableIdentity{SHA256: strings.Repeat("c", SHA256HexLength), Source: ExecutableSourceProcSelfExe, Status: ExecutableStatusCaptured},
			Isolation: WorkerIsolation{
				Mode:                 IsolationLinuxCgroupV2,
				Bootstrap:            BootstrapCgroupFDSIGSTOP,
				MemoryMaxBytes:       512 << 20,
				AddressSpaceMaxBytes: 1 << 30,
				HardLimitVerified:    true,
				StoppedVerified:      true,
				MembershipVerified:   true,
			},
		},
		Binary: BinaryProvenance{
			Captured: ExecutableIdentity{SHA256: strings.Repeat("d", SHA256HexLength), Source: ExecutableSourceProcSelfExe, Status: ExecutableStatusCaptured},
			Analyzed: &ExecutableIdentity{SHA256: strings.Repeat("d", SHA256HexLength), Source: ExecutableSourceInputFile, Status: ExecutableStatusCaptured},
			Match:    BinaryMatchVerified,
		},
		Status: AnalysisStatusOK,
		Attempts: []ProfileAttempt{{
			Kind:     "cpu",
			Mode:     ProfileModeInterval,
			Status:   AnalysisStatusOK,
			Coverage: ProfileCoverage{Complete: true},
			ExpectedInputs: []ExpectedProfileInput{{
				Kind: "cpu", Point: "interval", File: "cpu_interval.pprof",
			}},
			ObservedInputs: []ObservedProfileInput{{
				ExpectedFile: "cpu_interval.pprof",
				File:         "cpu_interval.pprof",
				SHA256:       strings.Repeat("b", SHA256HexLength),
				Bytes:        128,
			}},
			Summaries: []ProfileSummary{{
				SampleType:         "cpu",
				Unit:               "nanoseconds",
				NetTotal:           42,
				PositiveTotal:      42,
				PercentDenominator: 42,
				DenominatorMode:    DenominatorNet,
				Reports: []ProfileReport{{
					Granularity: GranularityFunctions,
					TopFlat:     []ProfileNode{{Function: "main.work", Value: 42}},
				}},
			}},
		}},
	}
}

func TestValidateRecomputesTopLevelStatus(t *testing.T) {
	analysis := validAnalysis()
	analysis.Binary.Match = BinaryMatchUnknown
	analysis.Status = AnalysisStatusOK
	analysis, err := SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "recomputed") {
		t.Fatalf("Validate status = %v", err)
	}
	analysis.Status = AnalysisStatusPartial
	analysis, err = SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(analysis); err != nil {
		t.Fatalf("Validate partial: %v", err)
	}
}

func TestAnalysisIDDeterministicAndFullSHA256(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	first, err := SetAnalysisID(analysis)
	if err != nil {
		t.Fatalf("SetAnalysisID: %v", err)
	}
	second, err := SetAnalysisID(analysis)
	if err != nil {
		t.Fatalf("SetAnalysisID replay: %v", err)
	}
	if first.AnalysisID != second.AnalysisID {
		t.Fatalf("IDs differ: %q vs %q", first.AnalysisID, second.AnalysisID)
	}
	if len(first.AnalysisID) != SHA256HexLength || !isLowerHex(first.AnalysisID) {
		t.Fatalf("AnalysisID = %q, want full lowercase SHA-256", first.AnalysisID)
	}
	if err := Validate(first); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDecodeRejectsUnknownAndTrailingFields(t *testing.T) {
	t.Parallel()

	analysis, err := SetAnalysisID(validAnalysis())
	if err != nil {
		t.Fatal(err)
	}
	body, err := CanonicalJSON(analysis)
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(body, []byte(`"schema_version":1`), []byte(`"schema_version":1,"unknown":true`), 1)
	if _, err := Decode(bytes.NewReader(unknown)); err == nil {
		t.Fatal("Decode accepted an unknown field")
	}
	if _, err := Decode(bytes.NewReader(append(body, []byte(` {}`)...))); err == nil {
		t.Fatal("Decode accepted a trailing JSON value")
	}
}

func TestValidateRequiresHardIsolationForUsableSummary(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	analysis.Analyzer.Isolation = WorkerIsolation{Mode: IsolationUnavailable, Bootstrap: BootstrapUnavailable}
	analysis, err := SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "hard isolation") {
		t.Fatalf("Validate = %v, want hard isolation rejection", err)
	}
}

func TestUnavailableIsolationAllowsMetadataOnlyFailure(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	analysis.Status = AnalysisStatusFailed
	analysis.Analyzer.Isolation = WorkerIsolation{Mode: IsolationUnavailable, Bootstrap: BootstrapUnavailable}
	analysis.Attempts[0].Status = AnalysisStatusFailed
	analysis.Attempts[0].ObservedInputs = nil
	analysis.Attempts[0].Summaries = nil
	analysis.Attempts[0].Diagnostics = []Diagnostic{{
		Level: DiagnosticError, Code: DiagnosticWorkerHardLimitUnavailable,
		Message: "hard worker isolation is unavailable",
	}}
	analysis, err := SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(analysis); err != nil {
		t.Fatalf("Validate metadata-only failure: %v", err)
	}

	analysis.Attempts[0].Summaries = validAnalysis().Attempts[0].Summaries
	analysis, err = SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(analysis); err == nil {
		t.Fatal("Validate accepted profile-derived summaries without hard isolation")
	}
}

func TestVerifiedIsolationAllowsProfileDerivedFailureWithoutSummary(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	analysis.Status = AnalysisStatusFailed
	analysis.Attempts[0].Status = AnalysisStatusFailed
	analysis.Attempts[0].Summaries = nil
	analysis.Attempts[0].Coverage.Complete = false
	analysis.Attempts[0].Diagnostics = []Diagnostic{{
		Level: DiagnosticError, Code: DiagnosticProfileInvalid, Message: "profile protobuf is invalid",
	}}
	analysis = signedAnalysis(t, analysis)
	if err := Validate(analysis); err != nil {
		t.Fatalf("Validate verified failed parse: %v", err)
	}
}

func TestNotRequiredIsolationRepresentsMissingCaptureWithoutFakeWorkerFailure(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	analysis.Status = AnalysisStatusFailed
	analysis.Analyzer.Isolation = WorkerIsolation{Mode: IsolationNotRequired, Bootstrap: BootstrapNotRequired}
	analysis.Attempts[0].Status = AnalysisStatusFailed
	analysis.Attempts[0].ObservedInputs = nil
	analysis.Attempts[0].Summaries = nil
	analysis.Attempts[0].Coverage.Complete = false
	analysis.Attempts[0].Diagnostics = []Diagnostic{{
		Level: DiagnosticError, Code: DiagnosticProfileMissing, Message: "capture was not published",
	}}
	analysis = signedAnalysis(t, analysis)
	if err := Validate(analysis); err != nil {
		t.Fatalf("Validate missing capture: %v", err)
	}
}

func TestValidateExpectedObservedInputs(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	analysis.Attempts[0].ObservedInputs[0].ExpectedFile = "other.pprof"
	analysis, err := SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "expected_file") {
		t.Fatalf("Validate = %v, want observed/expected mismatch", err)
	}
}

func TestValidateRejectsInvalidDenominatorIdentity(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	analysis.Attempts[0].Summaries[0].NegativeMagnitude = 1
	analysis, err := SetAnalysisID(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "net_total") {
		t.Fatalf("Validate = %v, want total identity rejection", err)
	}
}

func signedAnalysis(t *testing.T, analysis ProfileAnalysisV1) ProfileAnalysisV1 {
	t.Helper()
	result, err := SetAnalysisID(analysis)
	if err != nil {
		t.Fatalf("SetAnalysisID: %v", err)
	}
	return result
}

func TestValidateAcceptsSupportedIsolationAndReportVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		isolation   WorkerIsolation
		granularity string
		node        ProfileNode
	}{
		{
			name: "linux-functions",
			isolation: WorkerIsolation{
				Mode: IsolationLinuxCgroupV2, Bootstrap: BootstrapCgroupFDSIGSTOP,
				MemoryMaxBytes: 512 << 20, AddressSpaceMaxBytes: 1 << 30,
				HardLimitVerified: true, StoppedVerified: true, MembershipVerified: true,
			},
			granularity: GranularityFunctions,
			node:        ProfileNode{Function: "main.work", Value: 1},
		},
		{
			name: "darwin-filefunctions",
			isolation: WorkerIsolation{
				Mode: IsolationDarwinRLIMIT, Bootstrap: BootstrapRLIMITSIGSTOP,
				AddressSpaceMaxBytes: 1 << 30,
				HardLimitVerified:    true, StoppedVerified: true,
			},
			granularity: GranularityFileFunctions,
			node:        ProfileNode{Function: "main.work", File: "main.go", Value: 1},
		},
		{
			name: "files",
			isolation: WorkerIsolation{
				Mode: IsolationLinuxCgroupV2, Bootstrap: BootstrapCgroupFDSIGSTOP,
				MemoryMaxBytes: 512 << 20, AddressSpaceMaxBytes: 1 << 30,
				HardLimitVerified: true, StoppedVerified: true, MembershipVerified: true,
			},
			granularity: GranularityFiles,
			node:        ProfileNode{File: "main.go", Value: 1},
		},
		{
			name: "lines",
			isolation: WorkerIsolation{
				Mode: IsolationLinuxCgroupV2, Bootstrap: BootstrapCgroupFDSIGSTOP,
				MemoryMaxBytes: 512 << 20, AddressSpaceMaxBytes: 1 << 30,
				HardLimitVerified: true, StoppedVerified: true, MembershipVerified: true,
			},
			granularity: GranularityLines,
			node:        ProfileNode{Function: "main.work", File: "main.go", Line: 42, Value: 1},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			analysis := validAnalysis()
			analysis.Analyzer.Isolation = tt.isolation
			report := &analysis.Attempts[0].Summaries[0].Reports[0]
			report.Granularity = tt.granularity
			report.TopFlat = []ProfileNode{tt.node}
			analysis = signedAnalysis(t, analysis)
			if err := Validate(analysis); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestValidateDenominatorModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    string
		net     int64
		pos     int64
		neg     int64
		denom   int64
		wantErr string
	}{
		{name: "absolute", mode: DenominatorAbsoluteAddress, net: 5, pos: 8, neg: 3, denom: 11},
		{name: "base", mode: DenominatorBaseTotal, net: 5, pos: 8, neg: 3, denom: 20},
		{name: "none", mode: DenominatorNone, net: 5, pos: 8, neg: 3},
		{name: "bad-net", mode: DenominatorNet, net: 5, pos: 8, neg: 3, denom: 8, wantErr: "net denominator"},
		{name: "bad-absolute", mode: DenominatorAbsoluteAddress, net: 5, pos: 8, neg: 3, denom: 10, wantErr: "absolute-address"},
		{name: "bad-base", mode: DenominatorBaseTotal, net: 5, pos: 8, neg: 3, wantErr: "base-total"},
		{name: "bad-none", mode: DenominatorNone, net: 5, pos: 8, neg: 3, denom: 1, wantErr: "none denominator"},
		{name: "unknown", mode: "mystery", net: 5, pos: 8, neg: 3, denom: 1, wantErr: "unknown denominator"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			analysis := validAnalysis()
			analysis.Status = AnalysisStatusPartial
			analysis.Attempts[0].Status = AnalysisStatusPartial
			analysis.Attempts[0].Mode = ProfileModeCumulativeDelta
			analysis.Attempts[0].ExpectedInputs = []ExpectedProfileInput{
				{Kind: "cpu", Point: "open", File: "cpu_open.pprof"},
				{Kind: "cpu", Point: "close", File: "cpu_close.pprof"},
			}
			analysis.Attempts[0].ObservedInputs = nil
			summary := &analysis.Attempts[0].Summaries[0]
			summary.NetTotal = tt.net
			summary.PositiveTotal = tt.pos
			summary.NegativeMagnitude = tt.neg
			summary.PercentDenominator = tt.denom
			summary.DenominatorMode = tt.mode
			analysis = signedAnalysis(t, analysis)
			err := Validate(analysis)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRejectsMalformedContractFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*ProfileAnalysisV1)
		wantErr string
	}{
		{name: "schema", mutate: func(a *ProfileAnalysisV1) { a.SchemaVersion++ }, wantErr: "schema_version"},
		{name: "status", mutate: func(a *ProfileAnalysisV1) { a.Status = "unknown" }, wantErr: "analysis status"},
		{name: "snapshot-hash", mutate: func(a *ProfileAnalysisV1) { a.SnapshotSHA256 = "ABC" }, wantErr: "snapshot_sha256"},
		{name: "snapshot-path", mutate: func(a *ProfileAnalysisV1) { a.SnapshotBase = "../snapshot" }, wantErr: "snapshot_base"},
		{name: "run-control", mutate: func(a *ProfileAnalysisV1) { a.RunID = "bad\nrun" }, wantErr: "run_id"},
		{name: "generated-at", mutate: func(a *ProfileAnalysisV1) { a.GeneratedAt = time.Time{} }, wantErr: "generated_at"},
		{name: "mode", mutate: func(a *ProfileAnalysisV1) { a.Attempts[0].Mode = "unknown" }, wantErr: "unknown mode"},
		{name: "expected-path", mutate: func(a *ProfileAnalysisV1) { a.Attempts[0].ExpectedInputs[0].File = "../cpu.pprof" }, wantErr: "basename"},
		{name: "observed-hash", mutate: func(a *ProfileAnalysisV1) { a.Attempts[0].ObservedInputs[0].SHA256 = "bad" }, wantErr: "invalid hash"},
		{name: "diagnostic-level", mutate: func(a *ProfileAnalysisV1) {
			a.Attempts[0].Diagnostics = []Diagnostic{{Level: "fatal", Code: "bad", Message: "bad"}}
		}, wantErr: "unknown level"},
		{name: "report-granularity", mutate: func(a *ProfileAnalysisV1) { a.Attempts[0].Summaries[0].Reports[0].Granularity = "addresses" }, wantErr: "unknown granularity"},
		{name: "line-node", mutate: func(a *ProfileAnalysisV1) {
			r := &a.Attempts[0].Summaries[0].Reports[0]
			r.Granularity = GranularityLines
			r.TopFlat = []ProfileNode{{Function: "main.work", File: "main.go", Line: -1, Value: 1}}
		}, wantErr: "line must"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			analysis := validAnalysis()
			tt.mutate(&analysis)
			analysis = signedAnalysis(t, analysis)
			err := Validate(analysis)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateLabelBreakdownClosedSetAndBounds(t *testing.T) {
	analysis := validAnalysis()
	analysis.Attempts[0].Summaries[0].Labels = []LabelBreakdown{{
		Key: "http.route", Values: []LabelValue{{Value: "/users/{id}", Total: 42}},
	}}
	analysis = signedAnalysis(t, analysis)
	if err := Validate(analysis); err != nil {
		t.Fatalf("valid labels: %v", err)
	}

	for _, mutate := range []func(*ProfileAnalysisV1){
		func(a *ProfileAnalysisV1) { a.Attempts[0].Summaries[0].Labels[0].Key = "raw.path" },
		func(a *ProfileAnalysisV1) { a.Attempts[0].Summaries[0].Labels[0].Values[0].Value = "secret\nvalue" },
		func(a *ProfileAnalysisV1) {
			a.Attempts[0].Summaries[0].Labels[0].Values = make([]LabelValue, 257)
			for i := range a.Attempts[0].Summaries[0].Labels[0].Values {
				a.Attempts[0].Summaries[0].Labels[0].Values[i] = LabelValue{Value: "v", Total: 1}
			}
		},
	} {
		invalid := analysis
		invalid.Attempts = append([]ProfileAttempt(nil), analysis.Attempts...)
		invalid.Attempts[0].Summaries = append([]ProfileSummary(nil), analysis.Attempts[0].Summaries...)
		invalid.Attempts[0].Summaries[0].Labels = append([]LabelBreakdown(nil), analysis.Attempts[0].Summaries[0].Labels...)
		invalid.Attempts[0].Summaries[0].Labels[0].Values = append([]LabelValue(nil), analysis.Attempts[0].Summaries[0].Labels[0].Values...)
		mutate(&invalid)
		invalid = signedAnalysis(t, invalid)
		if err := Validate(invalid); err == nil {
			t.Fatal("Validate accepted malformed label breakdown")
		}
	}
}

func TestDecodeRejectsOversizedBodyAndMismatchedID(t *testing.T) {
	t.Parallel()

	if _, err := Decode(strings.NewReader(strings.Repeat("x", MaxAnalysisBodyBytes+1))); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Decode oversized body = %v", err)
	}
	analysis := signedAnalysis(t, validAnalysis())
	analysis.AnalysisID = strings.Repeat("0", SHA256HexLength)
	body, err := CanonicalJSON(analysis)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(bytes.NewReader(body)); err == nil || !strings.Contains(err.Error(), "analysis_id") {
		t.Fatalf("Decode mismatched ID = %v", err)
	}
}

func TestValidateBinaryVerifiedRequiresBoundRunningImageSHA(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("c", SHA256HexLength)
	validIdentity := ExecutableIdentity{
		SHA256: hash, Source: ExecutableSourceProcSelfExe, Status: ExecutableStatusCaptured,
	}
	t.Run("verified", func(t *testing.T) {
		t.Parallel()
		analysis := validAnalysis()
		analysis.Binary = BinaryProvenance{Captured: validIdentity, Analyzed: &ExecutableIdentity{
			SHA256: hash, Source: ExecutableSourceInputFile, Status: ExecutableStatusCaptured,
		}, Match: BinaryMatchVerified}
		analysis = signedAnalysis(t, analysis)
		if err := Validate(analysis); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})
	for _, tt := range []struct {
		name   string
		change func(*BinaryProvenance)
	}{
		{name: "no-analyzed", change: func(p *BinaryProvenance) { p.Analyzed = nil }},
		{name: "short-hash", change: func(p *BinaryProvenance) { p.Captured.SHA256 = "deadbee" }},
		{name: "path-unbound", change: func(p *BinaryProvenance) { p.Captured.Source = ExecutableSourcePathUnbound }},
		{name: "hash-mismatch", change: func(p *BinaryProvenance) { p.Analyzed.SHA256 = strings.Repeat("d", SHA256HexLength) }},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			analysis := validAnalysis()
			analyzed := validIdentity
			analyzed.Source = ExecutableSourceInputFile
			analysis.Binary = BinaryProvenance{Captured: validIdentity, Analyzed: &analyzed, Match: BinaryMatchVerified}
			tt.change(&analysis.Binary)
			analysis = signedAnalysis(t, analysis)
			if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "binary") {
				t.Fatalf("Validate = %v, want binary provenance rejection", err)
			}
		})
	}
}

func TestValidateRejectsUnsafeBuildSettingsAndObservedDuplicates(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	analysis.Binary.Captured = ExecutableIdentity{
		Source: ExecutableSourceUnavailable, Status: ExecutableStatusUnavailable,
		Settings: []BuildSetting{{Key: "SECRET_TOKEN", Value: "leak"}},
	}
	analysis = signedAnalysis(t, analysis)
	if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "build setting") {
		t.Fatalf("unsafe settings = %v", err)
	}

	analysis = validAnalysis()
	analysis.Attempts[0].ObservedInputs = append(analysis.Attempts[0].ObservedInputs, analysis.Attempts[0].ObservedInputs[0])
	analysis = signedAnalysis(t, analysis)
	if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "duplicate observed") {
		t.Fatalf("duplicate observed input = %v", err)
	}
}

func TestValidateRejectsWrongSignedReportLists(t *testing.T) {
	t.Parallel()

	analysis := validAnalysis()
	analysis.Attempts[0].Summaries[0].Reports[0].TopFlat[0].Value = -1
	analysis = signedAnalysis(t, analysis)
	if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("negative node in positive list = %v", err)
	}
	analysis = validAnalysis()
	report := &analysis.Attempts[0].Summaries[0].Reports[0]
	report.TopNegativeFlat = []ProfileNode{{Function: "main.loss", Value: 1}}
	analysis = signedAnalysis(t, analysis)
	if err := Validate(analysis); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("positive node in negative list = %v", err)
	}
}

func TestValidateExecutableContractMatrix(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("e", SHA256HexLength)
	valid := ExecutableIdentity{SHA256: hash, Source: ExecutableSourceProcSelfExe, Status: ExecutableStatusCaptured}
	if err := validateExecutable("binary", valid); err != nil {
		t.Fatalf("valid executable: %v", err)
	}
	for _, tt := range []struct {
		name   string
		change func(*ExecutableIdentity)
		want   string
	}{
		{name: "source", change: func(i *ExecutableIdentity) { i.Source = "pathname" }, want: "source"},
		{name: "status-empty", change: func(i *ExecutableIdentity) { i.Status = "" }, want: "status"},
		{name: "status-unknown", change: func(i *ExecutableIdentity) { i.Status = "broken" }, want: "status"},
		{name: "captured-no-hash", change: func(i *ExecutableIdentity) { i.SHA256 = "" }, want: "SHA-256"},
		{name: "build-hash", change: func(i *ExecutableIdentity) { i.BuildInfoSHA256 = "bad" }, want: "build info"},
		{name: "field", change: func(i *ExecutableIdentity) { i.GoVersion = strings.Repeat("g", 257) }, want: "go_version"},
		{name: "too-many-settings", change: func(i *ExecutableIdentity) {
			i.Settings = make([]BuildSetting, 33)
		}, want: "32 entries"},
		{name: "duplicate-setting", change: func(i *ExecutableIdentity) {
			i.Settings = []BuildSetting{{Key: "GOOS", Value: "linux"}, {Key: "GOOS", Value: "darwin"}}
		}, want: "duplicate"},
		{name: "empty-setting", change: func(i *ExecutableIdentity) {
			i.Settings = []BuildSetting{{Key: "GOOS", Value: ""}}
		}, want: "value"},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			identity := valid
			tt.change(&identity)
			if err := validateExecutable("binary", identity); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateExecutable = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateBinaryAndOptionalArtifactMatrix(t *testing.T) {
	t.Parallel()

	hashA, hashB := strings.Repeat("a", SHA256HexLength), strings.Repeat("b", SHA256HexLength)
	captured := ExecutableIdentity{SHA256: hashA, Source: ExecutableSourceProcSelfExe, Status: ExecutableStatusCaptured}
	analyzed := ExecutableIdentity{SHA256: hashB, Source: ExecutableSourceInputFile, Status: ExecutableStatusCaptured}
	if err := validateBinary(BinaryProvenance{Captured: captured, Analyzed: &analyzed, Match: BinaryMatchMismatch}); err != nil {
		t.Fatalf("valid mismatch: %v", err)
	}
	for _, provenance := range []BinaryProvenance{
		{Captured: captured, Match: BinaryMatchMismatch},
		{Captured: captured, Analyzed: &captured, Match: BinaryMatchMismatch},
		{Captured: captured, Match: "same-enough"},
	} {
		if err := validateBinary(provenance); err == nil {
			t.Fatalf("validateBinary accepted %#v", provenance)
		}
	}
	if err := validateOptionalArtifactPair("sidecar", "capture.meta.json", hashA); err != nil {
		t.Fatalf("valid sidecar: %v", err)
	}
	for _, tt := range []struct {
		file string
		hash string
	}{
		{file: "capture.meta.json"},
		{hash: hashA},
		{file: "../capture.meta.json", hash: hashA},
		{file: "capture.meta.json", hash: "bad"},
	} {
		if err := validateOptionalArtifactPair("sidecar", tt.file, tt.hash); err == nil {
			t.Fatalf("optional artifact accepted file=%q hash=%q", tt.file, tt.hash)
		}
	}
}

func TestDiagnosticCodesAreClosed(t *testing.T) {
	t.Parallel()

	if err := validateDiagnostic(Diagnostic{Level: DiagnosticError, Code: DiagnosticProfileMissing, Message: "missing"}); err != nil {
		t.Fatalf("known diagnostic: %v", err)
	}
	if err := validateDiagnostic(Diagnostic{Level: DiagnosticError, Code: "user-secret", Message: "bad"}); err == nil || !strings.Contains(err.Error(), "unknown diagnostic code") {
		t.Fatalf("unknown diagnostic = %v", err)
	}
}
