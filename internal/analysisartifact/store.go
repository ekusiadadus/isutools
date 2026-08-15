package analysisartifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

const (
	CurrentSchemaV1   = "isutools.external-analysis-current/v1"
	CommitSchemaV1    = "isutools.external-analysis-commit/v1"
	NoCurrentArtifact = "none"
	maxCurrentBytes   = int64(256 << 10)
	maxSequence       = uint64(999_999_999)
)

type Current struct {
	Schema             string `json:"schema"`
	Namespace          string `json:"namespace"`
	Kind               string `json:"kind"`
	ArtifactID         string `json:"artifact_id"`
	ManifestFile       string `json:"manifest_file"`
	ManifestSHA256     string `json:"manifest_sha256"`
	Sequence           uint64 `json:"sequence"`
	PreviousArtifactID string `json:"previous_artifact_id,omitempty"`
	CommitFile         string `json:"commit_file"`
}

type activationCommit struct {
	Schema             string  `json:"schema"`
	Current            Current `json:"current"`
	PreviousCommitFile string  `json:"previous_commit_file,omitempty"`
}

type PublishResult struct {
	Current
	Durability string `json:"durability"`
}

type ConflictError struct {
	CurrentArtifactID string
	Sequence          uint64
}

func (e *ConflictError) Error() string {
	return "analysisartifact: expected current artifact does not match"
}

type Store struct {
	root *safefs.Root
	seq  atomic.Uint64
}

type Summary struct {
	Namespace    string      `json:"namespace"`
	Kind         string      `json:"kind"`
	ArtifactID   string      `json:"artifact_id,omitempty"`
	Status       string      `json:"status"`
	Code         string      `json:"code,omitempty"`
	ManifestFile string      `json:"manifest_file"`
	Analyzer     Analyzer    `json:"analyzer,omitempty"`
	Run          *RunBinding `json:"run,omitempty"`
	Outputs      []FileRef   `json:"outputs,omitempty"`
}

func NewStore(root *safefs.Root) *Store { return &Store{root: root} }

// Content is one already-redacted analyzer output. Body is never accepted as
// an input reference: raw evidence must stay restricted and be described by a
// separately calculated FileRef in the manifest.
type Content struct {
	Role       string
	Extension  string
	MediaType  string
	Visibility string
	Body       []byte
	MaxBytes   int64
}

// PublishContent writes an immutable content-addressed output with mode 0600.
// Names are derived from the digest, not from an operator supplied path.
func (s *Store) PublishContent(namespace, kind string, content Content) (FileRef, error) {
	if s == nil || s.root == nil {
		return FileRef{}, errors.New("analysisartifact: store is unavailable")
	}
	if !validNamespace(namespace) || !validToken(kind, 64) || !validToken(content.Role, 64) ||
		!validToken(content.Extension, 12) || !validMediaType(content.MediaType) ||
		(content.Visibility != VisibilityPortable && content.Visibility != VisibilityRestricted) {
		return FileRef{}, errors.New("analysisartifact: invalid content metadata")
	}
	if content.MaxBytes < 1 || content.MaxBytes > int64(MaxFileBytes) || len(content.Body) == 0 || int64(len(content.Body)) > content.MaxBytes {
		return FileRef{}, errors.New("analysisartifact: content exceeds its byte budget")
	}
	digest := hashBytes(content.Body)
	name := "analysis-" + digest + "." + content.Extension
	ref := FileRef{
		Role: content.Role, Name: name, SHA256: digest, Bytes: uint64(len(content.Body)),
		MediaType: content.MediaType, Visibility: content.Visibility,
	}
	if err := validateFileRef(ref, false); err != nil {
		return FileRef{}, fmt.Errorf("analysisartifact: invalid content reference: %w", err)
	}
	_, err := s.publishImmutable(name, content.Body)
	if err != nil {
		return FileRef{}, err
	}
	return ref, nil
}

