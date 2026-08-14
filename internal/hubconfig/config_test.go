package hubconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStrictSecretFree(t *testing.T) {
	token := strings.Repeat("secret-token-", 3)
	path := filepath.Join(t.TempDir(), "peers.json")
	body := `[{"name":"app","endpoint":"http://127.0.0.1:19192","token":"` + token + `","required":true}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	peers, err := Load(path)
	if err != nil || len(peers) != 1 || peers[0].Token != token {
		t.Fatalf("Load = (%+v, %v)", peers, err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSuffix(body, "]")+`,"unknown":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if !errors.Is(err, ErrRejected) || strings.Contains(err.Error(), token) {
		t.Fatalf("strict error = %v", err)
	}
}

func TestLoadRejectsWorldReadableAndSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peers.json")
	if err := os.WriteFile(path, []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, ErrRejected) {
		t.Fatalf("mode error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); !errors.Is(err, ErrRejected) {
		t.Fatalf("symlink error = %v", err)
	}
}
