// Package pgoworkflow builds evidence-bound, opt-in Go PGO candidates.
package pgoworkflow

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
	"time"
	"unicode"

	pprofprofile "github.com/google/pprof/profile"
)

const (
	SchemaV1         = "isutools.pgo-candidate/v1"
	MaxManifestBytes = int64(256 << 10)
	MaxProfileBytes  = int64(64 << 20)
)

type Input struct {
	RunID                string
	SnapshotBase         string
	SnapshotSHA256       string
	ProfileSHA256        string
	ProfileBytes         uint64
	CapturedBinarySHA256 string
	SourceRevision       string
	SourceDirty          bool
	Toolchain            string
	MainPackage          string
	Rationale            string
	CreatedAt            time.Time
}

type FileRef struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  uint64 `json:"bytes"`
	Kind   string `json:"kind"`
}

type Command struct {
	Schema string   `json:"schema"`
	Argv   []string `json:"argv"`
}

type EvaluationLedger struct {
	PrimaryMetric       string          `json:"primary_metric"`
	MinimumEffect       string          `json:"minimum_effect"`
	Blocks              int             `json:"blocks"`
	Sequence            string          `json:"sequence"`
	RequirePass         bool            `json:"require_pass"`
	FixedInitialization string          `json:"fixed_initialization"`
	Status              string          `json:"status"`
	Results             []EvaluationRun `json:"results"`
}

type EvaluationRun struct {
	Block        int     `json:"block"`
	Variant      string  `json:"variant"`
	Score        float64 `json:"score"`
	Pass         bool    `json:"pass"`
	SnapshotSHA  string  `json:"snapshot_sha256"`
	BinarySHA256 string  `json:"binary_sha256"`
}

type Candidate struct {
	Schema               string           `json:"schema"`
	CandidateID          string           `json:"candidate_id"`
	CreatedAt            time.Time        `json:"created_at"`
	ExpiresAt            time.Time        `json:"expires_at"`
	RunID                string           `json:"run_id"`
	SnapshotBase         string           `json:"snapshot_base"`
	SnapshotSHA256       string           `json:"snapshot_sha256"`
	Profile              FileRef          `json:"profile"`
	CapturedBinarySHA256 string           `json:"captured_binary_sha256"`
	SourceRevision       string           `json:"source_revision"`
	SourceDirty          bool             `json:"source_dirty"`
	Toolchain            string           `json:"toolchain"`
	MainPackage          string           `json:"main_package"`
	Rationale            string           `json:"representative_workload_rationale"`
	BuildPGO             Command          `json:"build_pgo"`
	Rollback             Command          `json:"rollback"`
	Evaluation           EvaluationLedger `json:"evaluation"`
}

func Prepare(input Input) (Candidate, error) {
	if !safeText(input.RunID, 128) || !safeBase(input.SnapshotBase) || !validHash(input.SnapshotSHA256) ||
		!validHash(input.ProfileSHA256) || input.ProfileBytes == 0 || input.ProfileBytes > uint64(MaxProfileBytes) ||
		!validHash(input.CapturedBinarySHA256) || !safeRevision(input.SourceRevision) || input.SourceDirty ||
		!safeText(input.Toolchain, 64) || !safePackage(input.MainPackage) || !safeText(input.Rationale, 512) || input.CreatedAt.IsZero() {
		return Candidate{}, errors.New("pgoworkflow: invalid or unsafe candidate provenance")
	}
	created := input.CreatedAt.UTC()
	candidate := Candidate{
		Schema: SchemaV1, CreatedAt: created, ExpiresAt: created.Add(7 * 24 * time.Hour), RunID: input.RunID,
		SnapshotBase: input.SnapshotBase, SnapshotSHA256: input.SnapshotSHA256,
		Profile:              FileRef{Name: "default.pgo", SHA256: input.ProfileSHA256, Bytes: input.ProfileBytes, Kind: "cpu"},
		CapturedBinarySHA256: input.CapturedBinarySHA256, SourceRevision: input.SourceRevision, SourceDirty: false,
		Toolchain: input.Toolchain, MainPackage: input.MainPackage, Rationale: input.Rationale,
		BuildPGO: Command{Schema: "go-build-argv/v1", Argv: []string{"go", "build", "-pgo=CANDIDATE_DIR/default.pgo", "-o", "OUTPUT_BINARY", input.MainPackage}},
		Rollback: Command{Schema: "go-build-argv/v1", Argv: []string{"go", "build", "-pgo=off", "-o", "OUTPUT_BINARY", input.MainPackage}},
		Evaluation: EvaluationLedger{PrimaryMetric: "score", MinimumEffect: "operator-predeclared", Blocks: 4, Sequence: "ABBA", RequirePass: true,
			FixedInitialization: "same source, host, database initialization, warm-up, and benchmark workload", Status: "pending", Results: []EvaluationRun{}},
	}
	body, err := canonicalWithoutID(candidate)
	if err != nil {
		return Candidate{}, err
	}
	candidate.CandidateID = hashBytes(body)
	return candidate, Validate(candidate)
}