func (s *Store) Publish(namespace string, manifest Manifest, expectedCurrent string) (PublishResult, error) {
	if s == nil || s.root == nil {
		return PublishResult{}, errors.New("analysisartifact: store is unavailable")
	}
	if !validNamespace(namespace) {
		return PublishResult{}, errors.New("analysisartifact: invalid namespace")
	}
	if expectedCurrent != NoCurrentArtifact && !validHash(expectedCurrent) {
		return PublishResult{}, errors.New("analysisartifact: invalid expected current artifact")
	}
	if err := Validate(manifest); err != nil {
		return PublishResult{}, err
	}
	lock, err := s.root.TryLock(".analysisartifact." + namespace + "." + manifest.Kind + ".lock")
	if err != nil {
		return PublishResult{}, err
	}
	defer func() { _ = lock.Close() }()

	current, exists, err := s.loadCurrent(namespace, manifest.Kind)
	if err != nil {
		return PublishResult{}, err
	}
	if exists && current.ArtifactID == manifest.ArtifactID {
		if expectedCurrent != current.ArtifactID {
			return PublishResult{}, &ConflictError{CurrentArtifactID: current.ArtifactID, Sequence: current.Sequence}
		}
		return PublishResult{Current: current, Durability: string(safefs.DurabilityUnknown)}, nil
	}
	currentID := NoCurrentArtifact
	if exists {
		currentID = current.ArtifactID
	}
	if expectedCurrent != currentID {
		return PublishResult{}, &ConflictError{CurrentArtifactID: currentID, Sequence: current.Sequence}
	}
	sequence := uint64(1)
	if exists {
		if current.Sequence >= maxSequence {
			return PublishResult{}, errors.New("analysisartifact: activation sequence exhausted")
		}
		sequence = current.Sequence + 1
	}

	body, err := CanonicalJSON(manifest)
	if err != nil {
		return PublishResult{}, err
	}
	body = append(body, '\n')
	manifestSHA := hashBytes(body)
	manifestFile := namespace + "." + manifest.Kind + ".analysis." + manifest.ArtifactID + ".json"
	durabilityUnknown, err := s.publishImmutable(manifestFile, body)
	if err != nil {
		return PublishResult{}, err
	}

	next := Current{
		Schema: CurrentSchemaV1, Namespace: namespace, Kind: manifest.Kind, ArtifactID: manifest.ArtifactID,
		ManifestFile: manifestFile, ManifestSHA256: manifestSHA, Sequence: sequence,
		CommitFile: fmt.Sprintf("%s.%s.analysis.commit.%020d.json", namespace, manifest.Kind, sequence),
	}
	previousCommit := ""
	if exists {
		next.PreviousArtifactID = current.ArtifactID
		previousCommit = current.CommitFile
	}
	commitBody, err := json.Marshal(activationCommit{Schema: CommitSchemaV1, Current: next, PreviousCommitFile: previousCommit})
	if err != nil {
		return PublishResult{}, err
	}
	commitBody = append(commitBody, '\n')
	unknown, err := s.publishImmutable(next.CommitFile, commitBody)
	durabilityUnknown = durabilityUnknown || unknown
	if err != nil {
		return PublishResult{}, err
	}
	markerBody, err := json.Marshal(next)
	if err != nil {
		return PublishResult{}, err
	}
	markerBody = append(markerBody, '\n')
	temp := s.tempName(currentMarkerName(namespace, manifest.Kind))
	if err := s.writeTemp(temp, markerBody); err != nil {
		return PublishResult{}, err
	}
	publication, replaceErr := s.root.Replace(temp, currentMarkerName(namespace, manifest.Kind))
	if replaceErr != nil && !publication.Visible {
		_ = s.root.Remove(temp)
		return PublishResult{}, replaceErr
	}
	durabilityUnknown = durabilityUnknown || replaceErr != nil || publication.Durability != safefs.DurabilityDurable
	result := PublishResult{Current: next, Durability: string(safefs.DurabilityDurable)}
	if durabilityUnknown {
		result.Durability = string(safefs.DurabilityUnknown)
		return result, safefs.ErrDurabilityUnknown
	}
	return result, nil
}

