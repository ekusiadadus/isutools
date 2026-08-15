package analysisartifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

const maxSnapshotBytes = int64(32 << 20)

// VerifyRunBinding proves that the immutable snapshot named by a run-bound
// artifact still has the exact ledger hash, schema, and run identity.
func VerifyRunBinding(root *safefs.Root, binding RunBinding) error {
	if root == nil {
		return errors.New("analysisartifact: snapshot root is unavailable")
	}
	if err := validateRun(&binding); err != nil {
		return err
	}
	body, err := root.ReadFile(binding.SnapshotBase+".json", maxSnapshotBytes)
	if err != nil {
		return fmt.Errorf("analysisartifact: read bound snapshot: %w", err)
	}
	if hashBytes(body) != binding.SnapshotSHA256 {
		return errors.New("analysisartifact: snapshot sha256 mismatch")
	}
	var snapshot struct {
		Meta struct {
			SchemaVersion int `json:"schema_version"`
			Run           *struct {
				RunID string `json:"run_id"`
			} `json:"run"`
		} `json:"meta"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&snapshot); err != nil {
		return errors.New("analysisartifact: bound snapshot is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("analysisartifact: bound snapshot has trailing JSON")
	}
	if snapshot.Meta.Run == nil || snapshot.Meta.Run.RunID != binding.RunID || snapshot.Meta.SchemaVersion != binding.SnapshotSchemaVersion {
		return errors.New("analysisartifact: snapshot run binding mismatch")
	}
	return nil
}

type Inspection struct {
	Schema     string `json:"schema"`
	Kind       string `json:"kind,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
	Status     string `json:"status"`
	Code       string `json:"code,omitempty"`
}

// Inspect is the forward-compatible display path. Decode remains strict for
// trusted use; an unknown schema is visible as unsupported, never as corrupt.
func Inspect(reader io.Reader) (Inspection, error) {
	body, err := io.ReadAll(io.LimitReader(reader, MaxManifestBytes+1))
	if err != nil || int64(len(body)) > MaxManifestBytes {
		return Inspection{}, errors.New("analysisartifact: manifest exceeds display limit")
	}
	var header struct {
		Schema     string `json:"schema"`
		Kind       string `json:"kind"`
		ArtifactID string `json:"artifact_id"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(body, &header); err != nil || header.Schema == "" {
		return Inspection{}, errors.New("analysisartifact: invalid manifest header")
	}
	if header.Schema != SchemaV1 {
		return Inspection{Schema: header.Schema, Kind: boundedDisplay(header.Kind), ArtifactID: safeDisplayHash(header.ArtifactID), Status: StatusUnsupported, Code: "unknown-schema"}, nil
	}
	manifest, err := Decode(bytes.NewReader(body))
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{Schema: manifest.Schema, Kind: manifest.Kind, ArtifactID: manifest.ArtifactID, Status: manifest.Status}, nil
}

func boundedDisplay(value string) string {
	if validToken(value, 64) {
		return value
	}
	return ""
}

func safeDisplayHash(value string) string {
	if validHash(value) {
		return value
	}
	return ""
}
