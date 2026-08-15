package web

import (
	"encoding/json"
	"errors"
	"math/bits"
	"net/http"

	"github.com/ekusiadadus/isutools/internal/profilecapture"
	"github.com/ekusiadadus/isutools/internal/safefs"
)

const (
	profileAnalysisCapabilitiesSchema = "isutools.profile-analysis-capabilities/v1"
	profileSidecarMaxBytes            = uint64(profilecapture.MaxCompletionRecordBytes)
	cpuCoverageMaxBytes               = uint64(profilecapture.MaxCompletionRecordBytes)
	cpuLabelDictionaryMaxBytes        = uint64(128 << 10)
)

type ProfileAnalysisCapabilities struct {
	Schema                     string                   `json:"schema"`
	StrongAtomicVisibility     bool                     `json:"strong_atomic_visibility"`
	CrashDurability            string                   `json:"crash_durability"`
	CapabilityError            string                   `json:"capability_error,omitempty"`
	RetentionRuns              int                      `json:"retention_runs"`
	RetentionBytes             uint64                   `json:"retention_bytes"`
	ProfileCaptureMaxBytes     uint64                   `json:"profile_capture_max_bytes"`
	CaptureSidecarMaxBytes     uint64                   `json:"capture_sidecar_max_bytes"`
	CPUCoverageMaxBytes        uint64                   `json:"cpu_coverage_max_bytes"`
	CPULabelDictionaryMaxBytes uint64                   `json:"cpu_label_dictionary_max_bytes"`
	ExpectedProfileFilesPerRun uint64                   `json:"expected_profile_files_per_run"`
	SnapshotArtifactMaxPerRun  uint64                   `json:"snapshot_artifact_max_bytes_per_run"`
	PerRunCeilingBytes         uint64                   `json:"per_run_ceiling_bytes"`
	ProfileUsageKnown          bool                     `json:"profile_usage_known"`
	ProfileUsageBytes          uint64                   `json:"profile_usage_bytes,omitempty"`
	DataDirAvailableKnown      bool                     `json:"data_dir_available_known"`
	DataDirAvailableBytes      uint64                   `json:"data_dir_available_bytes,omitempty"`
	CurrentGeneration          int64                    `json:"current_generation"`
	EnabledProfileKinds        []string                 `json:"enabled_profile_kinds"`
	RuntimeProfileSemantics    []RuntimeProfileSemantic `json:"runtime_profile_semantics"`
}

func (h *handler) profileAnalysisCapabilities(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kinds := orderProfileKinds(h.p.RuntimeProfiles)
	expected := uint64(len(kinds) * 2)
	if h.p.CPUProfileMode == "run" {
		expected++
		kinds = append(kinds, "cpu")
	}
	capabilities := ProfileAnalysisCapabilities{
		Schema:        profileAnalysisCapabilitiesSchema,
		RetentionRuns: profileRetentionRuns, RetentionBytes: profileRetentionBytes,
		ProfileCaptureMaxBytes: runtimeProfileMaxBytes, CaptureSidecarMaxBytes: profileSidecarMaxBytes,
		CPUCoverageMaxBytes: cpuCoverageMaxBytes, CPULabelDictionaryMaxBytes: cpuLabelDictionaryMaxBytes,
		ExpectedProfileFilesPerRun: expected, SnapshotArtifactMaxPerRun: 2 * maxSnapshotBytes,
		CurrentGeneration: h.gen.Load(), EnabledProfileKinds: kinds,
		RuntimeProfileSemantics: RuntimeProfileSemantics(),
	}
	capabilities.PerRunCeilingBytes, _ = profileAnalysisPerRunCeiling(expected, h.p.CPUProfileMode == "run")
	if h.p.DataDir == "" {
		capabilities.CapabilityError = "data-dir-unavailable"
		writeCapabilities(writer, capabilities)
		return
	}
	root, err := safefs.Open(h.p.DataDir, safefs.Options{RequireStrongVisibility: true, Exclusive: false})
	if err != nil {
		capabilities.CapabilityError = "atomic-publication-unsupported"
		writeCapabilities(writer, capabilities)
		return
	}
	defer func() { _ = root.Close() }()
	capabilities.StrongAtomicVisibility = true
	capabilities.CrashDurability = string(root.PublicationDurability())
	if available, err := root.AvailableBytes(); err == nil {
		capabilities.DataDirAvailableKnown = true
		capabilities.DataDirAvailableBytes = available
	}
	if usage, err := rootRegularFileUsage(root); err == nil {
		capabilities.ProfileUsageKnown = true
		capabilities.ProfileUsageBytes = usage
	}
	writeCapabilities(writer, capabilities)
}

func writeCapabilities(writer http.ResponseWriter, capabilities ProfileAnalysisCapabilities) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(writer).Encode(capabilities)
}

func profileAnalysisPerRunCeiling(expectedFiles uint64, cpu bool) (uint64, error) {
	profileHigh, profiles := bits.Mul64(expectedFiles, runtimeProfileMaxBytes)
	sidecarHigh, sidecars := bits.Mul64(expectedFiles, profileSidecarMaxBytes)
	if profileHigh != 0 || sidecarHigh != 0 {
		return 0, errors.New("profile analysis ceiling overflow")
	}
	parts := []uint64{profiles, sidecars, 2 * maxSnapshotBytes}
	if cpu {
		parts = append(parts, cpuCoverageMaxBytes, cpuLabelDictionaryMaxBytes)
	}
	var total uint64
	for _, part := range parts {
		next, carry := bits.Add64(total, part, 0)
		if carry != 0 {
			return 0, errors.New("profile analysis ceiling overflow")
		}
		total = next
	}
	return total, nil
}

func rootRegularFileUsage(root *safefs.Root) (uint64, error) {
	entries, err := root.ReadDir()
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, entry := range entries {
		file, err := root.OpenRegular(entry.Name())
		if err != nil {
			continue
		}
		info, statErr := file.Stat()
		_ = file.Close()
		if statErr != nil || info.Size() < 0 {
			return 0, errors.New("profile analysis usage unavailable")
		}
		next, carry := bits.Add64(total, uint64(info.Size()), 0)
		if carry != 0 {
			return 0, errors.New("profile analysis usage overflow")
		}
		total = next
	}
	return total, nil
}
