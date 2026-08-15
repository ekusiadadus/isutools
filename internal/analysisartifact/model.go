// Package analysisartifact defines the bounded, analyzer-neutral envelope used
// to attach post-run evidence to an immutable isutools snapshot.
package analysisartifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	SchemaV1             = "isutools.external-analysis/v1"
	MaxManifestBytes     = int64(2 << 20)
	MaxFiles             = 32
	MaxDiagnostics       = 64
	MaxExtensions        = 8
	MaxExtensionBytes    = 64 << 10
	MaxMemoryBytes       = uint64(4 << 30)
	MaxFileBytes         = uint64(1 << 30)
	MaxTimeoutMS         = uint64((10 * time.Minute) / time.Millisecond)
	SHA256HexLength      = 64
	StatusReady          = "ready"
	StatusPartial        = "partial"
	StatusUnsupported    = "unsupported"
	StatusFailed         = "failed"
	StatusInvalid        = "invalid"
	KindAccessLog        = "accesslog"
	KindMySQLSlowLog     = "mysql-slowlog"
	KindRuntimeProfile   = "runtime-profile"
	KindTrace            = "trace"
	KindProfileHandoff   = "profile-handoff"
	KindPGO              = "pgo"
	VisibilityPortable   = "portable"
	VisibilityRestricted = "restricted"
	DiagnosticInfo       = "info"
	DiagnosticWarn       = "warn"
	DiagnosticError      = "error"
)

type Manifest struct {
	Schema      string                     `json:"schema"`
	ArtifactID  string                     `json:"artifact_id"`
	Kind        string                     `json:"kind"`
	GeneratedAt time.Time                  `json:"generated_at"`
	Analyzer    Analyzer                   `json:"analyzer"`
	Status      string                     `json:"status"`
	Run         *RunBinding                `json:"run,omitempty"`
	Inputs      []FileRef                  `json:"inputs,omitempty"`
	Outputs     []FileRef                  `json:"outputs,omitempty"`
	Executable  *Executable                `json:"executable,omitempty"`
	Coverage    Coverage                   `json:"coverage"`
	Budget      ResourceBudget             `json:"budget"`
	Diagnostics []Diagnostic               `json:"diagnostics,omitempty"`
	Extensions  map[string]json.RawMessage `json:"extensions,omitempty"`
}

type Analyzer struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
}

type RunBinding struct {
	RunID                 string `json:"run_id"`
	SnapshotBase          string `json:"snapshot_base"`
	SnapshotSHA256        string `json:"snapshot_sha256"`
	SnapshotSchemaVersion int    `json:"snapshot_schema_version"`
}