func ValidateCPUProfile(body []byte) error {
	if len(body) == 0 || int64(len(body)) > MaxProfileBytes {
		return errors.New("pgoworkflow: profile size is invalid")
	}
	parsed, err := pprofprofile.ParseData(body)
	if err != nil {
		return errors.New("pgoworkflow: profile is corrupt")
	}
	cpu := parsed.PeriodType != nil && parsed.PeriodType.Type == "cpu" && parsed.PeriodType.Unit == "nanoseconds"
	for _, sample := range parsed.SampleType {
		cpu = cpu || (sample.Type == "cpu" && sample.Unit == "nanoseconds")
	}
	if !cpu {
		return errors.New("pgoworkflow: profile is not a Go CPU profile")
	}
	return nil
}

func Validate(candidate Candidate) error {
	if candidate.Schema != SchemaV1 || !validHash(candidate.CandidateID) || candidate.Profile.Name != "default.pgo" || candidate.Profile.Kind != "cpu" ||
		!validHash(candidate.Profile.SHA256) || candidate.Profile.Bytes == 0 || candidate.Profile.Bytes > uint64(MaxProfileBytes) ||
		!validHash(candidate.SnapshotSHA256) || !validHash(candidate.CapturedBinarySHA256) || !safeText(candidate.RunID, 128) || !safeBase(candidate.SnapshotBase) ||
		!safeRevision(candidate.SourceRevision) || candidate.SourceDirty || !safeText(candidate.Toolchain, 64) || !safePackage(candidate.MainPackage) ||
		!safeText(candidate.Rationale, 512) || candidate.CreatedAt.IsZero() || !candidate.ExpiresAt.After(candidate.CreatedAt) || candidate.ExpiresAt.Sub(candidate.CreatedAt) > 7*24*time.Hour {
		return errors.New("pgoworkflow: invalid candidate identity")
	}
	wantPGO := []string{"go", "build", "-pgo=CANDIDATE_DIR/default.pgo", "-o", "OUTPUT_BINARY", candidate.MainPackage}
	wantRollback := []string{"go", "build", "-pgo=off", "-o", "OUTPUT_BINARY", candidate.MainPackage}
	if candidate.BuildPGO.Schema != "go-build-argv/v1" || candidate.Rollback.Schema != "go-build-argv/v1" ||
		!equalStrings(candidate.BuildPGO.Argv, wantPGO) || !equalStrings(candidate.Rollback.Argv, wantRollback) {
		return errors.New("pgoworkflow: build command identity mismatch")
	}
	copy := candidate
	copy.CandidateID = ""
	body, err := canonicalWithoutID(copy)
	if err != nil || hashBytes(body) != candidate.CandidateID {
		return errors.New("pgoworkflow: candidate id mismatch")
	}
	if candidate.Evaluation.Sequence != "ABBA" || candidate.Evaluation.Blocks < 4 || !candidate.Evaluation.RequirePass || candidate.Evaluation.Status != "pending" || len(candidate.Evaluation.Results) != 0 {
		return errors.New("pgoworkflow: unsafe evaluation design")
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func Encode(candidate Candidate) ([]byte, error) {
	if err := Validate(candidate); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(candidate, "", "  ")
	return append(body, '\n'), err
}

func Decode(reader io.Reader) (Candidate, error) {
	body, err := io.ReadAll(io.LimitReader(reader, MaxManifestBytes+1))
	if err != nil || int64(len(body)) > MaxManifestBytes {
		return Candidate{}, errors.New("pgoworkflow: candidate manifest exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var candidate Candidate
	if err := decoder.Decode(&candidate); err != nil {
		return Candidate{}, fmt.Errorf("pgoworkflow: decode candidate: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Candidate{}, errors.New("pgoworkflow: trailing JSON")
	}
	return candidate, Validate(candidate)
}

func canonicalWithoutID(candidate Candidate) ([]byte, error) {
	candidate.CandidateID = ""
	return json.Marshal(candidate)
}

func hashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func validHash(value string) bool {
	return len(value) == 64 && strings.IndexFunc(value, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') }) < 0
}

func safeText(value string, max int) bool {
	return value != "" && len(value) <= max && strings.IndexFunc(value, unicode.IsControl) < 0
}

func safeBase(value string) bool {
	return safeText(value, 200) && filepath.Base(value) == value && value != "."
}

func safeRevision(value string) bool {
	return len(value) >= 7 && len(value) <= 64 && strings.IndexFunc(value, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) < 0
}

func safePackage(value string) bool {
	return safeText(value, 256) && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, " \\:\x00") && !strings.Contains(value, "..")
}
