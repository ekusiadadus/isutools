package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/internal/analysisartifact"
	"github.com/ekusiadadus/isutools/internal/safefs"
	"github.com/ekusiadadus/isutools/internal/slowlog"
)

func runMySQLSlowlog(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("analyze mysql-slowlog", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var (
		filePath       = flags.String("file", "", "input file; stdin when omitted")
		usePTQD        = flags.Bool("pt-query-digest", false, "run the pinned external analyzer")
		expectedPTQD   = flags.String("pt-query-digest-version", slowlog.DefaultPTQDVersion, "required pt-query-digest version")
		dataDir        = flags.String("data-dir", "", "artifact data directory")
		namespace      = flags.String("namespace", "", "artifact namespace")
		expected       = flags.String("expected-current", analysisartifact.NoCurrentArtifact, "expected current artifact id")
		runID          = flags.String("run-id", "", "source run id")
		snapshotBase   = flags.String("snapshot-base", "", "source snapshot basename")
		snapshotSHA    = flags.String("snapshot-sha256", "", "source snapshot sha256")
		snapshotSchema = flags.Int("snapshot-schema", 0, "source snapshot schema version")
		maxInput       = flags.Int64("max-input-bytes", slowlog.DefaultMaxInputBytes, "input byte limit")
		maxLine        = flags.Int("max-line-bytes", slowlog.DefaultMaxLineBytes, "single line byte limit")
		maxQuery       = flags.Int("max-query-bytes", slowlog.DefaultMaxQueryBytes, "single query event byte limit")
		maxEvents      = flags.Int64("max-events", slowlog.DefaultMaxEvents, "event limit")
		maxClasses     = flags.Int("max-classes", slowlog.DefaultMaxClasses, "query class limit")
		ptqdTimeout    = flags.Duration("pt-query-digest-timeout", slowlog.DefaultPTQDTimeout, "external analyzer wall timeout")
		ptqdOutput     = flags.Int64("pt-query-digest-max-output-bytes", slowlog.DefaultPTQDMaxOutput, "restricted report byte limit")
		ptqdMemory     = flags.Uint64("pt-query-digest-max-memory-bytes", slowlog.DefaultPTQDMaxMemory, "hard child address-space limit")
		coverageSet    = flags.Bool("coverage", false, "require explicit run boundary coverage metadata")
		startDevice    = flags.Uint64("start-device", 0, "capture-start log device")
		startInode     = flags.Uint64("start-inode", 0, "capture-start log inode")
		startOffset    = flags.Uint64("start-offset", 0, "capture-start byte offset")
		startDBClock   = flags.String("start-db-clock", "", "capture-start DB clock (RFC3339Nano)")
		endDevice      = flags.Uint64("end-device", 0, "capture-end log device")
		endInode       = flags.Uint64("end-inode", 0, "capture-end log inode")
		endOffset      = flags.Uint64("end-offset", 0, "capture-end byte offset")
		endDBClock     = flags.String("end-db-clock", "", "capture-end DB clock (RFC3339Nano)")
	)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprintln(stdout, "usage: isutools analyze mysql-slowlog [--file mysql-slow.log] [--pt-query-digest] [--data-dir DIR --run-id ID --snapshot-base BASE --snapshot-sha256 SHA]")
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "isutools: invalid-flags")
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "isutools: invalid-flags")
		return 2
	}
	if *maxInput < 1 || *maxInput > slowlog.HardMaxInputBytes {
		_, _ = fmt.Fprintln(stderr, "slowlog: invalid-input-limit")
		return 2
	}
	if *ptqdTimeout < time.Second || *ptqdTimeout > 10*time.Minute || *ptqdOutput < 1 || *ptqdOutput > 64<<20 || *ptqdMemory < 64<<20 || *ptqdMemory > slowlog.HardPTQDMaxMemory {
		_, _ = fmt.Fprintln(stderr, "slowlog: invalid-analyzer-budget")
		return 2
	}
	input := stdin
	var closer io.Closer
	if *filePath != "" && *filePath != "-" {
		file, err := openRegularNoFollow(*filePath)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "isutools: input-not-regular")
			return 1
		}
		input, closer = file, file
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}
	body, err := io.ReadAll(io.LimitReader(input, *maxInput+1))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "slowlog: input-read-failed")
		return 1
	}
	if int64(len(body)) > *maxInput {
		_, _ = fmt.Fprintln(stderr, "slowlog: input-too-large")
		return 1
	}
	coverage := slowlog.Coverage{Complete: false, Reason: "run-boundary-unavailable"}
	if *coverageSet {
		started, startErr := time.Parse(time.RFC3339Nano, *startDBClock)
		ended, endErr := time.Parse(time.RFC3339Nano, *endDBClock)
		if startErr != nil || endErr != nil || *startInode == 0 || *endInode == 0 {
			_, _ = fmt.Fprintln(stderr, "slowlog: invalid-coverage")
			return 2
		}
		coverage = slowlog.EvaluateCoverage(
			slowlog.CapturePoint{Identity: slowlog.FileIdentity{Device: *startDevice, Inode: *startInode}, Offset: *startOffset, DBClock: started},
			slowlog.CapturePoint{Identity: slowlog.FileIdentity{Device: *endDevice, Inode: *endInode}, Offset: *endOffset, DBClock: ended},
		)
		if coverage.Complete && (coverage.EndOffset < coverage.StartOffset || coverage.EndOffset-coverage.StartOffset != uint64(len(body))) {
			coverage.Complete = false
			coverage.Reason = "input-span-mismatch"
		}
	}
	report, err := slowlog.Parse(bytes.NewReader(body), slowlog.Options{
		MaxInputBytes: *maxInput, MaxLineBytes: *maxLine, MaxQueryBytes: *maxQuery,
		MaxEvents: *maxEvents, MaxClasses: *maxClasses, Coverage: coverage,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "slowlog: encode-failed")
		return 1
	}
	encoded = append(encoded, '\n')
	if _, err := stdout.Write(encoded); err != nil {
		_, _ = fmt.Fprintln(stderr, "slowlog: output-write-failed")
		return 1
	}
	var ptqd slowlog.PTQDResult
	if *usePTQD {
		ptqd = (slowlog.PTQD{ExpectedVersion: *expectedPTQD, Timeout: *ptqdTimeout, MaxOutputBytes: *ptqdOutput, MaxMemoryBytes: *ptqdMemory}).Run(ctx, bytes.NewReader(body))
	}
	if *dataDir != "" {
		artifactID, publishErr := publishSlowlogArtifact(*dataDir, *namespace, *expected, *runID, *snapshotBase, *snapshotSHA, *snapshotSchema, body, encoded, report, ptqd, *usePTQD, *maxInput, *ptqdTimeout, *ptqdOutput, *ptqdMemory)
		if publishErr != nil {
			_, _ = fmt.Fprintln(stderr, publishErr)
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "slowlog: artifact=%s events=%d classes=%d partial=%d\n", artifactID, report.Health.Events, report.Health.Classes, report.Health.PartialEvents)
		return 0
	}
	if *namespace != "" || *runID != "" || *snapshotBase != "" || *snapshotSHA != "" || *snapshotSchema != 0 {
		_, _ = fmt.Fprintln(stderr, "slowlog: data-dir-required-for-binding")
		return 2
	}
	if *usePTQD && ptqd.Status != slowlog.PTQDReady {
		_, _ = fmt.Fprintf(stderr, "slowlog: pt-query-digest=%s code=%s\n", ptqd.Status, ptqd.Code)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "slowlog: events=%d classes=%d partial=%d\n", report.Health.Events, report.Health.Classes, report.Health.PartialEvents)
	return 0
}

