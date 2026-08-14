package agentconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecret(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "targets.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadTargetsStrictAndSecretFree(t *testing.T) {
	secret := "secret-user:secret-pass@tcp(127.0.0.1:3306)/isuconp"
	path := writeSecret(t, `[{"id":"db","driver":"mysql","dsn":"`+secret+`"}]`, 0o600)
	targets, err := LoadTargets(path)
	if err != nil || len(targets) != 1 || targets[0].DSN != secret {
		t.Fatalf("LoadTargets = (%+v, %v)", targets, err)
	}

	bad := writeSecret(t, `[{"id":"db","driver":"mysql","dsn":"`+secret+`","unknown":true}]`, 0o600)
	_, err = LoadTargets(bad)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("strict load error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("decode error disclosed DSN")
	}
}

func TestLoadTargetsRejectsUnsafeFiles(t *testing.T) {
	world := writeSecret(t, `[]`, 0o644)
	if _, err := LoadTargets(world); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("world-readable error = %v", err)
	}
	target := writeSecret(t, `[]`, 0o600)
	link := filepath.Join(t.TempDir(), "targets-link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTargets(link); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestAgentIDPersistsCanonicalUUID(t *testing.T) {
	dir := t.TempDir()
	first, err := AgentID(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AgentID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !uuidPattern.MatchString(first) {
		t.Fatalf("agent ids = %q, %q", first, second)
	}
	info, err := os.Stat(filepath.Join(dir, "agent-id"))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("agent-id mode = %v err=%v", info.Mode(), err)
	}
}