type FileRef struct {
	Role       string `json:"role"`
	Name       string `json:"name"`
	SHA256     string `json:"sha256"`
	Bytes      uint64 `json:"bytes"`
	MediaType  string `json:"media_type"`
	Visibility string `json:"visibility"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type Executable struct {
	CapturedSHA256 string `json:"captured_sha256,omitempty"`
	AnalyzedSHA256 string `json:"analyzed_sha256,omitempty"`
	Match          string `json:"match"`
	GoVersion      string `json:"go_version,omitempty"`
	VCSRevision    string `json:"vcs_revision,omitempty"`
	VCSModified    bool   `json:"vcs_modified,omitempty"`
}

type Coverage struct {
	Complete        bool      `json:"complete"`
	Clock           string    `json:"clock,omitempty"`
	StartedAt       time.Time `json:"started_at,omitzero"`
	EndedAt         time.Time `json:"ended_at,omitzero"`
	StartDevice     uint64    `json:"start_device,omitempty"`
	StartInode      uint64    `json:"start_inode,omitempty"`
	StartOffset     uint64    `json:"start_offset,omitempty"`
	EndDevice       uint64    `json:"end_device,omitempty"`
	EndInode        uint64    `json:"end_inode,omitempty"`
	EndOffset       uint64    `json:"end_offset,omitempty"`
	ApproximationNS uint64    `json:"approximation_ns,omitempty"`
	Reason          string    `json:"reason,omitempty"`
}

type ResourceBudget struct {
	TimeoutMS      uint64 `json:"timeout_ms,omitempty"`
	MaxInputBytes  uint64 `json:"max_input_bytes,omitempty"`
	MaxOutputBytes uint64 `json:"max_output_bytes,omitempty"`
	MaxMemoryBytes uint64 `json:"max_memory_bytes,omitempty"`
}

type Diagnostic struct {
	Level   string `json:"level"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func SetArtifactID(in Manifest) (Manifest, error) {
	in = normalize(in)
	in.ArtifactID = ""
	body, err := canonicalJSON(in)
	if err != nil {
		return in, err
	}
	sum := sha256.Sum256(body)
	in.ArtifactID = hex.EncodeToString(sum[:])
	return in, nil
}

func CanonicalJSON(in Manifest) ([]byte, error) {
	return canonicalJSON(normalize(in))
}

func canonicalJSON(in Manifest) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(in); err != nil {
		return nil, fmt.Errorf("analysisartifact: encode manifest: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

func normalize(in Manifest) Manifest {
	if !in.GeneratedAt.IsZero() {
		in.GeneratedAt = in.GeneratedAt.UTC()
	}
	if !in.Coverage.StartedAt.IsZero() {
		in.Coverage.StartedAt = in.Coverage.StartedAt.UTC()
	}
	if !in.Coverage.EndedAt.IsZero() {
		in.Coverage.EndedAt = in.Coverage.EndedAt.UTC()
	}
	return in
}

func Decode(r io.Reader) (Manifest, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("analysisartifact: read manifest: %w", err)
	}
	if int64(len(body)) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("analysisartifact: manifest exceeds %d bytes", MaxManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var out Manifest
	if err := dec.Decode(&out); err != nil {
		return Manifest{}, fmt.Errorf("analysisartifact: decode manifest: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("analysisartifact: trailing JSON value")
		}
		return Manifest{}, fmt.Errorf("analysisartifact: trailing JSON: %w", err)
	}
	if err := Validate(out); err != nil {
		return Manifest{}, err
	}
	return normalize(out), nil
}

func Validate(in Manifest) error {
	in = normalize(in)
	if in.Schema != SchemaV1 {
		return fmt.Errorf("analysisartifact: schema %q is unsupported", in.Schema)
	}
	if !validHash(in.ArtifactID) {
		return errors.New("analysisartifact: artifact_id must be a lowercase sha256")
	}
	if !validToken(in.Kind, 64) {
		return errors.New("analysisartifact: invalid kind")
	}
	if in.GeneratedAt.IsZero() {
		return errors.New("analysisartifact: generated_at is required")
	}
	if err := boundedText("analyzer name", in.Analyzer.Name, 64, true); err != nil {
		return err
	}
	if err := boundedText("analyzer version", in.Analyzer.Version, 128, true); err != nil {
		return err
	}
	if err := boundedText("analyzer revision", in.Analyzer.Revision, 128, false); err != nil {
		return err
	}
	switch in.Status {
	case StatusReady, StatusPartial, StatusUnsupported, StatusFailed, StatusInvalid:
	default:
		return fmt.Errorf("analysisartifact: invalid status %q", in.Status)
	}
	if err := validateRun(in.Run); err != nil {
		return err
	}
	if len(in.Inputs) > MaxFiles || len(in.Outputs) > MaxFiles || len(in.Inputs)+len(in.Outputs) > MaxFiles {
		return fmt.Errorf("analysisartifact: file reference count exceeds %d", MaxFiles)
	}
	for i := range in.Inputs {
		if err := validateFileRef(in.Inputs[i], true); err != nil {
			return fmt.Errorf("analysisartifact: input[%d]: %w", i, err)
		}
	}
	for i := range in.Outputs {
		if err := validateFileRef(in.Outputs[i], false); err != nil {
			return fmt.Errorf("analysisartifact: output[%d]: %w", i, err)
		}
	}
	if in.Status == StatusReady && len(in.Outputs) == 0 {
		return errors.New("analysisartifact: ready manifest requires an output")
	}
	if (in.Status == StatusFailed || in.Status == StatusInvalid || in.Status == StatusUnsupported) && len(in.Outputs) != 0 {
		return fmt.Errorf("analysisartifact: %s manifest must not publish outputs", in.Status)
	}
	if (in.Status == StatusFailed || in.Status == StatusInvalid) && !hasError(in.Diagnostics) {
		return fmt.Errorf("analysisartifact: %s manifest requires an error diagnostic", in.Status)
	}
	if err := validateExecutable(in.Executable); err != nil {
		return err
	}
	if err := validateCoverage(in.Coverage); err != nil {
		return err
	}
	if err := validateBudget(in.Budget); err != nil {
		return err
	}
	if len(in.Diagnostics) > MaxDiagnostics {
		return fmt.Errorf("analysisartifact: diagnostics exceed %d entries", MaxDiagnostics)
	}
	for i := range in.Diagnostics {
		if err := validateDiagnostic(in.Diagnostics[i]); err != nil {
			return fmt.Errorf("analysisartifact: diagnostic[%d]: %w", i, err)
		}
	}
	if err := validateExtensions(in.Extensions); err != nil {
		return err
	}
	want, err := SetArtifactID(in)
	if err != nil {
		return err
	}
	if in.ArtifactID != want.ArtifactID {
		return errors.New("analysisartifact: artifact_id does not match canonical manifest")
	}
	body, err := CanonicalJSON(in)
	if err != nil {
		return err
	}
	if int64(len(body)) > MaxManifestBytes {
		return fmt.Errorf("analysisartifact: canonical manifest exceeds %d bytes", MaxManifestBytes)
	}
	return nil
}

func validateRun(run *RunBinding) error {
	if run == nil {
		return nil
	}
	if boundedText("run id", run.RunID, 128, true) != nil || boundedText("snapshot base", run.SnapshotBase, 255, true) != nil ||
		filepath.Base(run.SnapshotBase) != run.SnapshotBase || run.SnapshotBase == "." || !validHash(run.SnapshotSHA256) || run.SnapshotSchemaVersion <= 0 {
		return errors.New("analysisartifact: invalid run binding")
	}
	return nil
}

func validateFileRef(ref FileRef, input bool) error {
	if !validToken(ref.Role, 64) {
		return errors.New("invalid role")
	}
	if err := boundedText("name", ref.Name, 255, true); err != nil || filepath.Base(ref.Name) != ref.Name || ref.Name == "." {
		return errors.New("name must be a bounded basename")
	}
	if !validHash(ref.SHA256) {
		return errors.New("sha256 must be a lowercase digest")
	}
	if ref.Bytes == 0 || ref.Bytes > MaxFileBytes {
		return errors.New("bytes are outside the file limit")
	}
	if !validMediaType(ref.MediaType) {
		return errors.New("invalid media type")
	}
	if input && ref.Visibility != VisibilityRestricted {
		return errors.New("input visibility must be restricted")
	}
	if !input && ref.Visibility != VisibilityRestricted && ref.Visibility != VisibilityPortable {
		return errors.New("invalid output visibility")
	}
	return nil
}

func validateExecutable(executable *Executable) error {
	if executable == nil {
		return nil
	}
	if executable.CapturedSHA256 != "" && !validHash(executable.CapturedSHA256) {
		return errors.New("analysisartifact: invalid captured executable sha256")
	}
	if executable.AnalyzedSHA256 != "" && !validHash(executable.AnalyzedSHA256) {
		return errors.New("analysisartifact: invalid analyzed executable sha256")
	}
	switch executable.Match {
	case "verified":
		if executable.CapturedSHA256 == "" || executable.AnalyzedSHA256 == "" || executable.CapturedSHA256 != executable.AnalyzedSHA256 {
			return errors.New("analysisartifact: verified executable hashes do not match")
		}
	case "mismatch", "unknown":
	default:
		return errors.New("analysisartifact: invalid executable match")
	}
	if err := boundedText("go version", executable.GoVersion, 64, false); err != nil {
		return err
	}
	return boundedText("vcs revision", executable.VCSRevision, 128, false)
}

func validateCoverage(coverage Coverage) error {
	if err := boundedText("coverage clock", coverage.Clock, 64, false); err != nil {
		return err
	}
	if err := boundedText("coverage reason", coverage.Reason, 256, false); err != nil {
		return err
	}
	if (!coverage.StartedAt.IsZero() || !coverage.EndedAt.IsZero()) &&
		(coverage.StartedAt.IsZero() || coverage.EndedAt.IsZero() || coverage.EndedAt.Before(coverage.StartedAt)) {
		return errors.New("analysisartifact: invalid coverage time interval")
	}
	if coverage.EndOffset < coverage.StartOffset {
		return errors.New("analysisartifact: invalid coverage offset interval")
	}
	return nil
}

func validateBudget(budget ResourceBudget) error {
	if budget.TimeoutMS > MaxTimeoutMS {
		return errors.New("analysisartifact: timeout exceeds limit")
	}
	if budget.MaxInputBytes > MaxFileBytes || budget.MaxOutputBytes > MaxFileBytes {
		return errors.New("analysisartifact: input/output byte budget exceeds limit")
	}
	if budget.MaxMemoryBytes > MaxMemoryBytes {
		return errors.New("analysisartifact: memory budget exceeds limit")
	}
	return nil
}

func validateDiagnostic(d Diagnostic) error {
	switch d.Level {
	case DiagnosticInfo, DiagnosticWarn, DiagnosticError:
	default:
		return errors.New("invalid diagnostic level")
	}
	if !validToken(d.Code, 64) {
		return errors.New("invalid diagnostic code")
	}
	if err := boundedText("diagnostic message", d.Message, 512, true); err != nil {
		return fmt.Errorf("invalid diagnostic message: %w", err)
	}
	return nil
}

func validateExtensions(extensions map[string]json.RawMessage) error {
	if len(extensions) > MaxExtensions {
		return fmt.Errorf("analysisartifact: extensions exceed %d", MaxExtensions)
	}
	keys := make([]string, 0, len(extensions))
	for key := range extensions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := extensions[key]
		if !validToken(key, 64) || len(raw) == 0 || len(raw) > MaxExtensionBytes || !json.Valid(raw) {
			return fmt.Errorf("analysisartifact: invalid extension %q", key)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return fmt.Errorf("analysisartifact: extension %q must be an object", key)
		}
	}
	return nil
}

func hasError(diagnostics []Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Level == DiagnosticError {
			return true
		}
	}
	return false
}

func validHash(value string) bool {
	if len(value) != SHA256HexLength {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validToken(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' && r != '_' {
			return false
		}
	}
	return true
}

func validMediaType(value string) bool {
	if value == "" || len(value) > 128 || !strings.Contains(value, "/") {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e || r == '\\' || r == '"' {
			return false
		}
	}
	return true
}

func boundedText(name, value string, limit int, required bool) error {
	if required && value == "" {
		return fmt.Errorf("analysisartifact: %s is required", name)
	}
	if len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("analysisartifact: %s is invalid", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("analysisartifact: %s contains a control character", name)
		}
	}
	return nil
}