func publishSlowlogArtifact(dataDir, namespace, expected, runID, snapshotBase, snapshotSHA string, snapshotSchema int, input, summary []byte, report slowlog.Report, ptqd slowlog.PTQDResult, ptqdRequested bool, maxInput int64, ptqdTimeout time.Duration, ptqdMaxOutput int64, ptqdMaxMemory uint64) (string, error) {
	if namespace == "" {
		namespace = runID
	}
	if namespace == "" || runID == "" || snapshotBase == "" || snapshotSHA == "" || snapshotSchema <= 0 {
		return "", errors.New("slowlog: complete run binding is required")
	}
	root, err := safefs.Open(dataDir, safefs.Options{})
	if err != nil {
		return "", errors.New("slowlog: artifact-directory-unavailable")
	}
	defer func() { _ = root.Close() }()
	binding := analysisartifact.RunBinding{RunID: runID, SnapshotBase: snapshotBase, SnapshotSHA256: snapshotSHA, SnapshotSchemaVersion: snapshotSchema}
	if err := analysisartifact.VerifyRunBinding(root, binding); err != nil {
		return "", err
	}
	store := analysisartifact.NewStore(root)
	outputs := make([]analysisartifact.FileRef, 0, 2)
	summaryRef, err := store.PublishContent(namespace, analysisartifact.KindMySQLSlowLog, analysisartifact.Content{
		Role: "normalized-summary", Extension: "json", MediaType: "application/json", Visibility: analysisartifact.VisibilityPortable,
		Body: summary, MaxBytes: 16 << 20,
	})
	if err != nil {
		return "", errors.New("slowlog: summary-publication-failed")
	}
	outputs = append(outputs, summaryRef)
	if ptqd.Status == slowlog.PTQDReady && len(ptqd.Report) > 0 {
		ptqdRef, publishErr := store.PublishContent(namespace, analysisartifact.KindMySQLSlowLog, analysisartifact.Content{
			Role: "pt-query-digest-report", Extension: "txt", MediaType: "text/plain", Visibility: analysisartifact.VisibilityRestricted,
			Body: ptqd.Report, MaxBytes: ptqdMaxOutput,
		})
		if publishErr != nil {
			return "", errors.New("slowlog: restricted-report-publication-failed")
		}
		outputs = append(outputs, ptqdRef)
	}
	inputHash := sha256.Sum256(input)
	status := analysisartifact.StatusReady
	diagnostics := []analysisartifact.Diagnostic{}
	if !report.Coverage.Complete || report.Health.PartialEvents > 0 {
		status = analysisartifact.StatusPartial
		diagnostics = append(diagnostics, analysisartifact.Diagnostic{Level: analysisartifact.DiagnosticWarn, Code: "slowlog-partial", Message: "slow-log coverage contains an incomplete event or interval"})
	}
	if ptqdRequested && ptqd.Status != slowlog.PTQDReady {
		status = analysisartifact.StatusPartial
		diagnostics = append(diagnostics, analysisartifact.Diagnostic{Level: analysisartifact.DiagnosticWarn, Code: ptqd.Code, Message: "pt-query-digest did not produce a usable restricted report"})
	}
	extension, _ := json.Marshal(map[string]any{
		"events": report.Health.Events, "classes": report.Health.Classes, "partial_events": report.Health.PartialEvents,
		"pt_query_digest_requested": ptqdRequested, "pt_query_digest_status": ptqd.Status, "pt_query_digest_version": boundedVersion(ptqd.Version),
	})
	manifest := analysisartifact.Manifest{
		Schema: analysisartifact.SchemaV1, Kind: analysisartifact.KindMySQLSlowLog, GeneratedAt: time.Now().UTC(),
		Analyzer: analysisartifact.Analyzer{Name: "isutools-slowlog", Version: version}, Status: status,
		Run:     &binding,
		Inputs:  []analysisartifact.FileRef{{Role: "mysql-slowlog", Name: "mysql-slow.log", SHA256: hex.EncodeToString(inputHash[:]), Bytes: uint64(len(input)), MediaType: "text/plain", Visibility: analysisartifact.VisibilityRestricted}},
		Outputs: outputs,
		Coverage: analysisartifact.Coverage{
			Complete: report.Coverage.Complete, Clock: "mysql-slowlog", StartedAt: report.Coverage.DBStartedAt, EndedAt: report.Coverage.DBEndedAt,
			StartDevice: report.Coverage.Identity.Device, StartInode: report.Coverage.Identity.Inode, StartOffset: report.Coverage.StartOffset,
			EndDevice: report.Coverage.Identity.Device, EndInode: report.Coverage.Identity.Inode, EndOffset: report.Coverage.EndOffset, Reason: report.Coverage.Reason,
		},
		Budget:      analysisartifact.ResourceBudget{TimeoutMS: uint64(ptqdTimeout / time.Millisecond), MaxInputBytes: uint64(maxInput), MaxOutputBytes: uint64(ptqdMaxOutput), MaxMemoryBytes: ptqdMaxMemory},
		Diagnostics: diagnostics, Extensions: map[string]json.RawMessage{"mysql-slowlog-v1": extension},
	}
	manifest, err = analysisartifact.SetArtifactID(manifest)
	if err != nil {
		return "", errors.New("slowlog: manifest-build-failed")
	}
	published, err := store.Publish(namespace, manifest, expected)
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		return "", err
	}
	return published.ArtifactID, nil
}

func boundedVersion(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		return ""
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}