func (s *Store) LoadCurrent(namespace, kind string) (Current, error) {
	if s == nil || s.root == nil || !validNamespace(namespace) || !validToken(kind, 64) {
		return Current{}, errors.New("analysisartifact: invalid current lookup")
	}
	current, exists, err := s.loadCurrent(namespace, kind)
	if err != nil {
		return Current{}, err
	}
	if !exists {
		return Current{}, errors.New("analysisartifact: current artifact does not exist")
	}
	return current, nil
}

// ListCurrent returns only verified, regular current markers. Restricted
// outputs remain represented by metadata but are omitted from the portable
// link list used by dashboards and exported reports.
func (s *Store) ListCurrent(limit int) []Summary {
	if s == nil || s.root == nil || limit < 1 {
		return nil
	}
	if limit > 256 {
		limit = 256
	}
	entries, err := s.root.ReadDir()
	if err != nil {
		return nil
	}
	summaries := make([]Summary, 0)
	for _, entry := range entries {
		if len(summaries) >= limit || !strings.HasSuffix(entry.Name(), ".analysis.current.json") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			continue
		}
		body, readErr := s.root.ReadFile(entry.Name(), maxCurrentBytes)
		var marker Current
		if readErr != nil || strictDecode(body, &marker) != nil || currentMarkerName(marker.Namespace, marker.Kind) != entry.Name() {
			continue
		}
		current, loadErr := s.LoadCurrent(marker.Namespace, marker.Kind)
		if loadErr != nil {
			summaries = append(summaries, Summary{Namespace: marker.Namespace, Kind: marker.Kind, Status: StatusInvalid, Code: "current-verification-failed", ManifestFile: marker.ManifestFile})
			continue
		}
		manifestBody, readErr := s.root.ReadFile(current.ManifestFile, MaxManifestBytes)
		if readErr != nil {
			continue
		}
		inspection, inspectErr := Inspect(bytes.NewReader(manifestBody))
		if inspectErr != nil {
			summaries = append(summaries, Summary{Namespace: current.Namespace, Kind: current.Kind, Status: StatusInvalid, Code: "manifest-invalid", ManifestFile: current.ManifestFile})
			continue
		}
		summary := Summary{Namespace: current.Namespace, Kind: inspection.Kind, ArtifactID: inspection.ArtifactID, Status: inspection.Status, Code: inspection.Code, ManifestFile: current.ManifestFile}
		if inspection.Schema == SchemaV1 {
			manifest, decodeErr := Decode(bytes.NewReader(manifestBody))
			if decodeErr != nil {
				continue
			}
			summary.Analyzer, summary.Run = manifest.Analyzer, manifest.Run
			for _, output := range manifest.Outputs {
				if output.Visibility == VisibilityPortable {
					summary.Outputs = append(summary.Outputs, output)
				}
			}
			if manifest.Run != nil {
				if verifyErr := VerifyRunBinding(s.root, *manifest.Run); verifyErr != nil {
					summary.Status, summary.Code, summary.Outputs = StatusInvalid, "snapshot-binding-mismatch", nil
				}
			}
		}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Namespace != summaries[j].Namespace {
			return summaries[i].Namespace > summaries[j].Namespace
		}
		return summaries[i].Kind < summaries[j].Kind
	})
	return summaries
}

