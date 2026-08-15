package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ekusiadadus/isutools/buildinfo"
	"github.com/ekusiadadus/isutools/internal/pprofanalyze"
	"github.com/ekusiadadus/isutools/internal/profilecapture"
	"github.com/ekusiadadus/isutools/internal/profilehandoff"
	"github.com/ekusiadadus/isutools/internal/profilemodel"
	"github.com/ekusiadadus/isutools/internal/safefs"
	"github.com/ekusiadadus/isutools/web"
)

const (
	bundleSchema             = "isutools.pprof-bundle/v2"
	legacyBundleSchema       = "isutools.pprof-bundle/v1"
	bundleMarkerSchema       = "isutools.pprof-bundle-current/v1"
	capabilitiesSchema       = "isutools.profile-analysis-capabilities/v1"
	cliVersion               = "1"
	pprofDependencyVersion   = "v0.0.0-20260709232956-b9395ee17fa0"
	maxHTTPMetadataBytes     = int64(256 << 10)
	maxFetchedProfileBytes   = int64(32 << 20)
	maxFetchedTraceBytes     = int64(256 << 20)
	defaultCommandTimeout    = 60 * time.Second
	minimumCommandTimeout    = time.Second
	maximumCommandTimeout    = 5 * time.Minute
	minimumPreflightHeadroom = uint64(64 << 20)
)

type fetchedArtifact struct {
	SHA256 string
	Limit  int64
}

var commandHTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
var launchAnalysisWorker = pprofanalyze.LaunchWorker

type bundleAttempt struct {
	Kind       string                              `json:"kind"`
	Mode       string                              `json:"mode"`
	Expected   []profilemodel.ExpectedProfileInput `json:"expected"`
	Observed   []profilemodel.ObservedProfileInput `json:"observed,omitempty"`
	Coverage   profilemodel.ProfileCoverage        `json:"coverage"`
	Dictionary *profilecapture.LabelDictionary     `json:"dictionary,omitempty"`
}

type bundleTrace struct {
	ExpectedFile  string `json:"expected_file"`
	File          string `json:"file,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Bytes         int64  `json:"bytes,omitempty"`
	Sidecar       string `json:"sidecar,omitempty"`
	SidecarSHA256 string `json:"sidecar_sha256,omitempty"`
	Status        string `json:"status"`
	Code          string `json:"code,omitempty"`
	Complete      bool   `json:"complete"`
}

type profileBundle struct {
	Schema                string                          `json:"schema"`
	BundleID              string                          `json:"bundle_id"`
	SnapshotBase          string                          `json:"snapshot_base"`
	SnapshotFile          string                          `json:"snapshot_file"`
	SnapshotSHA256        string                          `json:"snapshot_sha256"`
	SnapshotSchemaVersion int                             `json:"snapshot_schema_version"`
	RunID                 string                          `json:"run_id"`
	CapturedExecutable    profilemodel.ExecutableIdentity `json:"captured_executable"`
	Attempts              []bundleAttempt                 `json:"attempts,omitempty"`
	Trace                 *bundleTrace                    `json:"trace,omitempty"`
	CommandSchema         string                          `json:"command_schema,omitempty"`
	TestedGoVersion       string                          `json:"tested_go_version,omitempty"`
}

type bundleMarker struct {
	Schema         string `json:"schema"`
	BundleID       string `json:"bundle_id"`
	ManifestFile   string `json:"manifest_file"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

func main() {
	if pprofanalyze.IsWorkerInvocation(os.Args) {
		os.Exit(pprofanalyze.RunInheritedWorker())
	}
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: isutools-pprof <preflight|fetch|analyze|recipes|publish> [flags]")
		return 2
	}
	var err error
	switch args[0] {
	case "preflight":
		err = runPreflight(args[1:], stdout, stderr)
	case "fetch":
		err = runFetch(args[1:], stdout, stderr)
	case "analyze":
		var usable bool
		usable, err = runAnalyze(args[1:], stdout, stderr)
		if err == nil && !usable {
			return 4
		}
	case "recipes":
		err = runRecipes(args[1:], stdout, stderr)
	case "publish":
		err = runPublish(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		return 2
	}
	if err == nil {
		return 0
	}
	var usage *usageError
	if errors.As(err, &usage) {
		_, _ = fmt.Fprintln(stderr, usage.Error())
		return 2
	}
	_, _ = fmt.Fprintln(stderr, err)
	if args[0] == "publish" {
		return 5
	}
	if args[0] == "analyze" {
		return 4
	}
	return 3
}

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(stderr)
	return set
}

func parseFlags(set *flag.FlagSet, args []string) error {
	if err := set.Parse(args); err != nil {
		return &usageError{message: err.Error()}
	}
	if set.NArg() != 0 {
		return &usageError{message: "unexpected positional arguments"}
	}
	return nil
}

