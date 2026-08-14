package profilemodel

import "time"

const (
	SchemaVersionV1      = 1
	SHA256HexLength      = 64
	MaxAnalysisBodyBytes = 2 << 20
	MaxAttempts          = 8
	MaxSummaries         = 32
	MaxTopNodes          = 50
	MaxReportNodes       = 2000
	MaxFlameNodes        = 2048
	MaxFlameDepth        = 64
	MaxWorkerMemoryBytes = uint64(512 << 20)
	// Go reserves substantially more virtual address space than resident
	// memory on 64-bit systems. Keep physical memory fail-closed at 512 MiB via
	// cgroup memory.max, while allowing a still-bounded 4 GiB address space so
	// real multi-second runtime CPU profiles can be decoded.
	MaxWorkerAddressBytes = uint64(4 << 30)
)

const (
	AnalysisStatusOK      = "ok"
	AnalysisStatusPartial = "partial"
	AnalysisStatusFailed  = "failed"

	ProfileModeInterval        = "interval"
	ProfileModeCumulativeDelta = "cumulative-delta"

	GranularityFunctions     = "functions"
	GranularityFileFunctions = "filefunctions"
	GranularityFiles         = "files"
	GranularityLines         = "lines"

	DenominatorNet             = "net"
	DenominatorAbsoluteAddress = "absolute-address"
	DenominatorBaseTotal       = "base-total"
	DenominatorNone            = "none"

	BinaryMatchVerified = "verified"
	BinaryMatchMismatch = "mismatch"
	BinaryMatchUnknown  = "unknown"

	ExecutableSourceProcSelfExe   = "proc-self-exe"
	ExecutableSourcePlatformBound = "platform-bound"
	ExecutableSourceInputFile     = "input-file"
	ExecutableSourcePathUnbound   = "path-unbound"
	ExecutableSourceUnavailable   = "unavailable"

	ExecutableStatusCaptured          = "captured"
	ExecutableStatusUnavailable       = "unavailable"
	ExecutableStatusChangedDuringRead = "changed-during-read"

	IsolationLinuxCgroupV2 = "linux-cgroup-v2"
	IsolationDarwinRLIMIT  = "darwin-rlimit"
	IsolationNotRequired   = "not-required"
	IsolationUnavailable   = "unavailable"

	BootstrapCgroupFDSIGSTOP = "cgroupfd-sigstop"
	BootstrapRLIMITSIGSTOP   = "rlimit-sigstop"
	BootstrapNotRequired     = "not-required"
	BootstrapUnavailable     = "unavailable"

	DiagnosticInfo  = "info"
	DiagnosticWarn  = "warn"
	DiagnosticError = "error"

	DiagnosticWorkerHardLimitUnavailable = "worker-hard-limit-unavailable"
	DiagnosticProfileMissing             = "profile-missing"
	DiagnosticProfileTooLarge            = "profile-too-large"
	DiagnosticProfileInvalid             = "profile-invalid"
	DiagnosticSampleTypeIncompatible     = "sample-type-incompatible"
	DiagnosticUnsymbolized               = "unsymbolized"
	DiagnosticBinaryMismatch             = "binary-mismatch"
	DiagnosticNegativeDelta              = "negative-delta"
	DiagnosticCoverageTruncated          = "coverage-truncated"
	DiagnosticLabelOverflow              = "label-overflow"
	DiagnosticSourcePathRedacted         = "source-path-redacted"
	DiagnosticAnalysisTimeout            = "analysis-timeout"
	DiagnosticProfileInterference        = "profile-interference"
	DiagnosticProfilePending             = "profile-pending"
	DiagnosticArtifactExpired            = "artifact-expired"
	DiagnosticArtifactMutated            = "artifact-mutated"
	DiagnosticCPUStartStalled            = "cpu-start-stalled"
	DiagnosticCPUStopStalled             = "cpu-stop-stalled"
	DiagnosticProvenanceUnavailable      = "provenance-unavailable"
	DiagnosticWorkerMemoryLimit          = "worker-memory-limit"
	DiagnosticSampleValueOverflow        = "sample-value-overflow"
	DiagnosticNegativeIntervalSample     = "negative-interval-sample"
	DiagnosticForeignProfileLabel        = "foreign-profile-label"
	DiagnosticOutputTruncated            = "output-truncated"
	DiagnosticDurabilityUnknown          = "durability-unknown"
)

type ProfileAnalysisV1 struct {
	SchemaVersion         int                `json:"schema_version"`
	AnalysisID            string             `json:"analysis_id"`
	SnapshotBase          string             `json:"snapshot_base"`
	SnapshotSHA256        string             `json:"snapshot_sha256"`
	SnapshotSchemaVersion int                `json:"snapshot_schema_version"`
	RunID                 string             `json:"run_id"`
	GeneratedAt           time.Time          `json:"generated_at"`
	Analyzer              AnalyzerProvenance `json:"analyzer"`
	Binary                BinaryProvenance   `json:"binary"`
	Status                string             `json:"status"`
	Diagnostics           []Diagnostic       `json:"diagnostics,omitempty"`
	Attempts              []ProfileAttempt   `json:"attempts"`
}

type AnalyzerProvenance struct {
	Version      string             `json:"version"`
	Revision     string             `json:"revision,omitempty"`
	Dirty        bool               `json:"dirty,omitempty"`
	PprofVersion string             `json:"pprof_version,omitempty"`
	Executable   ExecutableIdentity `json:"executable"`
	Isolation    WorkerIsolation    `json:"isolation"`
}

