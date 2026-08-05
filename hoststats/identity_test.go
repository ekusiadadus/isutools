package hoststats

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestReadIdentity(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, testEnv{EnvRole: " app "}, newClock(fixtureTime))
	id := c.readIdentity()

	sum := sha256.Sum256([]byte("0123456789abcdef0123456789abcdef"))
	if want := hex.EncodeToString(sum[:])[:idHashLen]; id.MachineIDHash != want {
		t.Fatalf("machine id hash = %q, want %q", id.MachineIDHash, want)
	}
	if id.BootIDHash == "" || id.BootIDHash == id.MachineIDHash {
		t.Fatalf("boot id hash = %q, want a distinct hash", id.BootIDHash)
	}
	if id.Hostname != "isu1" {
		t.Fatalf("hostname = %q, want isu1", id.Hostname)
	}
	if id.PIDNS != "pid:[4026531836]" || id.CgroupNS != "cgroup:[4026531835]" {
		t.Fatalf("namespaces = %+v, want the link targets verbatim", id)
	}
	if id.NetNS == "" || id.MntNS == "" {
		t.Fatalf("namespaces = %+v, want all four", id)
	}
	if id.Role != "app" {
		t.Fatalf("role = %q, want the trimmed value", id.Role)
	}
	if id.AgentVersion == "" {
		t.Fatal("agent version = empty, want the build revision")
	}
}

func TestReadIdentityDegrades(t *testing.T) {
	t.Parallel()
	opt := testOptions(testEnv{}, newClock(fixtureTime))
	procFS := newProcFS()
	delete(procFS, pathBootID)
	opt.ProcFS = procFS
	opt.EtcFS = fstest.MapFS{}
	opt.Readlink = func(string) (string, error) { return "", fs.ErrPermission }
	opt.Hostname = func() (string, error) { return "", fs.ErrPermission }

	c, err := New(opt)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	id := c.readIdentity()

	// Identity is diagnostic context: every field degrades on its own and
	// none of them may cost the caller a host section.
	if id.Hostname != "" || id.MachineIDHash != "" || id.BootIDHash != "" {
		t.Fatalf("identity = %+v, want empty fields rather than errors", id)
	}
	if id.PIDNS != "" || id.NetNS != "" || id.MntNS != "" || id.CgroupNS != "" {
		t.Fatalf("namespaces = %+v, want empty when readlink is denied", id)
	}
	if id.AgentVersion == "" {
		t.Fatal("agent version must survive an unreadable host")
	}

	c.opt.Readlink = nil
	if got := c.namespace("pid"); got != "" {
		t.Fatalf("namespace() = %q, want empty without a readlink seam", got)
	}
}

func TestHashIDFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		fsys  fs.FS
		empty bool
	}{
		{name: "missing file", fsys: fstest.MapFS{}, empty: true},
		{name: "nil filesystem", fsys: nil, empty: true},
		{name: "blank content", fsys: fstest.MapFS{pathMachineID: mapFile("\n")}, empty: true},
		{name: "value", fsys: fstest.MapFS{pathMachineID: mapFile("abc\n")}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hashIDFile(tt.fsys, pathMachineID)
			if tt.empty {
				if got != "" {
					t.Fatalf("hashIDFile() = %q, want empty", got)
				}
				return
			}
			if len(got) != idHashLen {
				t.Fatalf("hashIDFile() = %q, want %d characters", got, idHashLen)
			}
		})
	}
}
