package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiteralLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:19192"} {
		if !literalLoopback(addr) {
			t.Fatalf("literalLoopback(%q) = false", addr)
		}
	}
	for _, addr := range []string{"localhost:1", "0.0.0.0:1", ":1"} {
		if literalLoopback(addr) {
			t.Fatalf("literalLoopback(%q) = true", addr)
		}
	}
}

func TestLoadAndRegisterTargetsDoesNotReturnDSN(t *testing.T) {
	secret := "user:super-secret@tcp(127.0.0.1:3306)/isuconp"
	path := filepath.Join(t.TempDir(), "targets.json")
	if err := os.WriteFile(path, []byte(`[{"id":"agent-test-db","driver":"mysql","dsn":"`+secret+`"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := loadAndRegisterTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].ID != "agent-test-db" {
		t.Fatalf("targets = %+v", result)
	}
	if strings.Contains(result[0].Display, "super-secret") {
		t.Fatal("PeerInfo display disclosed password")
	}

	bad := filepath.Join(t.TempDir(), "unsafe.json")
	if err := os.WriteFile(bad, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = loadAndRegisterTargets(bad)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("unsafe error = %v", err)
	}
}