func (s *Store) loadCurrent(namespace, kind string) (Current, bool, error) {
	body, err := s.root.ReadFile(currentMarkerName(namespace, kind), maxCurrentBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Current{}, false, nil
		}
		return Current{}, false, err
	}
	var current Current
	if err := strictDecode(body, &current); err != nil {
		return Current{}, false, fmt.Errorf("analysisartifact: invalid current marker: %w", err)
	}
	if err := validateCurrent(current, namespace, kind); err != nil {
		return Current{}, false, err
	}
	manifestBody, err := s.root.ReadFile(current.ManifestFile, MaxManifestBytes)
	if err != nil {
		return Current{}, false, fmt.Errorf("analysisartifact: read current manifest: %w", err)
	}
	if hashBytes(manifestBody) != current.ManifestSHA256 {
		return Current{}, false, errors.New("analysisartifact: current manifest hash mismatch")
	}
	inspection, err := Inspect(bytes.NewReader(manifestBody))
	if err != nil || inspection.ArtifactID != current.ArtifactID || inspection.Kind != kind {
		return Current{}, false, errors.New("analysisartifact: current manifest identity mismatch")
	}
	commitBody, err := s.root.ReadFile(current.CommitFile, maxCurrentBytes)
	if err != nil {
		return Current{}, false, fmt.Errorf("analysisartifact: read current activation commit: %w", err)
	}
	var commit activationCommit
	if err := strictDecode(commitBody, &commit); err != nil || commit.Schema != CommitSchemaV1 || !reflect.DeepEqual(commit.Current, current) {
		return Current{}, false, errors.New("analysisartifact: current activation commit mismatch")
	}
	return current, true, nil
}

func validateCurrent(current Current, namespace, kind string) error {
	if current.Schema != CurrentSchemaV1 || current.Namespace != namespace || current.Kind != kind ||
		!validNamespace(current.Namespace) || !validToken(current.Kind, 64) || !validHash(current.ArtifactID) ||
		!validHash(current.ManifestSHA256) || current.Sequence == 0 || current.Sequence > maxSequence ||
		(current.PreviousArtifactID != "" && !validHash(current.PreviousArtifactID)) {
		return errors.New("analysisartifact: invalid current marker identity")
	}
	wantManifest := namespace + "." + kind + ".analysis." + current.ArtifactID + ".json"
	wantCommit := fmt.Sprintf("%s.%s.analysis.commit.%020d.json", namespace, kind, current.Sequence)
	if current.ManifestFile != wantManifest || current.CommitFile != wantCommit {
		return errors.New("analysisartifact: invalid current marker filenames")
	}
	return nil
}

func currentMarkerName(namespace, kind string) string {
	return namespace + "." + kind + ".analysis.current.json"
}

func validNamespace(namespace string) bool {
	return validToken(namespace, 80) && namespace != "." && !strings.HasPrefix(namespace, ".")
}

func (s *Store) publishImmutable(final string, body []byte) (bool, error) {
	temp := s.tempName(final)
	if err := s.writeTemp(temp, body); err != nil {
		return false, err
	}
	publication, err := s.root.PublishNoReplace(temp, final)
	if errors.Is(err, safefs.ErrExists) {
		_ = s.root.Remove(temp)
		existing, readErr := s.root.ReadFile(final, int64(len(body))+1)
		if readErr == nil && bytes.Equal(existing, body) {
			return false, nil
		}
		return false, errors.New("analysisartifact: immutable artifact exists with different bytes")
	}
	if err != nil && !publication.Visible {
		_ = s.root.Remove(temp)
		return false, err
	}
	return err != nil || publication.Durability != safefs.DurabilityDurable, nil
}

func (s *Store) writeTemp(name string, body []byte) error {
	file, err := s.root.CreateExclusive(name, 0o600)
	if err != nil {
		return err
	}
	fail := func(cause error) error {
		_ = file.Close()
		_ = s.root.Remove(name)
		return cause
	}
	if _, err := file.Write(body); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = s.root.Remove(name)
		return err
	}
	return nil
}

func (s *Store) tempName(final string) string {
	return fmt.Sprintf(".%s.%020d.tmp", final, s.seq.Add(1))
}

func strictDecode(body []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}

func hashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
