// Package agentconfig loads the standalone peer's secret-bearing files.
package agentconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

const MaxTargetsBytes = 64 << 10

var (
	ErrUnsafeFile = errors.New("agent config: unsafe file")
	ErrInvalid    = errors.New("agent config: invalid configuration")
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

type Target struct {
	ID            string `json:"id"`
	Driver        string `json:"driver"`
	DSN           string `json:"dsn"`
	ExplainDriver string `json:"explain_driver,omitempty"`
	ExplainDSN    string `json:"explain_dsn,omitempty"`
}

// LoadTargets reads a strict, owner-only regular file. Errors deliberately
// identify only the field and entry number; a DSN is never interpolated.
func LoadTargets(path string) ([]Target, error) {
	file, err := openSecretFile(path, MaxTargetsBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	limited := io.LimitReader(file, MaxTargetsBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: read failed", ErrUnsafeFile)
	}
	if len(data) > MaxTargetsBytes {
		return nil, fmt.Errorf("%w: file exceeds size limit", ErrUnsafeFile)
	}
	var targets []Target
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&targets); err != nil {
		return nil, fmt.Errorf("%w: JSON decode failed", ErrInvalid)
	}
	if err := requireEOF(decoder); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(targets))
	for i, target := range targets {
		switch {
		case !idPattern.MatchString(target.ID):
			return nil, fmt.Errorf("%w: target %d has invalid id", ErrInvalid, i)
		case target.Driver == "" || len(target.Driver) > 64 || target.DSN == "":
			return nil, fmt.Errorf("%w: target %d is incomplete", ErrInvalid, i)
		case (target.ExplainDriver == "") != (target.ExplainDSN == ""):
			return nil, fmt.Errorf("%w: target %d explain credential is incomplete", ErrInvalid, i)
		}
		if _, ok := seen[target.ID]; ok {
			return nil, fmt.Errorf("%w: duplicate target id", ErrInvalid)
		}
		seen[target.ID] = struct{}{}
	}
	return targets, nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON value", ErrInvalid)
	}
	return nil
}

func openSecretFile(path string, maxBytes int64) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: file unavailable", ErrUnsafeFile)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 || before.Size() > maxBytes || !ownedByCurrentUser(before) {
		return nil, fmt.Errorf("%w: regular owner-only file required", ErrUnsafeFile)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open failed", ErrUnsafeFile)
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0o077 != 0 || after.Size() > maxBytes || !ownedByCurrentUser(after) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: file changed during validation", ErrUnsafeFile)
	}
	return file, nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

// AgentID loads or atomically creates the process identity in dataDir.
func AgentID(dataDir string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("%w: data directory is empty", ErrInvalid)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("%w: data directory unavailable", ErrUnsafeFile)
	}
	path := filepath.Join(dataDir, "agent-id")
	if value, err := readAgentID(path); err == nil {
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	value, err := newUUID()
	if err != nil {
		return "", fmt.Errorf("%w: random identity unavailable", ErrInvalid)
	}
	tmp, err := os.CreateTemp(dataDir, ".agent-id-*")
	if err != nil {
		return "", fmt.Errorf("%w: identity create failed", ErrUnsafeFile)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("%w: identity permission failed", ErrUnsafeFile)
	}
	if _, err := io.WriteString(tmp, value+"\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("%w: identity write failed", ErrUnsafeFile)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("%w: identity sync failed", ErrUnsafeFile)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("%w: identity close failed", ErrUnsafeFile)
	}
	if err := os.Rename(tmpName, path); err != nil {
		if value, readErr := readAgentID(path); readErr == nil {
			return value, nil
		}
		return "", fmt.Errorf("%w: identity publish failed", ErrUnsafeFile)
	}
	return value, nil
}

func readAgentID(path string) (string, error) {
	file, err := openSecretFile(path, 128)
	if err != nil {
		if os.IsNotExist(errors.Unwrap(err)) {
			return "", os.ErrNotExist
		}
		// Lstat errors are intentionally wrapped without their path; preserve
		// the not-exist condition separately for the create path.
		if _, lstatErr := os.Lstat(path); os.IsNotExist(lstatErr) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil || len(data) > 128 {
		return "", fmt.Errorf("%w: identity read failed", ErrUnsafeFile)
	}
	value := string(bytes.TrimSpace(data))
	if !uuidPattern.MatchString(value) {
		return "", fmt.Errorf("%w: identity has invalid format", ErrInvalid)
	}
	return value, nil
}

func newUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
