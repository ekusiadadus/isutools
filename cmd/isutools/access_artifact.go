package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/ekusiadadus/isutools/internal/accessinspect"
	"github.com/ekusiadadus/isutools/internal/analysisartifact"
	"github.com/ekusiadadus/isutools/internal/safefs"
)

func accessLogCoverage(enabled bool, startDevice, startInode, startOffset uint64, startClock string, endDevice, endInode, endOffset uint64, endClock string) (analysisartifact.Coverage, error) {
	coverage := analysisartifact.Coverage{Complete: false, Clock: "proxy-log", Reason: "run-boundary-unavailable"}
	if !enabled {
		return coverage, nil
	}
	started, startErr := time.Parse(time.RFC3339Nano, startClock)
	ended, endErr := time.Parse(time.RFC3339Nano, endClock)
	if startErr != nil || endErr != nil || startInode == 0 || endInode == 0 {
		return coverage, errors.New("accessinspect: invalid coverage")
	}
	coverage = analysisartifact.Coverage{
		Complete: true, Clock: "proxy-log", StartedAt: started, EndedAt: ended,
		StartDevice: startDevice, StartInode: startInode, StartOffset: startOffset,
		EndDevice: endDevice, EndInode: endInode, EndOffset: endOffset,
	}
	switch {
	case startDevice != endDevice || startInode != endInode:
		coverage.Complete, coverage.Reason = false, "log-rotated"
	case endOffset < startOffset:
		coverage.Complete, coverage.Reason = false, "log-truncated"
	case ended.Before(started):
		coverage.Complete, coverage.Reason = false, "proxy-clock-backwards"
	}
	return coverage, nil
}

func publishAccesslogArtifact(dataDir, namespace, expected, runID, snapshotBase, snapshotSHA string, snapshotSchema int,
	inputHash []byte, inputBytes uint64, output []byte, report accessinspect.Report, coverage analysisartifact.Coverage, outputFormat string, maxInput, maxRecords int64, maxKeys int,
) (string, error) {
	if namespace == "" {
		namespace = runID
	}
	if namespace == "" || runID == "" || snapshotBase == "" || snapshotSHA == "" || snapshotSchema <= 0 || len(inputHash) != 32 {
		return "", errors.New("accessinspect: complete run binding is required")
	}
	root, err := safefs.Open(dataDir, safefs.Options{})
	if err != nil {
		return "", errors.New("accessinspect: artifact-directory-unavailable")
	}
	defer func() { _ = root.Close() }()
	binding := analysisartifact.RunBinding{RunID: runID, SnapshotBase: snapshotBase, SnapshotSHA256: snapshotSHA, SnapshotSchemaVersion: snapshotSchema}
	if err := analysisartifact.VerifyRunBinding(root, binding); err != nil {
		return "", err
	}
	extension, mediaType := accessOutputType(outputFormat)
	store := analysisartifact.NewStore(root)
	outputRef, err := store.PublishContent(namespace, analysisartifact.KindAccessLog, analysisartifact.Content{
		Role: "normalized-report", Extension: extension, MediaType: mediaType, Visibility: analysisartifact.VisibilityPortable,
		Body: output, MaxBytes: 64 << 20,
	})
	if err != nil {
		return "", errors.New("accessinspect: output-publication-failed")
	}
	if coverage.Complete && (coverage.EndOffset < coverage.StartOffset || coverage.EndOffset-coverage.StartOffset != inputBytes) {
		coverage.Complete, coverage.Reason = false, "input-span-mismatch"
	}
	status := analysisartifact.StatusReady
	diagnostics := []analysisartifact.Diagnostic{}
	if !coverage.Complete || report.Health.Malformed != 0 || report.Health.Partial != 0 {
		status = analysisartifact.StatusPartial
		if coverage.Reason == "" {
			coverage.Reason = "malformed-or-partial-input"
		}
		diagnostics = append(diagnostics, analysisartifact.Diagnostic{Level: analysisartifact.DiagnosticWarn, Code: "accesslog-partial", Message: "the access-log interval or one or more input lines are incomplete"})
	}
	extensionBody, _ := json.Marshal(map[string]any{
		"lines": report.Health.Lines, "parsed": report.Health.Parsed, "selected": report.Health.Filtered,
		"malformed": report.Health.Malformed, "partial": report.Health.Partial, "keys": len(report.Rows), "query_values_stripped": report.Health.QueryStripped,
	})
	manifest := analysisartifact.Manifest{
		Schema: analysisartifact.SchemaV1, Kind: analysisartifact.KindAccessLog, GeneratedAt: time.Now().UTC(),
		Analyzer: analysisartifact.Analyzer{Name: "isutools-accessinspect", Version: version}, Status: status, Run: &binding,
		Inputs:  []analysisartifact.FileRef{{Role: "proxy-access-log", Name: "proxy-access.log", SHA256: hex.EncodeToString(inputHash), Bytes: inputBytes, MediaType: "text/plain", Visibility: analysisartifact.VisibilityRestricted}},
		Outputs: []analysisartifact.FileRef{outputRef}, Coverage: coverage,
		Budget:      analysisartifact.ResourceBudget{MaxInputBytes: uint64(maxInput), MaxOutputBytes: 64 << 20, MaxMemoryBytes: uint64(maxInput) + uint64(maxRecords)*64 + uint64(maxKeys)*2048},
		Diagnostics: diagnostics, Extensions: map[string]json.RawMessage{"accessinspect-v1": extensionBody},
	}
	manifest, err = analysisartifact.SetArtifactID(manifest)
	if err != nil {
		return "", errors.New("accessinspect: manifest-build-failed")
	}
	published, err := store.Publish(namespace, manifest, expected)
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		return "", err
	}
	return published.ArtifactID, nil
}

func accessOutputType(format string) (string, string) {
	switch format {
	case "json":
		return "json", "application/json"
	case "markdown":
		return "md", "text/markdown"
	case "csv":
		return "csv", "text/csv"
	case "tsv":
		return "tsv", "text/tab-separated-values"
	default:
		return "txt", "text/plain"
	}
}
