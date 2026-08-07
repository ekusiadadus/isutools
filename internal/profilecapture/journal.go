package profilecapture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

const (
	CompletionSchemaV1       = "isutools.cpu-profile-completion/v1"
	CompletionPhaseInitial   = "initial"
	CompletionPhaseCoverage  = "coverage"
	MaxCompletionRecordBytes = int64(128 << 10)
	maxCompletionSequence    = uint64(999_999)
)

type CaptureCoverage struct {
	BoundaryStart    time.Time     `json:"boundary_start,omitzero"`
	BoundaryFinish   time.Time     `json:"boundary_finish,omitzero"`
	StartRequestedAt time.Time     `json:"start_requested_at,omitzero"`
	StartCompletedAt time.Time     `json:"start_completed_at,omitzero"`
	StopRequestedAt  time.Time     `json:"stop_requested_at,omitzero"`
	StopCompletedAt  time.Time     `json:"stop_completed_at,omitzero"`
	StopReason       string        `json:"stop_reason,omitempty"`
	RunSpan          time.Duration `json:"run_span_ns,omitempty"`
	CaptureSpan      time.Duration `json:"capture_span_ns,omitempty"`
	HeadLoss         time.Duration `json:"head_loss_ns,omitempty"`
	TailExcess       time.Duration `json:"tail_excess_ns,omitempty"`
	TailLoss         time.Duration `json:"tail_loss_ns,omitempty"`
	Complete         bool          `json:"complete"`
}

// CompletionRecord is immutable. Sequence zero is the artifact-completion
// record; later sequence numbers append revised run-boundary coverage without
// changing either the profile or the initial record.
type CompletionRecord struct {
	Schema    string            `json:"schema"`
	Phase     string            `json:"phase"`
	Sequence  uint64            `json:"sequence"`
	RunID     string            `json:"run_id"`
	Epoch     uint64            `json:"epoch"`
	CaptureID string            `json:"capture_id"`
	State     State             `json:"state"`
	Code      string            `json:"code,omitempty"`
	Profile   PublishedArtifact `json:"profile"`
	Labels    LabelDictionary   `json:"labels"`
	Coverage  CaptureCoverage   `json:"coverage"`
}

// CompletionAttachment is the immutable file identity returned by a journal
// write. A caller may expose it in a snapshot only when Visible is true; an
// error after the no-replace publication can therefore be represented without
// falsely claiming that the artifact was never published.
type CompletionAttachment struct {
	Phase      string
	Sequence   uint64
	File       string
	SHA256     string
	Bytes      int64
	Visible    bool
	Durability string
}

type CompletionJournal interface {
	Record(CompletionRecord) (CompletionAttachment, error)
}

type FileCompletionJournal struct{ root *safefs.Root }

func NewFileCompletionJournal(root *safefs.Root) *FileCompletionJournal {
	return &FileCompletionJournal{root: root}
}

func (j *FileCompletionJournal) Record(record CompletionRecord) (CompletionAttachment, error) {
	if j == nil || j.root == nil {
		return CompletionAttachment{}, errors.New("profilecapture: completion journal is unavailable")
	}
	if err := validateCompletionRecord(record); err != nil {
		return CompletionAttachment{}, err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return CompletionAttachment{}, err
	}
	body = append(body, '\n')
	if int64(len(body)) > MaxCompletionRecordBytes {
		return CompletionAttachment{}, errors.New("profilecapture: completion record exceeds limit")
	}
	final := completionRecordName(record, body)
	sum := sha256.Sum256(body)
	attachment := CompletionAttachment{
		Phase: record.Phase, Sequence: record.Sequence, File: final,
		SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(body)),
	}
	temp := "." + final + "." + newCaptureID() + ".tmp"
	file, err := j.root.CreateExclusive(temp, 0o600)
	if err != nil {
		return CompletionAttachment{}, err
	}
	removeTemp := true
	defer func() {
		_ = file.Close()
		if removeTemp {
			_ = j.root.Remove(temp)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return CompletionAttachment{}, fmt.Errorf("profilecapture: write completion record: %w", err)
	}
	if err := file.Sync(); err != nil {
		return CompletionAttachment{}, fmt.Errorf("profilecapture: sync completion record: %w", err)
	}
	if err := file.Close(); err != nil {
		return CompletionAttachment{}, fmt.Errorf("profilecapture: close completion record: %w", err)
	}
	publication, err := j.root.PublishNoReplace(temp, final)
	if errors.Is(err, safefs.ErrExists) {
		existing, readErr := j.root.ReadFile(final, MaxCompletionRecordBytes)
		if readErr == nil && bytes.Equal(existing, body) {
			attachment.Visible = true
			attachment.Durability = string(safefs.DurabilityUnknown)
			return attachment, nil
		}
		return CompletionAttachment{}, safefs.ErrExists
	}
	attachment.Visible = publication.Visible
	attachment.Durability = string(publication.Durability)
	if publication.Visible {
		removeTemp = false
	}
	return attachment, err
}