func runPreflight(args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("preflight", stderr)
	admin := set.String("admin", "", "admin origin")
	blockRuns := set.Uint64("block-runs", 0, "number of runs in the block")
	timeout := set.Duration("timeout", defaultCommandTimeout, "request timeout")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if *admin == "" || *blockRuns == 0 || !validTimeout(*timeout) {
		return &usageError{message: "preflight requires --admin, --block-runs > 0, and timeout in 1s..5m"}
	}
	endpoint, err := adminEndpoint(*admin, "/profile-analysis-capabilities", nil)
	if err != nil {
		return &usageError{message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	body, status, err := httpRequest(ctx, http.MethodGet, endpoint, "", nil, maxHTTPMetadataBytes)
	if err != nil {
		return fmt.Errorf("preflight request: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("preflight endpoint returned HTTP %d", status)
	}
	var capabilities web.ProfileAnalysisCapabilities
	if err := decodeStrict(body, &capabilities); err != nil || capabilities.Schema != capabilitiesSchema {
		return fmt.Errorf("invalid capabilities response: %v", err)
	}
	base, overflow := checkedMultiply(*blockRuns, capabilities.PerRunCeilingBytes)
	if overflow {
		return errors.New("preflight byte calculation overflow")
	}
	tenPercent := base / 10
	if base%10 != 0 {
		tenPercent++
	}
	margin := minimumPreflightHeadroom
	if tenPercent > margin {
		margin = tenPercent
	}
	required, carry := bits.Add64(base, margin, 0)
	if carry != 0 {
		return errors.New("preflight byte calculation overflow")
	}
	var failures []string
	if capabilities.CapabilityError != "" || !capabilities.StrongAtomicVisibility {
		failures = append(failures, "strong atomic publication unavailable")
	}
	if capabilities.ExpectedProfileFilesPerRun == 0 || capabilities.PerRunCeilingBytes == 0 {
		failures = append(failures, "no bounded profile set is enabled")
	}
	if uint64(capabilities.RetentionRuns) < *blockRuns {
		failures = append(failures, "retention run count is insufficient")
	}
	if capabilities.RetentionBytes < base {
		failures = append(failures, "retention byte ceiling is insufficient")
	}
	if !capabilities.ProfileUsageKnown {
		failures = append(failures, "current profile usage is unknown")
	}
	if !capabilities.DataDirAvailableKnown || capabilities.DataDirAvailableBytes < required {
		failures = append(failures, "DataDir free space is unknown or insufficient")
	}
	if len(failures) != 0 {
		return fmt.Errorf("preflight refused: %s", strings.Join(failures, "; "))
	}
	if _, err := fmt.Fprintf(stdout, "preflight ok: runs=%d required_bytes=%d generation=%d\n", *blockRuns, required, capabilities.CurrentGeneration); err != nil {
		return fmt.Errorf("write preflight result: %w", err)
	}
	return nil
}

func runFetch(args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("fetch", stderr)
	admin := set.String("admin", "", "admin origin")
	dataDir := set.String("data-dir", "", "mounted DataDir")
	base := set.String("snapshot-base", "", "exact snapshot base")
	pinnedHash := set.String("snapshot-sha256", "", "SHA-256 returned by /save")
	bundleDir := set.String("bundle-dir", "", "local bundle directory")
	timeout := set.Duration("timeout", defaultCommandTimeout, "request timeout")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if (*admin == "") == (*dataDir == "") || !validBase(*base) || !validHash(*pinnedHash) || *bundleDir == "" || !validTimeout(*timeout) {
		return &usageError{message: "fetch requires exactly one of --admin/--data-dir, exact --snapshot-base, --snapshot-sha256, --bundle-dir, and timeout in 1s..5m"}
	}
	if err := os.MkdirAll(*bundleDir, 0o700); err != nil {
		return fmt.Errorf("create bundle directory: %w", err)
	}
	if err := os.Chmod(*bundleDir, 0o700); err != nil {
		return fmt.Errorf("protect bundle directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	readSource, closeSource, err := makeFetchSource(ctx, *admin, *dataDir)
	if err != nil {
		return err
	}
	defer closeSource()
	snapshotName := *base + ".json"
	snapshotBody, err := readSource(snapshotName, int64(32<<20))
	if err != nil {
		return fmt.Errorf("fetch snapshot: %w", err)
	}
	if hashBytes(snapshotBody) != *pinnedHash {
		return errors.New("fetched snapshot does not match ledger SHA-256")
	}
	// Saved run JSON embeds Snapshot at the top level and may carry one
	// comparison predecessor. Keep this decoder structurally identical to
	// web.jsonPayload instead of inventing a {"snapshot": ...} wire wrapper.
	var persisted struct {
		web.Snapshot
		Prev json.RawMessage `json:"prev,omitempty"`
	}
	if err := decodeStrict(snapshotBody, &persisted); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	manifest := persisted.Meta.Profiles
	if manifest == nil || manifest.RunID == "" || manifest.Executable == nil {
		return errors.New("snapshot has no profile manifest or capture-time executable identity")
	}
	bundle, files, err := bundleFromManifest(*base, *pinnedHash, persisted.Meta.SchemaVersion, manifest)
	if err != nil {
		return err
	}
	bundleRoot, err := safefs.Open(*bundleDir, safefs.Options{RequireStrongVisibility: true, Exclusive: true})
	if err != nil {
		return fmt.Errorf("open bundle directory: %w", err)
	}
	defer func() { _ = bundleRoot.Close() }()
	if err := publishBundleFile(bundleRoot, snapshotName, snapshotBody); err != nil {
		return err
	}
	for name, artifact := range files {
		body, err := readSource(name, artifact.Limit)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", name, err)
		}
		if hashBytes(body) != artifact.SHA256 {
			return fmt.Errorf("fetched artifact %s does not match snapshot SHA-256", name)
		}
		if err := publishBundleFile(bundleRoot, name, body); err != nil {
			return err
		}
	}
	manifestName, err := publishBundleManifest(bundleRoot, &bundle)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "fetch ok: bundle=%s manifest=%s profiles=%d\n", bundle.BundleID, manifestName, len(files)); err != nil {
		return fmt.Errorf("write fetch result: %w", err)
	}
	return nil
}

func makeFetchSource(ctx context.Context, admin, dataDir string) (func(string, int64) ([]byte, error), func(), error) {
	if admin != "" {
		origin, err := validateAdminOrigin(admin)
		if err != nil {
			return nil, nil, err
		}
		return func(name string, limit int64) ([]byte, error) {
			endpoint := *origin
			endpoint.Path = "/files/" + url.PathEscape(name)
			body, status, err := httpRequest(ctx, http.MethodGet, endpoint.String(), "", nil, limit)
			if err != nil {
				return nil, err
			}
			if status != http.StatusOK {
				return nil, fmt.Errorf("HTTP %d", status)
			}
			return body, nil
		}, func() {}, nil
	}
	root, err := safefs.Open(dataDir, safefs.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("open source DataDir: %w", err)
	}
	return func(name string, limit int64) ([]byte, error) { return root.ReadFile(name, limit) }, func() { _ = root.Close() }, nil
}

func bundleFromManifest(base, snapshotHash string, snapshotSchema int, manifest *web.ProfileManifest) (profileBundle, map[string]fetchedArtifact, error) {
	expectations := append([]web.ProfileExpectation(nil), manifest.Expected...)
	if len(expectations) == 0 {
		for _, pair := range manifest.Pairs {
			expectations = append(expectations, web.ProfileExpectation{Kind: pair.Kind, Mode: profilemodel.ProfileModeCumulativeDelta, Inputs: []web.ProfileExpectedInput{
				{Kind: pair.Kind, Point: "open", File: pair.OpenFile}, {Kind: pair.Kind, Point: "close", File: pair.CloseFile},
			}})
		}
		if manifest.CPU != nil && manifest.CPU.ExpectedFile != "" {
			expectations = append(expectations, web.ProfileExpectation{Kind: "cpu", Mode: profilemodel.ProfileModeInterval, Inputs: []web.ProfileExpectedInput{{Kind: "cpu", Point: "interval", File: manifest.CPU.ExpectedFile}}})
		}
	}
	if len(expectations) > profilemodel.MaxAttempts {
		return profileBundle{}, nil, errors.New("snapshot has too many profile expectations")
	}
	captures := make(map[string]web.ProfileCapture)
	for _, capture := range manifest.Captures {
		if capture.Status == "ok" && capture.File != "" && validHash(capture.SHA256) {
			captures[capture.File] = capture
		}
	}
	pairs := make(map[string]web.ProfilePair)
	for _, pair := range manifest.Pairs {
		pairs[pair.Kind] = pair
	}
	files := make(map[string]fetchedArtifact)
	bundle := profileBundle{
		Schema: bundleSchema, SnapshotBase: base, SnapshotFile: base + ".json", SnapshotSHA256: snapshotHash,
		SnapshotSchemaVersion: snapshotSchema, RunID: manifest.RunID, CapturedExecutable: modelExecutable(*manifest.Executable),
		CommandSchema: profilehandoff.SchemaV1, TestedGoVersion: runtime.Version(),
	}
	seenKinds := make(map[string]struct{}, len(expectations))
	for _, expectation := range expectations {
		if expectation.Kind == "" || len(expectation.Inputs) == 0 {
			return profileBundle{}, nil, errors.New("malformed profile expectation")
		}
		if _, duplicate := seenKinds[expectation.Kind]; duplicate {
			return profileBundle{}, nil, errors.New("duplicate profile expectation")
		}
		seenKinds[expectation.Kind] = struct{}{}
		attempt := bundleAttempt{Kind: expectation.Kind, Mode: expectation.Mode}
		for _, input := range expectation.Inputs {
			if input.Kind != expectation.Kind || !validArtifactName(input.File) {
				return profileBundle{}, nil, errors.New("unsafe expected profile input")
			}
			attempt.Expected = append(attempt.Expected, profilemodel.ExpectedProfileInput{Kind: input.Kind, Point: input.Point, File: input.File})
			if capture, ok := captures[input.File]; ok {
				attempt.Observed = append(attempt.Observed, profilemodel.ObservedProfileInput{
					ExpectedFile: input.File, File: input.File, SHA256: capture.SHA256, Bytes: capture.Bytes, Symbolized: true,
				})
				if err := addFetchedArtifact(files, input.File, capture.SHA256, maxFetchedProfileBytes); err != nil {
					return profileBundle{}, nil, err
				}
			}
		}
		if pair, ok := pairs[expectation.Kind]; ok {
			attempt.Coverage = profilemodel.ProfileCoverage{
				RunSpanNs: pair.RunSpanNs, HeadLossNs: pair.HeadLossNs, TailExcessNs: pair.TailExcessNs,
				Complete: len(attempt.Observed) == len(attempt.Expected),
			}
		}
		if expectation.Kind == "cpu" && manifest.CPU != nil {
			cpu := manifest.CPU
			attempt.Coverage = profilemodel.ProfileCoverage{
				RunSpanNs: cpu.RunSpanNs, CaptureSpanNs: cpu.CaptureSpanNs, HeadLossNs: cpu.HeadLossNs,
				TailExcessNs: cpu.TailExcessNs, TailLossNs: cpu.TailLossNs, StopReason: cpu.StopReason, Complete: cpu.Complete,
			}
			if cpu.File != "" && cpu.File == cpu.ExpectedFile && validHash(cpu.SHA256) && cpu.Bytes >= 0 {
				observed := profilemodel.ObservedProfileInput{ExpectedFile: cpu.ExpectedFile, File: cpu.File, SHA256: cpu.SHA256, Bytes: cpu.Bytes, Symbolized: true}
				if err := addFetchedArtifact(files, cpu.File, cpu.SHA256, maxFetchedProfileBytes); err != nil {
					return profileBundle{}, nil, err
				}
				if cpu.Sidecar != "" || cpu.SidecarSHA256 != "" {
					if !validAttachmentName(cpu.Sidecar) || !validHash(cpu.SidecarSHA256) {
						return profileBundle{}, nil, errors.New("malformed CPU completion sidecar identity")
					}
					observed.Sidecar, observed.SidecarSHA256 = cpu.Sidecar, cpu.SidecarSHA256
					if err := addFetchedArtifact(files, cpu.Sidecar, cpu.SidecarSHA256, profilecapture.MaxCompletionRecordBytes); err != nil {
						return profileBundle{}, nil, err
					}
				}
				if cpu.CoverageFile != "" || cpu.CoverageSHA256 != "" {
					if !validAttachmentName(cpu.CoverageFile) || !validHash(cpu.CoverageSHA256) {
						return profileBundle{}, nil, errors.New("malformed CPU coverage identity")
					}
					observed.CoverageFile, observed.CoverageSHA256 = cpu.CoverageFile, cpu.CoverageSHA256
					if err := addFetchedArtifact(files, cpu.CoverageFile, cpu.CoverageSHA256, profilecapture.MaxCompletionRecordBytes); err != nil {
						return profileBundle{}, nil, err
					}
				}
				attempt.Observed = []profilemodel.ObservedProfileInput{observed}
			}
			if dictionary := modelDictionary(manifest.CPULabelDictionary); dictionary != nil {
				attempt.Dictionary = dictionary
			}
		}
		bundle.Attempts = append(bundle.Attempts, attempt)
	}
	sort.Slice(bundle.Attempts, func(i, j int) bool { return bundle.Attempts[i].Kind < bundle.Attempts[j].Kind })
	if trace := manifest.Trace; trace != nil {
		if !validTraceName(trace.ExpectedFile) {
			return profileBundle{}, nil, errors.New("unsafe expected trace input")
		}
		bundle.Trace = &bundleTrace{ExpectedFile: trace.ExpectedFile, Status: trace.Status, Code: trace.Code, Complete: trace.Complete}
		if trace.File != "" || trace.SHA256 != "" || trace.Sidecar != "" || trace.SidecarSHA256 != "" {
			if trace.File != trace.ExpectedFile || !validTraceName(trace.File) || !validHash(trace.SHA256) || trace.Bytes < 1 ||
				!validAttachmentName(trace.Sidecar) || !validHash(trace.SidecarSHA256) {
				return profileBundle{}, nil, errors.New("malformed trace artifact identity")
			}
			bundle.Trace.File, bundle.Trace.SHA256, bundle.Trace.Bytes = trace.File, trace.SHA256, trace.Bytes
			bundle.Trace.Sidecar, bundle.Trace.SidecarSHA256 = trace.Sidecar, trace.SidecarSHA256
			if err := addFetchedArtifact(files, trace.File, trace.SHA256, maxFetchedTraceBytes); err != nil {
				return profileBundle{}, nil, err
			}
			if err := addFetchedArtifact(files, trace.Sidecar, trace.SidecarSHA256, profilecapture.MaxCompletionRecordBytes); err != nil {
				return profileBundle{}, nil, err
			}
		}
	}
	if len(bundle.Attempts) == 0 && bundle.Trace == nil {
		return profileBundle{}, nil, errors.New("snapshot has no bounded profile or trace expectations")
	}
	return bundle, files, nil
}

func addFetchedArtifact(files map[string]fetchedArtifact, name, sha256 string, limit int64) error {
	if (!validArtifactName(name) && !validAttachmentName(name) && !validTraceName(name)) || !validHash(sha256) || limit <= 0 {
		return errors.New("invalid fetched artifact identity")
	}
	value := fetchedArtifact{SHA256: sha256, Limit: limit}
	if previous, exists := files[name]; exists && previous != value {
		return errors.New("conflicting fetched artifact identity")
	}
	files[name] = value
	return nil
}

func publishBundleManifest(root *safefs.Root, bundle *profileBundle) (string, error) {
	bundle.BundleID = ""
	body, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	bundle.BundleID = hashBytes(body)
	body, err = json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	body = append(body, '\n')
	name := "bundle." + bundle.BundleID + ".json"
	if err := publishBundleFile(root, name, body); err != nil {
		return "", err
	}
	markerBody, _ := json.Marshal(bundleMarker{Schema: bundleMarkerSchema, BundleID: bundle.BundleID, ManifestFile: name, ManifestSHA256: hashBytes(body)})
	markerBody = append(markerBody, '\n')
	if err := replaceBundleFile(root, "bundle.current.json", markerBody); err != nil {
		return "", err
	}
	return name, nil
}

func runAnalyze(args []string, stdout, stderr io.Writer) (bool, error) {
	set := newFlagSet("analyze", stderr)
	bundleDir := set.String("bundle-dir", "", "bundle directory")
	binaryPath := set.String("binary", "", "exact target binary")
	sourceRoot := set.String("source-root", "", "optional source display root")
	top := set.Int("top", 20, "top nodes per report")
	output := set.String("output", "-", "analysis JSON path or -")
	timeout := set.Duration("timeout", defaultCommandTimeout, "worker timeout")
	if err := parseFlags(set, args); err != nil {
		return false, err
	}
	if *bundleDir == "" || *top < 1 || *top > profilemodel.MaxTopNodes || *output == "" || !validTimeout(*timeout) {
		return false, &usageError{message: "analyze requires --bundle-dir, --top 1..50, --output, and timeout in 1s..5m"}
	}
	if *sourceRoot != "" {
		info, err := os.Stat(*sourceRoot)
		if err != nil || !info.IsDir() {
			return false, errors.New("source root is not a directory")
		}
		absolute, err := filepath.Abs(*sourceRoot)
		if err != nil {
			return false, errors.New("source root cannot be resolved")
		}
		*sourceRoot = filepath.Clean(absolute)
	}
	root, err := safefs.Open(*bundleDir, safefs.Options{})
	if err != nil {
		return false, fmt.Errorf("open bundle: %w", err)
	}
	defer func() { _ = root.Close() }()
	bundle, err := loadBundle(root)
	if err != nil {
		return false, err
	}
	analysis, usable, err := analyzeBundle(root, bundle, *binaryPath, *sourceRoot, *top, *timeout)
	if err != nil {
		return false, err
	}
	body, err := profilemodel.CanonicalJSON(analysis)
	if err != nil {
		return false, err
	}
	body = append(body, '\n')
	if *output == "-" {
		if _, err := stdout.Write(body); err != nil {
			return false, err
		}
		if _, err := fmt.Fprintf(stderr, "analysis %s status=%s attempts=%d\n", analysis.AnalysisID, analysis.Status, len(analysis.Attempts)); err != nil {
			return false, fmt.Errorf("write analysis status: %w", err)
		}
	} else {
		if err := writePrivateFile(*output, body); err != nil {
			return false, err
		}
		if _, err := fmt.Fprintf(stdout, "analysis %s status=%s output=%s\n", analysis.AnalysisID, analysis.Status, *output); err != nil {
			return false, fmt.Errorf("write analysis result: %w", err)
		}
	}
	return usable, nil
}

func analyzeBundle(root *safefs.Root, bundle profileBundle, binaryPath, sourceRoot string, top int, timeout time.Duration) (profilemodel.ProfileAnalysisV1, bool, error) {
	analyzerIdentity, _ := buildinfo.CaptureExecutable()
	analysis := profilemodel.ProfileAnalysisV1{
		SchemaVersion: profilemodel.SchemaVersionV1, SnapshotBase: bundle.SnapshotBase, SnapshotSHA256: bundle.SnapshotSHA256,
		SnapshotSchemaVersion: bundle.SnapshotSchemaVersion, RunID: bundle.RunID, GeneratedAt: time.Now().UTC(),
		Analyzer: profilemodel.AnalyzerProvenance{Version: cliVersion, Revision: analyzerIdentity.VCSRevision, Dirty: analyzerIdentity.VCSModified, PprofVersion: pprofDependencyVersion, Executable: modelExecutable(analyzerIdentity)},
		Binary:   profilemodel.BinaryProvenance{Captured: bundle.CapturedExecutable, Match: profilemodel.BinaryMatchUnknown},
	}
	if binaryPath != "" {
		identity, err := buildinfo.CaptureInputFile(binaryPath)
		if err != nil {
			return profilemodel.ProfileAnalysisV1{}, false, fmt.Errorf("capture input binary: %w", err)
		}
		converted := modelExecutable(identity)
		analysis.Binary.Analyzed = &converted
		if bundle.CapturedExecutable.SHA256 != converted.SHA256 {
			analysis.Binary.Match = profilemodel.BinaryMatchMismatch
			analysis.Diagnostics = append(analysis.Diagnostics, diagnostic(profilemodel.DiagnosticWarn, profilemodel.DiagnosticBinaryMismatch, "analyzed binary SHA-256 differs from the capture-time running image"))
		} else if bundle.CapturedExecutable.Status == profilemodel.ExecutableStatusCaptured &&
			(bundle.CapturedExecutable.Source == profilemodel.ExecutableSourceProcSelfExe || bundle.CapturedExecutable.Source == profilemodel.ExecutableSourcePlatformBound) {
			analysis.Binary.Match = profilemodel.BinaryMatchVerified
		} else {
			analysis.Diagnostics = append(analysis.Diagnostics, diagnostic(profilemodel.DiagnosticWarn, profilemodel.DiagnosticProvenanceUnavailable, "capture-time executable identity is not bound to the running image"))
		}
	} else {
		analysis.Diagnostics = append(analysis.Diagnostics, diagnostic(profilemodel.DiagnosticWarn, profilemodel.DiagnosticProvenanceUnavailable, "no analyzed binary was supplied"))
	}
	complete := make([]int, 0, len(bundle.Attempts))
	analysis.Attempts = make([]profilemodel.ProfileAttempt, len(bundle.Attempts))
	for index, bundled := range bundle.Attempts {
		attempt := profilemodel.ProfileAttempt{Kind: bundled.Kind, Mode: bundled.Mode, ExpectedInputs: bundled.Expected, Coverage: bundled.Coverage}
		attempt.Flame = unavailableFlame(bundled.Mode, "profile-missing", analysis.GeneratedAt)
		if len(bundled.Observed) != len(bundled.Expected) {
			attempt.Status = profilemodel.AnalysisStatusFailed
			attempt.Diagnostics = []profilemodel.Diagnostic{diagnostic(profilemodel.DiagnosticError, profilemodel.DiagnosticProfileMissing, "one or more expected profile inputs were not captured")}
		} else {
			attempt.ObservedInputs = append([]profilemodel.ObservedProfileInput(nil), bundled.Observed...)
			complete = append(complete, index)
		}
		analysis.Attempts[index] = attempt
	}
	if len(complete) == 0 {
		analysis.Analyzer.Isolation = profilemodel.WorkerIsolation{Mode: profilemodel.IsolationNotRequired, Bootstrap: profilemodel.BootstrapNotRequired}
		analysis.Status = profilemodel.RecomputeStatus(analysis)
		signed, err := profilemodel.SetAnalysisID(analysis)
		if err == nil {
			err = profilemodel.Validate(signed)
		}
		return signed, false, err
	}
	executable, err := os.Executable()
	if err != nil {
		return profilemodel.ProfileAnalysisV1{}, false, err
	}
	workerOptions := pprofanalyze.WorkerOptions{CgroupRoot: os.Getenv("ISUTOOLS_PPROF_CGROUP_ROOT"), Timeout: timeout}
	for _, index := range complete {
		bundled := bundle.Attempts[index]
		files := make([]*os.File, 0, len(bundled.Observed))
		for _, observed := range bundled.Observed {
			file, openErr := root.OpenRegular(observed.File)
			if openErr != nil {
				for _, opened := range files {
					_ = opened.Close()
				}
				return profilemodel.ProfileAnalysisV1{}, false, openErr
			}
			files = append(files, file)
		}
		result, workerErr := launchAnalysisWorker(context.Background(), executable, pprofanalyze.WorkerJob{Mode: bundled.Mode, TopN: top, Dictionary: bundled.Dictionary, Profiles: files}, workerOptions)
		for _, file := range files {
			_ = file.Close()
		}
		if workerErr != nil {
			if errors.Is(workerErr, pprofanalyze.ErrHardIsolationUnavailable) {
				for attemptIndex := range analysis.Attempts {
					analysis.Attempts[attemptIndex].ObservedInputs = nil
					analysis.Attempts[attemptIndex].Summaries = nil
					analysis.Attempts[attemptIndex].Status = profilemodel.AnalysisStatusFailed
					analysis.Attempts[attemptIndex].Diagnostics = []profilemodel.Diagnostic{diagnostic(profilemodel.DiagnosticError, profilemodel.DiagnosticWorkerHardLimitUnavailable, "hard worker isolation could not be established before reading profile data")}
				}
				analysis.Analyzer.Isolation = profilemodel.WorkerIsolation{Mode: profilemodel.IsolationUnavailable, Bootstrap: profilemodel.BootstrapUnavailable}
				analysis.Status = profilemodel.AnalysisStatusFailed
				signed, signErr := profilemodel.SetAnalysisID(analysis)
				if signErr == nil {
					signErr = profilemodel.Validate(signed)
				}
				return signed, false, signErr
			}
			return profilemodel.ProfileAnalysisV1{}, false, workerErr
		}
		if index == complete[0] {
			analysis.Analyzer.Isolation = result.Isolation
		} else if analysis.Analyzer.Isolation != result.Isolation {
			return profilemodel.ProfileAnalysisV1{}, false, errors.New("worker isolation changed between attempts")
		}
		attempt := &analysis.Attempts[index]
		attempt.Summaries = result.Summaries
		attempt.Flame = flameForAttempt(result.Flame, bundled, analysis)
		sourceRedacted := sanitizeSourcePaths(attempt.Summaries, sourceRoot)
		if sourceRedacted {
			attempt.Diagnostics = append(attempt.Diagnostics, diagnostic(profilemodel.DiagnosticWarn, profilemodel.DiagnosticSourcePathRedacted, "one or more source paths were outside the selected source root and were redacted"))
		}
		if result.ErrorCode != "" {
			attempt.Status = profilemodel.AnalysisStatusFailed
			attempt.Diagnostics = append(attempt.Diagnostics, diagnostic(profilemodel.DiagnosticError, result.ErrorCode, result.ErrorMessage))
		} else {
			attempt.Status = profilemodel.AnalysisStatusOK
			if sourceRedacted {
				attempt.Status = profilemodel.AnalysisStatusPartial
			}
			if !attempt.Coverage.Complete || attempt.Coverage.TailLossNs > 0 {
				attempt.Status = profilemodel.AnalysisStatusPartial
				attempt.Diagnostics = append(attempt.Diagnostics, diagnostic(profilemodel.DiagnosticWarn, profilemodel.DiagnosticCoverageTruncated, "profile coverage does not span the complete run"))
			}
			if result.ForeignProfileLabel {
				attempt.Status = profilemodel.AnalysisStatusPartial
				attempt.Diagnostics = append(attempt.Diagnostics, diagnostic(profilemodel.DiagnosticWarn, profilemodel.DiagnosticForeignProfileLabel, "untrusted or unknown private profile labels were ignored"))
			}
		}
	}
	analysis.Status = profilemodel.RecomputeStatus(analysis)
	signed, err := profilemodel.SetAnalysisID(analysis)
	if err == nil {
		err = profilemodel.Validate(signed)
	}
	return signed, analysis.Status != profilemodel.AnalysisStatusFailed, err
}

func unavailableFlame(mode, reason string, generatedAt time.Time) *profilemodel.FlameGraph {
	return &profilemodel.FlameGraph{
		Status: "unsupported", Reason: reason, Mode: mode,
		NodeLimit: profilemodel.MaxFlameNodes, DepthLimit: profilemodel.MaxFlameDepth,
		AnalyzerVersion: cliVersion, GeneratedAt: generatedAt,
	}
}

func flameForAttempt(worker *profilemodel.FlameGraph, bundled bundleAttempt, analysis profilemodel.ProfileAnalysisV1) *profilemodel.FlameGraph {
	if bundled.Kind != "cpu" {
		return unavailableFlame(bundled.Mode, "unsupported-profile-type", analysis.GeneratedAt)
	}
	if analysis.Binary.Match != profilemodel.BinaryMatchVerified || analysis.Binary.Analyzed == nil {
		reason := "binary-provenance-unverified"
		if analysis.Binary.Match == profilemodel.BinaryMatchMismatch {
			reason = "binary-provenance-mismatch"
		}
		return unavailableFlame(bundled.Mode, reason, analysis.GeneratedAt)
	}
	if worker == nil || worker.Status != "ready" || len(worker.Nodes) == 0 {
		return unavailableFlame(bundled.Mode, "flame-data-unavailable", analysis.GeneratedAt)
	}
	flame := *worker
	flame.Nodes = append([]profilemodel.FlameNode(nil), worker.Nodes...)
	flame.Mode = bundled.Mode
	flame.InputSHA256 = make([]string, 0, len(bundled.Observed))
	for _, input := range bundled.Observed {
		flame.InputSHA256 = append(flame.InputSHA256, input.SHA256)
	}
	flame.BinarySHA256 = analysis.Binary.Analyzed.SHA256
	flame.AnalyzerVersion = cliVersion
	flame.GeneratedAt = analysis.GeneratedAt
	return &flame
}

func sanitizeSourcePaths(summaries []profilemodel.ProfileSummary, sourceRoot string) bool {
	redacted := false
	for summaryIndex := range summaries {
		for reportIndex := range summaries[summaryIndex].Reports {
			report := &summaries[summaryIndex].Reports[reportIndex]
			lists := [][]profilemodel.ProfileNode{report.TopFlat, report.TopCumulative, report.TopNegativeFlat, report.TopNegativeCumulative}
			for _, nodes := range lists {
				for nodeIndex := range nodes {
					if nodes[nodeIndex].File == "" {
						continue
					}
					path := filepath.Clean(nodes[nodeIndex].File)
					if filepath.IsAbs(path) && sourceRoot != "" {
						relative, err := filepath.Rel(sourceRoot, path)
						if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
							nodes[nodeIndex].File = filepath.ToSlash(relative)
							continue
						}
					}
					if !filepath.IsAbs(path) && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator)) {
						nodes[nodeIndex].File = filepath.ToSlash(path)
						continue
					}
					nodes[nodeIndex].File = "(redacted)"
					redacted = true
				}
			}
		}
	}
	return redacted
}

