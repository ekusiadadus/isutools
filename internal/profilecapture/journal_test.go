package profilecapture

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

func TestFileCompletionJournalPublishesImmutableMetaAndSequencedCoverage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := safefs.Open(dir, safefs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	journal := NewFileCompletionJournal(root)
	captureID := strings.Repeat("a", 32)
	labels := NewStandaloneLabelScope(captureID)
	labels.Seal()
	record := CompletionRecord{
		Schema: CompletionSchemaV1, Phase: CompletionPhaseInitial, RunID: "run-1", Epoch: 1,
		CaptureID: captureID, State: StatePublished, Profile: PublishedArtifact{File: "cpu_" + captureID + ".pprof", SHA256: strings.Repeat("b", 64), Bytes: 12, Visible: true, Durability: "durable"},
		Labels:   labels.Dictionary("run-1", 1),
		Coverage: CaptureCoverage{BoundaryStart: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)},
	}
	initial, err := journal.Record(record)
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		t.Fatal(err)
	}
	if initial.File != "cpu_"+captureID+".meta.json" || initial.SHA256 == "" || initial.Bytes <= 0 || !initial.Visible || initial.Phase != CompletionPhaseInitial {
		t.Fatalf("initial attachment = %#v", initial)
	}
	record.Phase, record.Sequence, record.Coverage.TailLoss = CompletionPhaseCoverage, 1, time.Second
	coverage, err := journal.Record(record)
	if err != nil && !errors.Is(err, safefs.ErrDurabilityUnknown) {
		t.Fatal(err)
	}
	if coverage.File == initial.File || coverage.SHA256 == "" || coverage.Bytes <= 0 || !coverage.Visible || coverage.Phase != CompletionPhaseCoverage || coverage.Sequence != 1 {
		t.Fatalf("coverage attachment = %#v", coverage)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("journal files = %v", entries)
	}
	latest, err := journal.LoadLatest(captureID)
	if err != nil || latest.Phase != CompletionPhaseCoverage || latest.Sequence != 1 || latest.Coverage.TailLoss != time.Second {
		t.Fatalf("LoadLatest = %#v err=%v", latest, err)
	}
	body, err := root.ReadFile("cpu_"+captureID+".meta.json", MaxCompletionRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var persisted CompletionRecord
	if err := json.Unmarshal(body, &persisted); err != nil || persisted.Phase != CompletionPhaseInitial {
		t.Fatalf("persisted meta = %#v err=%v", persisted, err)
	}
	different := record
	different.Phase, different.Sequence, different.State = CompletionPhaseInitial, 0, StateOrphaned
	if _, err := journal.Record(different); !errors.Is(err, safefs.ErrExists) {
		t.Fatalf("mutable initial record = %v, want ErrExists", err)
	}
}

func TestCompletionCoverageSequenceFitsSixDigitArtifactGrammar(t *testing.T) {
	t.Parallel()
	captureID := strings.Repeat("a", 32)
	labels := NewStandaloneLabelScope(captureID)
	labels.Seal()
	record := CompletionRecord{
		Schema: CompletionSchemaV1, Phase: CompletionPhaseCoverage, Sequence: 1_000_000,
		RunID: "run-1", Epoch: 1, CaptureID: captureID, State: StatePublished,
		Profile: PublishedArtifact{File: "cpu_" + captureID + ".pprof", SHA256: strings.Repeat("b", 64), Bytes: 12, Visible: true, Durability: "durable"},
		Labels:  labels.Dictionary("run-1", 1),
	}
	if err := validateCompletionRecord(record); err == nil {
		t.Fatal("seven-digit completion sequence passed validation")
	}
	record.Sequence = 999_999
	if err := validateCompletionRecord(record); err != nil {
		t.Fatalf("six-digit completion sequence rejected: %v", err)
	}
}