func (j *FileCompletionJournal) LoadLatest(captureID string) (CompletionRecord, error) {
	if j == nil || j.root == nil || !validCaptureID(captureID) {
		return CompletionRecord{}, errors.New("profilecapture: invalid completion lookup")
	}
	entries, err := j.root.ReadDir()
	if err != nil {
		return CompletionRecord{}, err
	}
	prefix := "cpu_" + captureID + ".coverage."
	names := make([]string, 0, 4)
	initial := "cpu_" + captureID + ".meta.json"
	for _, entry := range entries {
		if entry.Name() == initial || strings.HasPrefix(entry.Name(), prefix) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var latest CompletionRecord
	found := false
	for _, name := range names {
		body, readErr := j.root.ReadFile(name, MaxCompletionRecordBytes)
		if readErr != nil {
			return CompletionRecord{}, readErr
		}
		var record CompletionRecord
		if err := decodeCompletionRecord(body, &record); err != nil {
			return CompletionRecord{}, err
		}
		if record.CaptureID != captureID || name != completionRecordName(record, body) {
			return CompletionRecord{}, errors.New("profilecapture: completion filename does not match record")
		}
		if !found || record.Sequence > latest.Sequence {
			latest, found = record, true
		}
	}
	if !found {
		return CompletionRecord{}, os.ErrNotExist
	}
	return latest, nil
}

func completionRecordName(record CompletionRecord, body []byte) string {
	if record.Phase == CompletionPhaseInitial {
		return "cpu_" + record.CaptureID + ".meta.json"
	}
	sum := sha256.Sum256(body)
	return "cpu_" + record.CaptureID + ".coverage." + fmt.Sprintf("%06d", record.Sequence) + "." + hex.EncodeToString(sum[:]) + ".json"
}

func decodeCompletionRecord(body []byte, record *CompletionRecord) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(record); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("profilecapture: trailing completion JSON")
	}
	return validateCompletionRecord(*record)
}

func validateCompletionRecord(record CompletionRecord) error {
	if record.Schema != CompletionSchemaV1 || !validCaptureID(record.CaptureID) || record.RunID == "" || record.Epoch == 0 ||
		!validCompletionText(record.RunID, 128) || !validCompletionText(record.Code, 64) ||
		record.Sequence > maxCompletionSequence {
		return errors.New("profilecapture: invalid completion record identity")
	}
	if (record.Phase == CompletionPhaseInitial && record.Sequence != 0) ||
		(record.Phase == CompletionPhaseCoverage && record.Sequence == 0) ||
		(record.Phase != CompletionPhaseInitial && record.Phase != CompletionPhaseCoverage) {
		return errors.New("profilecapture: invalid completion record phase")
	}
	expectedProfile := "cpu_" + record.CaptureID + ".pprof"
	if record.Profile.File != expectedProfile || !record.Profile.Visible || record.Profile.Bytes <= 0 ||
		len(record.Profile.SHA256) != 64 || !lowerHex(record.Profile.SHA256) ||
		(record.State != StatePublished && record.State != StateOrphaned) {
		return errors.New("profilecapture: invalid completion profile")
	}
	if !validPersistedLabelDictionary(record.Labels, record) {
		return errors.New("profilecapture: invalid completion label dictionary")
	}
	for _, duration := range []time.Duration{record.Coverage.RunSpan, record.Coverage.CaptureSpan, record.Coverage.HeadLoss, record.Coverage.TailExcess, record.Coverage.TailLoss} {
		if duration < 0 {
			return errors.New("profilecapture: negative completion duration")
		}
	}
	return nil
}

func validPersistedLabelDictionary(dictionary LabelDictionary, record CompletionRecord) bool {
	if dictionary.RunID != record.RunID || dictionary.Epoch != record.Epoch || dictionary.CaptureID != record.CaptureID ||
		!dictionary.Sealed || len(dictionary.Tuples) > MaxLabelTuples || len(dictionary.SHA256) != 64 || !lowerHex(dictionary.SHA256) {
		return false
	}
	seen := make(map[string]struct{}, len(dictionary.Tuples))
	overflow := 0
	for _, tuple := range dictionary.Tuples {
		if !validCaptureID(tuple.TupleID) || !validLabelMethod(tuple.Method) || !validLabelText(tuple.Route, 128) ||
			!validOptionalLabelToken(tuple.Scenario) || !validOptionalLabelToken(tuple.Region) {
			return false
		}
		if _, duplicate := seen[tuple.TupleID]; duplicate {
			return false
		}
		seen[tuple.TupleID] = struct{}{}
		if tuple.Overflow {
			overflow++
			if tuple.Method != "OTHER" || tuple.Route != "(overflow)" || tuple.Scenario != "" || tuple.Region != "" {
				return false
			}
		}
	}
	if overflow > 1 {
		return false
	}
	expected := dictionary.SHA256
	dictionary.SHA256 = ""
	body, err := json.Marshal(dictionary)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(body)
	return expected == hex.EncodeToString(sum[:])
}

func validCaptureID(value string) bool { return len(value) == 32 && lowerHex(value) }

func validCompletionText(value string, maximum int) bool {
	if len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}
