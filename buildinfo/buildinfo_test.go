package buildinfo

import (
	"runtime/debug"
	"testing"
)

func biWith(settings map[string]string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		bi := &debug.BuildInfo{}
		for k, v := range settings {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: k, Value: v})
		}
		return bi, true
	}
}

func noBI() (*debug.BuildInfo, bool) { return nil, false }

func envWith(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func noEnv(string) string { return "" }

func TestGetFromVCS(t *testing.T) {
	info := get(biWith(map[string]string{
		"vcs.revision": "f4fdb31aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"vcs.modified": "true",
	}), noEnv)
	if info.Source != "vcs" {
		t.Errorf("source = %q, want vcs", info.Source)
	}
	if !info.Dirty {
		t.Error("want dirty")
	}
	if got := info.Short(); got != "f4fdb31 (dirty)" {
		t.Errorf("Short() = %q, want %q", got, "f4fdb31 (dirty)")
	}
}

func TestGetFromVCSClean(t *testing.T) {
	info := get(biWith(map[string]string{
		"vcs.revision": "abc1234def",
		"vcs.modified": "false",
	}), noEnv)
	if got := info.Short(); got != "abc1234" {
		t.Errorf("Short() = %q, want %q", got, "abc1234")
	}
}

func TestGetFallbackToLdflags(t *testing.T) {
	oldRev, oldDirty := Revision, DirtyFlag
	t.Cleanup(func() { Revision, DirtyFlag = oldRev, oldDirty })
	Revision, DirtyFlag = "1234567890", "dirty"

	info := get(noBI, noEnv)
	if info.Source != "ldflags" {
		t.Errorf("source = %q, want ldflags", info.Source)
	}
	if got := info.Short(); got != "1234567 (dirty)" {
		t.Errorf("Short() = %q", got)
	}
}

func TestGetFallbackToEnv(t *testing.T) {
	info := get(noBI, envWith(map[string]string{"ISUTOOLS_GIT_HASH": "cafebabe99"}))
	if info.Source != "env" {
		t.Errorf("source = %q, want env", info.Source)
	}
	if got := info.Short(); got != "cafebab" {
		t.Errorf("Short() = %q", got)
	}
}

func TestGetUnknown(t *testing.T) {
	info := get(noBI, noEnv)
	if info.Source != "unknown" {
		t.Errorf("source = %q, want unknown", info.Source)
	}
	if got := info.Short(); got != "unknown" {
		t.Errorf("Short() = %q", got)
	}
}