type handoffManifest struct {
	Schema            string                  `json:"schema"`
	RunID             string                  `json:"run_id"`
	SnapshotBase      string                  `json:"snapshot_base"`
	SnapshotSHA256    string                  `json:"snapshot_sha256"`
	BinarySHA256      string                  `json:"binary_sha256"`
	BinaryMatch       bool                    `json:"binary_match"`
	TestedGoVersion   string                  `json:"tested_go_version"`
	CapturedGoVersion string                  `json:"captured_go_version,omitempty"`
	Recipes           []profilehandoff.Recipe `json:"recipes"`
}

func runRecipes(args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("recipes", stderr)
	bundleDir := set.String("bundle-dir", "", "bundle directory")
	binaryPath := set.String("binary", "", "exact target binary")
	sourceRoot := set.String("source-root", "", "matching source tree")
	output := set.String("output", "json", "json or shell")
	timeout := set.Duration("timeout", defaultCommandTimeout, "isolated profile validation timeout")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if *bundleDir == "" || (*output != "json" && *output != "shell") || !validTimeout(*timeout) {
		return &usageError{message: "recipes requires --bundle-dir, --output json|shell, and timeout in 1s..5m"}
	}
	sourceAvailable := false
	if *sourceRoot != "" {
		info, err := os.Stat(*sourceRoot)
		if err != nil || !info.IsDir() {
			return errors.New("source root is not a directory")
		}
		sourceAvailable = true
	}
	root, err := safefs.Open(*bundleDir, safefs.Options{})
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	defer func() { _ = root.Close() }()
	bundle, err := loadBundle(root)
	if err != nil {
		return err
	}
	binary := "MATCHING_BINARY"
	binaryMatch := false
	if *binaryPath != "" {
		identity, captureErr := buildinfo.CaptureInputFile(*binaryPath)
		if captureErr != nil {
			return fmt.Errorf("capture input binary: %w", captureErr)
		}
		binary = *binaryPath
		binaryMatch = validHash(bundle.CapturedExecutable.SHA256) && identity.SHA256 == bundle.CapturedExecutable.SHA256 &&
			bundle.CapturedExecutable.Status == profilemodel.ExecutableStatusCaptured &&
			(bundle.CapturedExecutable.Source == profilemodel.ExecutableSourceProcSelfExe || bundle.CapturedExecutable.Source == profilemodel.ExecutableSourcePlatformBound)
	}
	handoff := handoffManifest{
		Schema: profilehandoff.SchemaV1, RunID: bundle.RunID, SnapshotBase: bundle.SnapshotBase,
		SnapshotSHA256: bundle.SnapshotSHA256, BinarySHA256: bundle.CapturedExecutable.SHA256,
		BinaryMatch: binaryMatch, TestedGoVersion: runtime.Version(), CapturedGoVersion: bundle.CapturedExecutable.GoVersion,
	}
	for _, attempt := range bundle.Attempts {
		inputs := make([]profilehandoff.InputFile, 0, len(attempt.Expected))
		observed := make(map[string]profilemodel.ObservedProfileInput, len(attempt.Observed))
		for _, value := range attempt.Observed {
			observed[value.ExpectedFile] = value
		}
		complete := len(attempt.Expected) != 0 && len(attempt.Expected) == len(attempt.Observed)
		for _, expected := range attempt.Expected {
			value := profilehandoff.InputFile{Point: expected.Point, File: expected.File}
			if found, ok := observed[expected.File]; ok {
				value.File, value.SHA256 = found.File, found.SHA256
			} else {
				complete = false
			}
			inputs = append(inputs, value)
		}
		sampleTypes := []profilehandoff.SampleType(nil)
		if complete {
			var validated bool
			sampleTypes, validated = validateRecipeAttempt(root, attempt, *timeout)
			complete = validated
		}
		recipes, generateErr := profilehandoff.Generate(profilehandoff.ProfileInput{
			Kind: attempt.Kind, Mode: attempt.Mode, Binary: binary, BinaryMatch: binaryMatch,
			SourceAvailable: sourceAvailable, SourceRoot: *sourceRoot, HasLabels: attempt.Dictionary != nil, SampleTypes: sampleTypes, Inputs: inputs,
		})
		if generateErr != nil {
			return fmt.Errorf("generate %s recipes: %w", attempt.Kind, generateErr)
		}
		if !complete {
			for index := range recipes {
				recipes[index].Ready = false
				recipes[index].Code = "profile-missing"
				recipes[index].Conditions = []string{"fetch every expected profile input and verify its SHA-256"}
			}
		}
		handoff.Recipes = append(handoff.Recipes, recipes...)
	}
	if bundle.Trace != nil {
		file := bundle.Trace.ExpectedFile
		if bundle.Trace.File != "" {
			file = bundle.Trace.File
		}
		ready := bundle.Trace.Complete && bundle.Trace.Status == "published" && bundle.Trace.File != ""
		recipes, generateErr := profilehandoff.GenerateTrace(file, ready)
		if generateErr != nil {
			return fmt.Errorf("generate trace recipe: %w", generateErr)
		}
		handoff.Recipes = append(handoff.Recipes, recipes...)
	}
	if *output == "json" {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(handoff); err != nil {
			return fmt.Errorf("write recipe manifest: %w", err)
		}
		return nil
	}
	for _, recipe := range handoff.Recipes {
		if !recipe.Ready {
			if _, err := fmt.Fprintf(stdout, "# unavailable %s: %s\n", recipe.Purpose, recipe.Code); err != nil {
				return fmt.Errorf("write recipes: %w", err)
			}
			continue
		}
		command, renderErr := profilehandoff.RenderShell(recipe.Argv)
		if renderErr != nil {
			return renderErr
		}
		if _, err := fmt.Fprintln(stdout, command); err != nil {
			return fmt.Errorf("write recipes: %w", err)
		}
	}
	return nil
}

