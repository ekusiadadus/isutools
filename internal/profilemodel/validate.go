package profilemodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func SetAnalysisID(in ProfileAnalysisV1) (ProfileAnalysisV1, error) {
	in.AnalysisID = ""
	body, err := CanonicalJSON(in)
	if err != nil {
		return ProfileAnalysisV1{}, err
	}
	sum := sha256.Sum256(body)
	in.AnalysisID = hex.EncodeToString(sum[:])
	return in, nil
}

func CanonicalJSON(in ProfileAnalysisV1) ([]byte, error) {
	if !in.GeneratedAt.IsZero() {
		in.GeneratedAt = in.GeneratedAt.UTC()
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(in); err != nil {
		return nil, fmt.Errorf("encode canonical profile analysis: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

func Decode(r io.Reader) (ProfileAnalysisV1, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxAnalysisBodyBytes+1))
	if err != nil {
		return ProfileAnalysisV1{}, fmt.Errorf("read profile analysis: %w", err)
	}
	if len(body) > MaxAnalysisBodyBytes {
		return ProfileAnalysisV1{}, fmt.Errorf("profile analysis exceeds %d bytes", MaxAnalysisBodyBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var out ProfileAnalysisV1
	if err := dec.Decode(&out); err != nil {
		return ProfileAnalysisV1{}, fmt.Errorf("decode profile analysis: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ProfileAnalysisV1{}, errors.New("decode profile analysis: trailing JSON value")
		}
		return ProfileAnalysisV1{}, fmt.Errorf("decode profile analysis trailing data: %w", err)
	}
	if err := Validate(out); err != nil {
		return ProfileAnalysisV1{}, err
	}
	return out, nil
}

func Validate(in ProfileAnalysisV1) error {
	if in.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("schema_version = %d, want %d", in.SchemaVersion, SchemaVersionV1)
	}
	if !validStatus(in.Status) {
		return fmt.Errorf("unknown analysis status %q", in.Status)
	}
	if !validHash(in.SnapshotSHA256) {
		return errors.New("snapshot_sha256 must be a full lowercase SHA-256")
	}
	if err := validateBoundedString("snapshot_base", in.SnapshotBase, 255); err != nil {
		return err
	}
	if filepath.Base(in.SnapshotBase) != in.SnapshotBase || in.SnapshotBase == "." {
		return errors.New("snapshot_base must be a basename")
	}
	if err := validateBoundedString("run_id", in.RunID, 128); err != nil {
		return err
	}
	if in.GeneratedAt.IsZero() {
		return errors.New("generated_at is required")
	}
	if err := validateBoundedString("analyzer version", in.Analyzer.Version, 256); err != nil {
		return err
	}
	if in.Analyzer.Revision != "" {
		if err := validateBoundedString("analyzer revision", in.Analyzer.Revision, 256); err != nil {
			return err
		}
	}
	if in.Analyzer.PprofVersion != "" {
		if err := validateBoundedString("pprof version", in.Analyzer.PprofVersion, 256); err != nil {
			return err
		}
	}
	if err := validateExecutable("analyzer executable", in.Analyzer.Executable); err != nil {
		return err
	}
	if err := validateBinary(in.Binary); err != nil {
		return err
	}
	if len(in.Diagnostics) > 100 {
		return errors.New("analysis diagnostics exceed 100 entries")
	}
	for i, diagnostic := range in.Diagnostics {
		if err := validateDiagnostic(diagnostic); err != nil {
			return fmt.Errorf("analysis diagnostics[%d]: %w", i, err)
		}
	}
	if len(in.Attempts) == 0 || len(in.Attempts) > MaxAttempts {
		return fmt.Errorf("attempt count %d is outside 1..%d", len(in.Attempts), MaxAttempts)
	}
	if err := validateIsolation(in); err != nil {
		return err
	}

	var summaries, nodes, observedFiles int
	for i := range in.Attempts {
		attemptSummaries, attemptNodes, err := validateAttempt(in.Attempts[i])
		if err != nil {
			return fmt.Errorf("attempt[%d]: %w", i, err)
		}
		summaries += attemptSummaries
		nodes += attemptNodes
		observedFiles += len(in.Attempts[i].ObservedInputs)
	}
	if summaries > MaxSummaries {
		return fmt.Errorf("summary count %d exceeds %d", summaries, MaxSummaries)
	}
	if nodes > MaxReportNodes {
		return fmt.Errorf("report node count %d exceeds %d", nodes, MaxReportNodes)
	}
	if observedFiles > 8 {
		return fmt.Errorf("observed profile file count %d exceeds 8", observedFiles)
	}
	if in.Status == AnalysisStatusFailed && summaries != 0 {
		return errors.New("failed analysis must not contain usable summaries")
	}
	if in.Status == AnalysisStatusOK {
		for _, attempt := range in.Attempts {
			if attempt.Status != AnalysisStatusOK {
				return errors.New("ok analysis contains a non-ok attempt")
			}
		}
	}
	if recomputed := RecomputeStatus(in); recomputed != in.Status {
		return fmt.Errorf("analysis status %q does not match recomputed status %q", in.Status, recomputed)
	}

	want, err := SetAnalysisID(in)
	if err != nil {
		return err
	}
	if in.AnalysisID != want.AnalysisID {
		return errors.New("analysis_id does not match canonical body")
	}
	body, err := CanonicalJSON(in)
	if err != nil {
		return err
	}
	if len(body) > MaxAnalysisBodyBytes {
		return fmt.Errorf("canonical analysis body exceeds %d bytes", MaxAnalysisBodyBytes)
	}
	return nil
}

// RecomputeStatus is the single status authority shared by analyzer and
// publisher. It intentionally derives the verdict only from bounded wire
// fields, never from a caller's top-level Status claim.
func RecomputeStatus(in ProfileAnalysisV1) string {
	usable := 0
	partial := in.Binary.Match != BinaryMatchVerified || in.Analyzer.Executable.Status != ExecutableStatusCaptured
	for _, diagnostic := range in.Diagnostics {
		partial = partial || diagnostic.Level != DiagnosticInfo
	}
	for _, attempt := range in.Attempts {
		usable += len(attempt.Summaries)
		if attempt.Status != AnalysisStatusOK || len(attempt.ObservedInputs) != len(attempt.ExpectedInputs) || !attempt.Coverage.Complete {
			partial = true
		}
		for _, diagnostic := range attempt.Diagnostics {
			partial = partial || diagnostic.Level != DiagnosticInfo
		}
	}
	if usable == 0 {
		return AnalysisStatusFailed
	}
	if partial {
		return AnalysisStatusPartial
	}
	return AnalysisStatusOK
}

func validateIsolation(in ProfileAnalysisV1) error {
	hasSummary, hasObserved := false, false
	for _, attempt := range in.Attempts {
		hasSummary = hasSummary || len(attempt.Summaries) > 0
		hasObserved = hasObserved || len(attempt.ObservedInputs) > 0
	}
	isolation := in.Analyzer.Isolation
	if hasSummary || hasObserved {
		if !isolation.HardLimitVerified || !isolation.StoppedVerified {
			return errors.New("profile-derived results require verified hard isolation and stopped bootstrap")
		}
		switch isolation.Mode {
		case IsolationLinuxCgroupV2:
			if isolation.Bootstrap != BootstrapCgroupFDSIGSTOP || !isolation.MembershipVerified ||
				isolation.MemoryMaxBytes == 0 || isolation.MemoryMaxBytes > MaxWorkerMemoryBytes ||
				isolation.AddressSpaceMaxBytes < isolation.MemoryMaxBytes || isolation.AddressSpaceMaxBytes > MaxWorkerAddressBytes {
				return errors.New("linux hard isolation requires cgroupfd-sigstop membership proof")
			}
		case IsolationDarwinRLIMIT:
			if isolation.Bootstrap != BootstrapRLIMITSIGSTOP || isolation.MembershipVerified || isolation.MemoryMaxBytes != 0 ||
				isolation.AddressSpaceMaxBytes == 0 || isolation.AddressSpaceMaxBytes > MaxWorkerAddressBytes {
				return errors.New("darwin hard isolation requires rlimit-sigstop")
			}
		default:
			return fmt.Errorf("unknown hard isolation mode %q", isolation.Mode)
		}
		return nil
	}
	if isolation.Mode == IsolationNotRequired {
		if isolation.Bootstrap != BootstrapNotRequired || isolation.HardLimitVerified || isolation.StoppedVerified ||
			isolation.MembershipVerified || isolation.MemoryMaxBytes != 0 || isolation.AddressSpaceMaxBytes != 0 {
			return errors.New("not-required isolation must not claim a worker bootstrap")
		}
		for _, attempt := range in.Attempts {
			if attempt.Status != AnalysisStatusFailed || len(attempt.ObservedInputs) != 0 || len(attempt.Summaries) != 0 {
				return errors.New("not-required isolation may contain missing expected inputs only")
			}
			found := false
			for _, diagnostic := range attempt.Diagnostics {
				found = found || diagnostic.Code == DiagnosticProfileMissing || diagnostic.Code == DiagnosticProfilePending ||
					diagnostic.Code == DiagnosticArtifactExpired
			}
			if !found {
				return errors.New("not-required isolation requires a missing, pending, or expired diagnostic")
			}
		}
		return nil
	}
	if isolation.Mode != IsolationUnavailable || isolation.Bootstrap != BootstrapUnavailable ||
		isolation.HardLimitVerified || isolation.StoppedVerified || isolation.MembershipVerified ||
		isolation.MemoryMaxBytes != 0 || isolation.AddressSpaceMaxBytes != 0 {
		return errors.New("summary-free failure must record unavailable hard isolation")
	}
	if in.Status != AnalysisStatusFailed {
		return errors.New("unavailable hard isolation must produce failed analysis")
	}
	for _, attempt := range in.Attempts {
		if attempt.Status != AnalysisStatusFailed || len(attempt.ObservedInputs) != 0 {
			return errors.New("unavailable hard isolation may contain expected metadata only")
		}
		found := false
		for _, diagnostic := range attempt.Diagnostics {
			found = found || diagnostic.Code == DiagnosticWorkerHardLimitUnavailable
		}
		if !found {
			return errors.New("unavailable hard isolation requires worker-hard-limit-unavailable diagnostic")
		}
	}
	return nil
}

func validateAttempt(attempt ProfileAttempt) (int, int, error) {
	if !validStatus(attempt.Status) {
		return 0, 0, fmt.Errorf("unknown status %q", attempt.Status)
	}
	if attempt.Kind == "" || len(attempt.Kind) > 64 {
		return 0, 0, errors.New("kind is required and limited to 64 bytes")
	}
	wantInputs := 0
	switch attempt.Mode {
	case ProfileModeInterval:
		wantInputs = 1
	case ProfileModeCumulativeDelta:
		wantInputs = 2
	default:
		return 0, 0, fmt.Errorf("unknown mode %q", attempt.Mode)
	}
	if len(attempt.ExpectedInputs) != wantInputs {
		return 0, 0, fmt.Errorf("expected_inputs has %d entries, want %d", len(attempt.ExpectedInputs), wantInputs)
	}
	expected := make(map[string]ExpectedProfileInput, len(attempt.ExpectedInputs))
	for i, input := range attempt.ExpectedInputs {
		if input.Kind != attempt.Kind {
			return 0, 0, fmt.Errorf("expected_inputs[%d] kind does not match attempt", i)
		}
		if err := validateBasename("expected input file", input.File); err != nil {
			return 0, 0, err
		}
		if attempt.Mode == ProfileModeInterval && input.Point != "interval" {
			return 0, 0, errors.New("interval attempt requires interval point")
		}
		if attempt.Mode == ProfileModeCumulativeDelta && input.Point != "open" && input.Point != "close" {
			return 0, 0, errors.New("cumulative attempt requires open/close points")
		}
		if _, duplicate := expected[input.File]; duplicate {
			return 0, 0, errors.New("duplicate expected input file")
		}
		expected[input.File] = input
	}
	observed := make(map[string]struct{}, len(attempt.ObservedInputs))
	for i, input := range attempt.ObservedInputs {
		if _, ok := expected[input.ExpectedFile]; !ok {
			return 0, 0, fmt.Errorf("observed_inputs[%d] expected_file is not declared", i)
		}
		if input.File != input.ExpectedFile {
			return 0, 0, fmt.Errorf("observed_inputs[%d] file does not match expected_file", i)
		}
		if err := validateBasename("observed input file", input.File); err != nil {
			return 0, 0, err
		}
		if !validHash(input.SHA256) || input.Bytes < 0 {
			return 0, 0, fmt.Errorf("observed_inputs[%d] has invalid hash or byte count", i)
		}
		if _, duplicate := observed[input.ExpectedFile]; duplicate {
			return 0, 0, errors.New("duplicate observed input")
		}
		observed[input.ExpectedFile] = struct{}{}
		if err := validateOptionalArtifactPair("sidecar", input.Sidecar, input.SidecarSHA256); err != nil {
			return 0, 0, fmt.Errorf("observed_inputs[%d]: %w", i, err)
		}
		if err := validateOptionalArtifactPair("coverage", input.CoverageFile, input.CoverageSHA256); err != nil {
			return 0, 0, fmt.Errorf("observed_inputs[%d]: %w", i, err)
		}
	}
	if attempt.Status == AnalysisStatusFailed && len(attempt.Summaries) != 0 {
		return 0, 0, errors.New("failed attempt must not contain summaries")
	}
	if attempt.Status == AnalysisStatusOK && len(attempt.Summaries) == 0 {
		return 0, 0, errors.New("ok attempt requires a summary")
	}
	if attempt.Status == AnalysisStatusOK && len(attempt.ObservedInputs) != len(attempt.ExpectedInputs) {
		return 0, 0, errors.New("ok attempt requires every expected input to be observed")
	}
	if attempt.Status == AnalysisStatusPartial && len(attempt.Summaries) == 0 {
		return 0, 0, errors.New("partial attempt requires a usable summary")
	}
	for i, diagnostic := range attempt.Diagnostics {
		if err := validateDiagnostic(diagnostic); err != nil {
			return 0, 0, fmt.Errorf("diagnostics[%d]: %w", i, err)
		}
	}
	nodes := 0
	seenSampleType := make(map[string]struct{}, len(attempt.Summaries))
	for i, summary := range attempt.Summaries {
		key := summary.SampleType + "\x00" + summary.Unit
		if _, duplicate := seenSampleType[key]; duplicate {
			return 0, 0, errors.New("duplicate sample type summary")
		}
		seenSampleType[key] = struct{}{}
		count, err := validateSummary(summary, attempt.Mode)
		if err != nil {
			return 0, 0, fmt.Errorf("summaries[%d]: %w", i, err)
		}
		nodes += count
	}
	return len(attempt.Summaries), nodes, nil
}

func validateSummary(summary ProfileSummary, mode string) (int, error) {
	if err := validateBoundedString("sample_type", summary.SampleType, 64); err != nil {
		return 0, err
	}
	if err := validateBoundedString("unit", summary.Unit, 32); err != nil {
		return 0, err
	}
	if summary.PositiveTotal < 0 || summary.NegativeMagnitude < 0 || summary.PercentDenominator < 0 {
		return 0, errors.New("totals and denominator magnitudes must be non-negative")
	}
	negative, ok := CheckedNegate(summary.NegativeMagnitude)
	if !ok {
		return 0, errors.New("negative_magnitude cannot be negated")
	}
	net, ok := CheckedAdd(summary.PositiveTotal, negative)
	if !ok || net != summary.NetTotal {
		return 0, errors.New("net_total does not equal positive_total - negative_magnitude")
	}
	switch summary.DenominatorMode {
	case DenominatorNet:
		if summary.NegativeMagnitude != 0 || summary.PercentDenominator != summary.PositiveTotal {
			return 0, errors.New("net denominator requires a non-negative profile total")
		}
	case DenominatorAbsoluteAddress:
		want, ok := CheckedAdd(summary.PositiveTotal, summary.NegativeMagnitude)
		if !ok || summary.PercentDenominator != want {
			return 0, errors.New("absolute-address denominator does not match total magnitude")
		}
	case DenominatorBaseTotal:
		if summary.PercentDenominator == 0 {
			return 0, errors.New("base-total denominator must be positive")
		}
	case DenominatorNone:
		if summary.PercentDenominator != 0 {
			return 0, errors.New("none denominator must be zero")
		}
	default:
		return 0, fmt.Errorf("unknown denominator mode %q", summary.DenominatorMode)
	}
	if mode == ProfileModeInterval && summary.NegativeMagnitude != 0 {
		return 0, errors.New("interval summary contains a negative sample")
	}
	if err := validateLabels(summary.Labels); err != nil {
		return 0, err
	}
	nodes := 0
	seenGranularity := make(map[string]struct{}, len(summary.Reports))
	for i, report := range summary.Reports {
		if _, duplicate := seenGranularity[report.Granularity]; duplicate {
			return 0, errors.New("duplicate report granularity")
		}
		seenGranularity[report.Granularity] = struct{}{}
		count, err := validateReport(report)
		if err != nil {
			return 0, fmt.Errorf("reports[%d]: %w", i, err)
		}
		nodes += count
	}
	return nodes, nil
}

func validateLabels(labels []LabelBreakdown) error {
	if len(labels) > 4 {
		return errors.New("logical label breakdowns exceed 4 keys")
	}
	seenKeys := make(map[string]struct{}, len(labels))
	for i, label := range labels {
		switch label.Key {
		case "http.method", "http.route", "isutools.scenario", "isutools.region":
		default:
			return fmt.Errorf("labels[%d] has unknown logical key %q", i, label.Key)
		}
		if _, duplicate := seenKeys[label.Key]; duplicate {
			return fmt.Errorf("labels[%d] duplicates key %q", i, label.Key)
		}
		seenKeys[label.Key] = struct{}{}
		if len(label.Values) == 0 || len(label.Values) > 256 {
			return fmt.Errorf("labels[%d] value count is outside 1..256", i)
		}
		seenValues := make(map[string]struct{}, len(label.Values))
		for j, value := range label.Values {
			if err := validateBoundedString("label value", value.Value, 128); err != nil {
				return fmt.Errorf("labels[%d].values[%d]: %w", i, j, err)
			}
			if _, duplicate := seenValues[value.Value]; duplicate {
				return fmt.Errorf("labels[%d] duplicates value %q", i, value.Value)
			}
			seenValues[value.Value] = struct{}{}
		}
	}
	return nil
}

func validateReport(report ProfileReport) (int, error) {
	lists := []struct {
		nodes    []ProfileNode
		negative bool
	}{
		{nodes: report.TopFlat}, {nodes: report.TopCumulative},
		{nodes: report.TopNegativeFlat, negative: true}, {nodes: report.TopNegativeCumulative, negative: true},
	}
	for _, list := range lists {
		if len(list.nodes) > MaxTopNodes {
			return 0, fmt.Errorf("top list exceeds %d nodes", MaxTopNodes)
		}
		for _, node := range list.nodes {
			if list.negative && node.Value >= 0 {
				return 0, errors.New("negative list contains a non-negative node")
			}
			if !list.negative && node.Value <= 0 {
				return 0, errors.New("positive list contains a non-positive node")
			}
			if err := validateNode(report.Granularity, node); err != nil {
				return 0, err
			}
		}
	}
	switch report.Granularity {
	case GranularityFunctions, GranularityFileFunctions, GranularityFiles, GranularityLines:
	default:
		return 0, fmt.Errorf("unknown granularity %q", report.Granularity)
	}
	return len(report.TopFlat) + len(report.TopCumulative) + len(report.TopNegativeFlat) + len(report.TopNegativeCumulative), nil
}

func validateBinary(binary BinaryProvenance) error {
	if err := validateExecutable("binary captured", binary.Captured); err != nil {
		return err
	}
	if binary.Analyzed != nil {
		if err := validateExecutable("binary analyzed", *binary.Analyzed); err != nil {
			return err
		}
	}
	switch binary.Match {
	case BinaryMatchUnknown:
		return nil
	case BinaryMatchVerified:
		if binary.Analyzed == nil || binary.Captured.Status != ExecutableStatusCaptured || binary.Analyzed.Status != ExecutableStatusCaptured ||
			(binary.Captured.Source != ExecutableSourceProcSelfExe && binary.Captured.Source != ExecutableSourcePlatformBound) ||
			!validHash(binary.Captured.SHA256) || binary.Captured.SHA256 != binary.Analyzed.SHA256 {
			return errors.New("binary verified requires identical full SHA-256 from a bound running image and analyzed input")
		}
		return nil
	case BinaryMatchMismatch:
		if binary.Analyzed == nil || !validHash(binary.Captured.SHA256) || !validHash(binary.Analyzed.SHA256) || binary.Captured.SHA256 == binary.Analyzed.SHA256 {
			return errors.New("binary mismatch requires two different full SHA-256 identities")
		}
		return nil
	default:
		return fmt.Errorf("unknown binary match %q", binary.Match)
	}
}

func validateExecutable(field string, identity ExecutableIdentity) error {
	switch identity.Source {
	case ExecutableSourceProcSelfExe, ExecutableSourcePlatformBound, ExecutableSourceInputFile,
		ExecutableSourcePathUnbound, ExecutableSourceUnavailable:
	default:
		return fmt.Errorf("%s source %q is unknown", field, identity.Source)
	}
	switch identity.Status {
	case ExecutableStatusCaptured:
		if !validHash(identity.SHA256) {
			return fmt.Errorf("%s captured status requires full SHA-256", field)
		}
	case ExecutableStatusUnavailable, ExecutableStatusChangedDuringRead:
	case "":
		return fmt.Errorf("%s status is required", field)
	default:
		return fmt.Errorf("%s status %q is unknown", field, identity.Status)
	}
	if identity.SHA256 != "" && !validHash(identity.SHA256) {
		return fmt.Errorf("%s SHA-256 is invalid", field)
	}
	if identity.BuildInfoSHA256 != "" && !validHash(identity.BuildInfoSHA256) {
		return fmt.Errorf("%s build info SHA-256 is invalid", field)
	}
	for _, fieldValue := range []struct{ name, value string }{
		{name: "go_version", value: identity.GoVersion},
		{name: "main_module", value: identity.MainModule},
		{name: "main_version", value: identity.MainVersion},
		{name: "main_sum", value: identity.MainSum},
		{name: "vcs_revision", value: identity.VCSRevision},
	} {
		name, value := fieldValue.name, fieldValue.value
		if value != "" {
			if err := validateBoundedString(field+" "+name, value, 256); err != nil {
				return err
			}
		}
	}
	if len(identity.Settings) > 32 {
		return fmt.Errorf("%s build settings exceed 32 entries", field)
	}
	seen := make(map[string]struct{}, len(identity.Settings))
	total := 0
	for _, setting := range identity.Settings {
		if !safeBuildSetting(setting.Key) {
			return fmt.Errorf("%s build setting %q is not in the safe allowlist", field, setting.Key)
		}
		if _, duplicate := seen[setting.Key]; duplicate {
			return fmt.Errorf("%s duplicate build setting %q", field, setting.Key)
		}
		seen[setting.Key] = struct{}{}
		if err := validateBoundedString(field+" build setting key", setting.Key, 64); err != nil {
			return err
		}
		if err := validateBoundedString(field+" build setting value", setting.Value, 256); err != nil {
			return err
		}
		total += len(setting.Key) + len(setting.Value)
		if total > 8<<10 {
			return fmt.Errorf("%s build settings exceed 8 KiB", field)
		}
	}
	return nil
}

func safeBuildSetting(key string) bool {
	switch key {
	case "GOOS", "GOARCH", "CGO_ENABLED", "-buildmode", "-compiler", "-tags", "-trimpath", "pgo", "ldflags",
		"GO386", "GOAMD64", "GOARM", "GOARM64", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM":
		return true
	default:
		return false
	}
}

func validateOptionalArtifactPair(field, file, hash string) error {
	if (file == "") != (hash == "") {
		return fmt.Errorf("%s file and hash must appear together", field)
	}
	if file == "" {
		return nil
	}
	if err := validateBasename(field+" file", file); err != nil {
		return err
	}
	if !validHash(hash) {
		return fmt.Errorf("%s hash is invalid", field)
	}
	return nil
}

func validateNode(granularity string, node ProfileNode) error {
	if node.Line < 0 {
		return errors.New("line must be non-negative")
	}
	if node.Function != "" {
		if err := validateBoundedString("function", node.Function, 512); err != nil {
			return err
		}
	}
	if node.File != "" {
		if err := validateBoundedString("file", node.File, 512); err != nil {
			return err
		}
	}
	switch granularity {
	case GranularityFunctions:
		if node.Function == "" || node.File != "" || node.Line != 0 {
			return errors.New("functions node must contain function only")
		}
	case GranularityFileFunctions:
		if node.Function == "" || node.File == "" || node.Line != 0 {
			return errors.New("filefunctions node must contain function and file")
		}
	case GranularityFiles:
		if node.Function != "" || node.File == "" || node.Line != 0 {
			return errors.New("files node must contain file only")
		}
	case GranularityLines:
		if node.Function == "" || node.File == "" {
			return errors.New("lines node must contain function and file")
		}
	}
	return nil
}

func validateDiagnostic(d Diagnostic) error {
	if d.Level != DiagnosticInfo && d.Level != DiagnosticWarn && d.Level != DiagnosticError {
		return fmt.Errorf("unknown level %q", d.Level)
	}
	if err := validateBoundedString("diagnostic code", d.Code, 64); err != nil {
		return err
	}
	if !validDiagnosticCode(d.Code) {
		return fmt.Errorf("unknown diagnostic code %q", d.Code)
	}
	return validateBoundedString("diagnostic message", d.Message, 1024)
}

func validDiagnosticCode(code string) bool {
	switch code {
	case DiagnosticProfileMissing, DiagnosticProfileTooLarge, DiagnosticProfileInvalid,
		DiagnosticSampleTypeIncompatible, DiagnosticUnsymbolized, DiagnosticBinaryMismatch,
		DiagnosticNegativeDelta, DiagnosticCoverageTruncated, DiagnosticLabelOverflow,
		DiagnosticSourcePathRedacted, DiagnosticAnalysisTimeout, DiagnosticProfileInterference,
		DiagnosticProfilePending, DiagnosticArtifactExpired, DiagnosticArtifactMutated,
		DiagnosticCPUStartStalled, DiagnosticCPUStopStalled, DiagnosticProvenanceUnavailable,
		DiagnosticWorkerMemoryLimit, DiagnosticWorkerHardLimitUnavailable,
		DiagnosticSampleValueOverflow, DiagnosticNegativeIntervalSample,
		DiagnosticForeignProfileLabel, DiagnosticOutputTruncated, DiagnosticDurabilityUnknown:
		return true
	default:
		return false
	}
}

func validateBasename(field, value string) error {
	if err := validateBoundedString(field, value, 255); err != nil {
		return err
	}
	if filepath.Base(value) != value || value == "." || value == ".." {
		return fmt.Errorf("%s must be a basename", field)
	}
	return nil
}

func validateBoundedString(field, value string, max int) error {
	if value == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !utf8.ValidString(value) || len(value) > max || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("%s is invalid or exceeds %d bytes", field, max)
	}
	return nil
}

func validStatus(status string) bool {
	return status == AnalysisStatusOK || status == AnalysisStatusPartial || status == AnalysisStatusFailed
}

func validHash(value string) bool {
	return len(value) == SHA256HexLength && isLowerHex(value)
}

func isLowerHex(value string) bool {
	for _, c := range []byte(value) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