type WorkerIsolation struct {
	Mode                 string `json:"mode"`
	Bootstrap            string `json:"bootstrap"`
	MemoryMaxBytes       uint64 `json:"memory_max_bytes,omitempty"`
	AddressSpaceMaxBytes uint64 `json:"address_space_max_bytes,omitempty"`
	HardLimitVerified    bool   `json:"hard_limit_verified"`
	StoppedVerified      bool   `json:"stopped_verified"`
	MembershipVerified   bool   `json:"membership_verified,omitempty"`
}

type BinaryProvenance struct {
	Captured ExecutableIdentity  `json:"captured"`
	Analyzed *ExecutableIdentity `json:"analyzed,omitempty"`
	Match    string              `json:"match"`
}

type ExecutableIdentity struct {
	SHA256          string         `json:"sha256,omitempty"`
	BuildInfoSHA256 string         `json:"build_info_sha256,omitempty"`
	Source          string         `json:"source,omitempty"`
	GoVersion       string         `json:"go_version,omitempty"`
	MainModule      string         `json:"main_module,omitempty"`
	MainVersion     string         `json:"main_version,omitempty"`
	MainSum         string         `json:"main_sum,omitempty"`
	VCSRevision     string         `json:"vcs_revision,omitempty"`
	VCSModified     bool           `json:"vcs_modified,omitempty"`
	Settings        []BuildSetting `json:"settings,omitempty"`
	Status          string         `json:"status,omitempty"`
}

type BuildSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ExpectedProfileInput struct {
	Kind  string `json:"kind"`
	Point string `json:"point"`
	File  string `json:"file"`
}

type ObservedProfileInput struct {
	ExpectedFile   string `json:"expected_file"`
	File           string `json:"file"`
	SHA256         string `json:"sha256"`
	Sidecar        string `json:"sidecar,omitempty"`
	SidecarSHA256  string `json:"sidecar_sha256,omitempty"`
	CoverageFile   string `json:"coverage_file,omitempty"`
	CoverageSHA256 string `json:"coverage_sha256,omitempty"`
	Bytes          int64  `json:"bytes"`
	Symbolized     bool   `json:"symbolized"`
}

type ProfileAttempt struct {
	Kind           string                 `json:"kind"`
	Mode           string                 `json:"mode"`
	Status         string                 `json:"status"`
	Coverage       ProfileCoverage        `json:"coverage"`
	ExpectedInputs []ExpectedProfileInput `json:"expected_inputs"`
	ObservedInputs []ObservedProfileInput `json:"observed_inputs,omitempty"`
	Diagnostics    []Diagnostic           `json:"diagnostics,omitempty"`
	Summaries      []ProfileSummary       `json:"summaries,omitempty"`
	Flame          *FlameGraph            `json:"flame,omitempty"`
}

type FlameGraph struct {
	Status          string      `json:"status"`
	Reason          string      `json:"reason,omitempty"`
	Mode            string      `json:"mode"`
	SampleType      string      `json:"sample_type,omitempty"`
	Unit            string      `json:"unit,omitempty"`
	TotalWeight     int64       `json:"total_weight,omitempty"`
	Nodes           []FlameNode `json:"nodes,omitempty"`
	Truncated       bool        `json:"truncated"`
	NodeLimit       int         `json:"node_limit"`
	DepthLimit      int         `json:"depth_limit"`
	InputSHA256     []string    `json:"input_sha256,omitempty"`
	BinarySHA256    string      `json:"binary_sha256,omitempty"`
	AnalyzerVersion string      `json:"analyzer_version"`
	GeneratedAt     time.Time   `json:"generated_at"`
}

type FlameNode struct {
	Function string `json:"function"`
	Depth    int    `json:"depth"`
	X        int    `json:"x_permyriad"`
	Width    int    `json:"width_permyriad"`
	Value    int64  `json:"value"`
	Sign     string `json:"sign"`
}

type ProfileSummary struct {
	SampleType         string           `json:"sample_type"`
	Unit               string           `json:"unit"`
	NetTotal           int64            `json:"net_total"`
	PositiveTotal      int64            `json:"positive_total"`
	NegativeMagnitude  int64            `json:"negative_magnitude"`
	PercentDenominator int64            `json:"percent_denominator"`
	DenominatorMode    string           `json:"denominator_mode"`
	Reports            []ProfileReport  `json:"reports"`
	Labels             []LabelBreakdown `json:"labels,omitempty"`
}

type ProfileReport struct {
	Granularity           string        `json:"granularity"`
	TopFlat               []ProfileNode `json:"top_flat,omitempty"`
	TopCumulative         []ProfileNode `json:"top_cumulative,omitempty"`
	TopNegativeFlat       []ProfileNode `json:"top_negative_flat,omitempty"`
	TopNegativeCumulative []ProfileNode `json:"top_negative_cumulative,omitempty"`
}

type ProfileCoverage struct {
	RunSpanNs     int64  `json:"run_span_ns,omitempty"`
	CaptureSpanNs int64  `json:"capture_span_ns,omitempty"`
	HeadLossNs    int64  `json:"head_loss_ns,omitempty"`
	TailExcessNs  int64  `json:"tail_excess_ns,omitempty"`
	TailLossNs    int64  `json:"tail_loss_ns,omitempty"`
	StopReason    string `json:"stop_reason,omitempty"`
	Complete      bool   `json:"complete"`
}

type ProfileNode struct {
	Function string `json:"function,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int64  `json:"line,omitempty"`
	Value    int64  `json:"value"`
}

type LabelBreakdown struct {
	Key    string       `json:"key"`
	Values []LabelValue `json:"values"`
}

type LabelValue struct {
	Value string `json:"value"`
	Total int64  `json:"total"`
}

type Diagnostic struct {
	Level   string `json:"level"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