func validateRecipeAttempt(root *safefs.Root, attempt bundleAttempt, timeout time.Duration) ([]profilehandoff.SampleType, bool) {
	executable, err := os.Executable()
	if err != nil {
		return nil, false
	}
	files := make([]*os.File, 0, len(attempt.Observed))
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	for _, observed := range attempt.Observed {
		file, openErr := root.OpenRegular(observed.File)
		if openErr != nil {
			return nil, false
		}
		files = append(files, file)
	}
	result, err := launchAnalysisWorker(context.Background(), executable, pprofanalyze.WorkerJob{
		Mode: attempt.Mode, TopN: 1, Dictionary: attempt.Dictionary, Profiles: files,
	}, pprofanalyze.WorkerOptions{CgroupRoot: os.Getenv("ISUTOOLS_PPROF_CGROUP_ROOT"), Timeout: timeout})
	if err != nil || result.ErrorCode != "" || len(result.Summaries) == 0 {
		return nil, false
	}
	seen := make(map[string]bool)
	types := make([]profilehandoff.SampleType, 0, len(result.Summaries))
	for _, summary := range result.Summaries {
		if summary.SampleType == "" || summary.Unit == "" || seen[summary.SampleType+"\x00"+summary.Unit] {
			continue
		}
		seen[summary.SampleType+"\x00"+summary.Unit] = true
		types = append(types, profilehandoff.SampleType{Type: summary.SampleType, Unit: summary.Unit})
	}
	sort.Slice(types, func(i, j int) bool {
		if types[i].Type != types[j].Type {
			return types[i].Type < types[j].Type
		}
		return types[i].Unit < types[j].Unit
	})
	return types, len(types) != 0
}

func runPublish(args []string, stdout, stderr io.Writer) error {
	set := newFlagSet("publish", stderr)
	admin := set.String("admin", "", "admin origin")
	analysisPath := set.String("analysis", "", "analysis JSON")
	expected := set.String("expected-current", "", "none or current artifact ID")
	timeout := set.Duration("timeout", defaultCommandTimeout, "request timeout")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if *admin == "" || *analysisPath == "" || (*expected != "none" && !validHash(*expected)) || !validTimeout(*timeout) {
		return &usageError{message: "publish requires --admin, --analysis, explicit --expected-current none|64hex, and timeout in 1s..5m"}
	}
	if _, err := validateAdminOrigin(*admin); err != nil {
		return &usageError{message: err.Error()}
	}
	file, err := os.Open(*analysisPath)
	if err != nil {
		return err
	}
	analysis, err := profilemodel.Decode(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	query := url.Values{"snapshot": []string{analysis.SnapshotBase}}
	endpoint, err := adminEndpoint(*admin, "/profile-analysis", query)
	if err != nil {
		return &usageError{message: err.Error()}
	}
	payload, err := json.Marshal(web.ProfileAnalysisPublishRequest{ExpectedCurrentArtifactID: *expected, Analysis: analysis})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	body, status, err := httpRequest(ctx, http.MethodPost, endpoint, "application/json", payload, maxHTTPMetadataBytes)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusAccepted {
		return fmt.Errorf("publish returned HTTP %d: %s", status, boundedText(body))
	}
	var response web.ProfileAnalysisPublishResponse
	if err := decodeStrict(body, &response); err != nil || !validHash(response.ArtifactID) {
		return fmt.Errorf("invalid publish response: %v", err)
	}
	if _, err := fmt.Fprintf(stdout, "publish ok: artifact=%s sequence=%d durability=%s\n", response.ArtifactID, response.CommitSequence, response.Durability); err != nil {
		return fmt.Errorf("write publish result: %w", err)
	}
	return nil
}

func loadBundle(root *safefs.Root) (profileBundle, error) {
	markerBody, err := root.ReadFile("bundle.current.json", maxHTTPMetadataBytes)
	if err != nil {
		return profileBundle{}, err
	}
	var marker bundleMarker
	if err := decodeStrict(markerBody, &marker); err != nil || marker.Schema != bundleMarkerSchema || !validHash(marker.BundleID) || !validHash(marker.ManifestSHA256) ||
		marker.ManifestFile != "bundle."+marker.BundleID+".json" {
		return profileBundle{}, errors.New("invalid bundle marker")
	}
	body, err := root.ReadFile(marker.ManifestFile, maxHTTPMetadataBytes)
	if err != nil || hashBytes(body) != marker.ManifestSHA256 {
		return profileBundle{}, errors.New("bundle manifest hash mismatch")
	}
	var bundle profileBundle
	if err := decodeStrict(body, &bundle); err != nil || (bundle.Schema != bundleSchema && bundle.Schema != legacyBundleSchema) || bundle.BundleID != marker.BundleID || !validBase(bundle.SnapshotBase) || !validHash(bundle.SnapshotSHA256) || bundle.RunID == "" || (len(bundle.Attempts) == 0 && bundle.Trace == nil) {
		return profileBundle{}, errors.New("invalid bundle manifest")
	}
	if bundle.Schema == bundleSchema && (bundle.CommandSchema != profilehandoff.SchemaV1 || bundle.TestedGoVersion == "") {
		return profileBundle{}, errors.New("invalid bundle command provenance")
	}
	copy := bundle
	copy.BundleID = ""
	canonical, _ := json.Marshal(copy)
	if hashBytes(canonical) != bundle.BundleID {
		return profileBundle{}, errors.New("bundle ID mismatch")
	}
	snapshot, err := root.ReadFile(bundle.SnapshotFile, 32<<20)
	if err != nil || hashBytes(snapshot) != bundle.SnapshotSHA256 {
		return profileBundle{}, errors.New("bundle snapshot hash mismatch")
	}
	for _, attempt := range bundle.Attempts {
		for _, observed := range attempt.Observed {
			body, readErr := root.ReadFile(observed.File, maxFetchedProfileBytes)
			if readErr != nil || int64(len(body)) != observed.Bytes || hashBytes(body) != observed.SHA256 {
				return profileBundle{}, fmt.Errorf("bundle profile %s hash mismatch", observed.File)
			}
			for _, attachment := range []struct {
				kind string
				file string
				hash string
			}{
				{kind: "sidecar", file: observed.Sidecar, hash: observed.SidecarSHA256},
				{kind: "coverage", file: observed.CoverageFile, hash: observed.CoverageSHA256},
			} {
				if attachment.file == "" {
					continue
				}
				body, readErr := root.ReadFile(attachment.file, profilecapture.MaxCompletionRecordBytes)
				if readErr != nil || hashBytes(body) != attachment.hash {
					return profileBundle{}, fmt.Errorf("bundle %s %s hash mismatch", attachment.kind, attachment.file)
				}
			}
		}
	}
	if trace := bundle.Trace; trace != nil {
		if !validTraceName(trace.ExpectedFile) {
			return profileBundle{}, errors.New("invalid bundle trace expectation")
		}
		if trace.File != "" {
			if trace.File != trace.ExpectedFile || !validHash(trace.SHA256) || trace.Bytes < 1 || !validAttachmentName(trace.Sidecar) || !validHash(trace.SidecarSHA256) {
				return profileBundle{}, errors.New("invalid bundle trace identity")
			}
			body, readErr := root.ReadFile(trace.File, maxFetchedTraceBytes)
			if readErr != nil || int64(len(body)) != trace.Bytes || hashBytes(body) != trace.SHA256 {
				return profileBundle{}, errors.New("bundle trace hash mismatch")
			}
			sidecar, readErr := root.ReadFile(trace.Sidecar, profilecapture.MaxCompletionRecordBytes)
			if readErr != nil || hashBytes(sidecar) != trace.SidecarSHA256 {
				return profileBundle{}, errors.New("bundle trace sidecar hash mismatch")
			}
		}
	}
	return bundle, nil
}

func publishBundleFile(root *safefs.Root, name string, body []byte) error {
	temp := "." + name + "." + randomToken() + ".tmp"
	file, err := root.CreateExclusive(temp, 0o600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = root.Remove(temp)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	publication, err := root.PublishNoReplace(temp, name)
	if errors.Is(err, safefs.ErrExists) {
		existing, readErr := root.ReadFile(name, int64(len(body)))
		if readErr == nil && bytes.Equal(existing, body) {
			return nil
		}
		return safefs.ErrExists
	}
	if publication.Visible {
		cleanup = false
	}
	if err != nil && !publication.Visible {
		return err
	}
	return nil
}

func replaceBundleFile(root *safefs.Root, name string, body []byte) error {
	temp := "." + name + "." + randomToken() + ".tmp"
	file, err := root.CreateExclusive(temp, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = root.Remove(temp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = root.Remove(temp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(temp)
		return err
	}
	publication, err := root.Replace(temp, name)
	if err != nil && !publication.Visible {
		_ = root.Remove(temp)
		return err
	}
	return nil
}

func writePrivateFile(path string, body []byte) error {
	directory, name := filepath.Dir(path), filepath.Base(path)
	if name == "." || name == "" {
		return errors.New("invalid output file")
	}
	root, err := safefs.Open(directory, safefs.Options{RequireStrongVisibility: true})
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return replaceBundleFile(root, name, body)
}

func httpRequest(ctx context.Context, method, endpoint, contentType string, payload []byte, limit int64) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := commandHTTPClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(body)) > limit {
		return nil, 0, errors.New("HTTP response exceeds limit")
	}
	return body, response.StatusCode, nil
}

func validateAdminOrigin(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("admin must be an http(s) origin without credentials, path, query, or fragment")
	}
	return parsed, nil
}

func adminEndpoint(raw, path string, query url.Values) (string, error) {
	parsed, err := validateAdminOrigin(raw)
	if err != nil {
		return "", err
	}
	parsed.Path = path
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func decodeStrict(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func modelExecutable(identity buildinfo.ExecutableIdentity) profilemodel.ExecutableIdentity {
	settings := make([]profilemodel.BuildSetting, len(identity.Settings))
	for index, setting := range identity.Settings {
		settings[index] = profilemodel.BuildSetting{Key: setting.Key, Value: setting.Value}
	}
	return profilemodel.ExecutableIdentity{
		SHA256: identity.SHA256, BuildInfoSHA256: identity.BuildInfoSHA256, Source: identity.Source,
		GoVersion: identity.GoVersion, MainModule: identity.MainModule, MainVersion: identity.MainVersion,
		MainSum: identity.MainSum, VCSRevision: identity.VCSRevision, VCSModified: identity.VCSModified,
		Settings: settings, Status: identity.Status,
	}
}

func modelDictionary(dictionary *web.CPULabelDictionary) *profilecapture.LabelDictionary {
	if dictionary == nil {
		return nil
	}
	tuples := make([]profilecapture.SafeLabelTuple, len(dictionary.Tuples))
	for index, tuple := range dictionary.Tuples {
		tuples[index] = profilecapture.SafeLabelTuple{TupleID: tuple.TupleID, Method: tuple.Method, Route: tuple.Route, Scenario: tuple.Scenario, Region: tuple.Region, Overflow: tuple.Overflow}
	}
	return &profilecapture.LabelDictionary{RunID: dictionary.RunID, Epoch: dictionary.Epoch, CaptureID: dictionary.CaptureID, Sealed: dictionary.Sealed, Tuples: tuples, SHA256: dictionary.SHA256}
}

func diagnostic(level, code, message string) profilemodel.Diagnostic {
	if len(message) > 512 {
		message = message[:512]
	}
	return profilemodel.Diagnostic{Level: level, Code: code, Message: message}
}

func validTimeout(value time.Duration) bool {
	return value >= minimumCommandTimeout && value <= maximumCommandTimeout
}
func validHash(value string) bool {
	return len(value) == 64 && strings.IndexFunc(value, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) < 0
}
func validBase(value string) bool {
	return value != "" && len(value) <= 200 && filepath.Base(value) == value && value != "."
}
func validArtifactName(value string) bool {
	return validBase(value) && strings.HasSuffix(value, ".pprof")
}
func validAttachmentName(value string) bool {
	return validBase(value) && strings.HasSuffix(value, ".json")
}
func validTraceName(value string) bool {
	return validBase(value) && strings.HasPrefix(value, "trace_") && strings.HasSuffix(value, ".out")
}
func hashBytes(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }
func checkedMultiply(a, b uint64) (uint64, bool) {
	high, low := bits.Mul64(a, b)
	return low, high != 0
}
func randomToken() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}
func boundedText(body []byte) string {
	text := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(body))
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}
