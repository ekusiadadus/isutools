//go:build linux

package pprofanalyze

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

// TestLaunchWorkerLinuxCgroupBootstrap is opt-in because an ordinary test
// process does not own a delegated cgroup. integration/test-linux-cgroup.sh
// creates one without putting the parent test process inside it.
func TestLaunchWorkerLinuxCgroupBootstrap(t *testing.T) {
	root := os.Getenv("ISUTOOLS_PPROF_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("set ISUTOOLS_PPROF_TEST_CGROUP_ROOT to a delegated cgroup v2 directory")
	}
	body := encodedProfile(t, syntheticProfile(13), true)
	path := t.TempDir() + "/profile.pprof"
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := LaunchWorker(context.Background(), executable, WorkerJob{
		Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: []*os.File{profile},
	}, WorkerOptions{CgroupRoot: root, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("LaunchWorker: %v", err)
	}
	if result.ErrorCode != "" || len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 13 {
		t.Fatalf("worker result = %#v", result)
	}
	isolation := result.Isolation
	if isolation.Mode != profilemodel.IsolationLinuxCgroupV2 ||
		isolation.Bootstrap != profilemodel.BootstrapCgroupFDSIGSTOP ||
		!isolation.HardLimitVerified || !isolation.StoppedVerified || !isolation.MembershipVerified ||
		isolation.MemoryMaxBytes != defaultMemoryMax || isolation.AddressSpaceMaxBytes != defaultAddressSpaceMax {
		t.Fatalf("isolation proof = %#v", isolation)
	}
}

func TestLaunchWorkerLinuxCgroupRealRuntimeProfile(t *testing.T) {
	root := os.Getenv("ISUTOOLS_PPROF_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("set ISUTOOLS_PPROF_TEST_CGROUP_ROOT to a delegated cgroup v2 directory")
	}
	var body bytes.Buffer
	if err := pprof.StartCPUProfile(&body); err != nil {
		t.Fatalf("StartCPUProfile: %v", err)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	var value uint64 = 1
	for time.Now().Before(deadline) {
		value = value*6364136223846793005 + 1442695040888963407
	}
	pprof.StopCPUProfile()
	runtime.KeepAlive(value)
	if body.Len() == 0 {
		t.Fatal("runtime CPU profile is empty")
	}
	path := t.TempDir() + "/runtime-cpu.pprof"
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := LaunchWorker(context.Background(), executable, WorkerJob{
		Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: []*os.File{profile},
	}, WorkerOptions{CgroupRoot: root, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("LaunchWorker real profile: %v", err)
	}
	if result.ErrorCode != "" || len(result.Summaries) == 0 {
		t.Fatalf("runtime profile result = %#v", result)
	}
	positive := false
	for _, summary := range result.Summaries {
		positive = positive || summary.PositiveTotal > 0
	}
	if !positive {
		t.Fatalf("runtime profile has no positive samples: %#v", result.Summaries)
	}
	if !result.Isolation.HardLimitVerified || !result.Isolation.StoppedVerified || !result.Isolation.MembershipVerified {
		t.Fatalf("runtime profile isolation proof = %#v", result.Isolation)
	}
}

func TestLaunchWorkerLinuxCgroupOOMLeavesParentUsable(t *testing.T) {
	root := os.Getenv("ISUTOOLS_PPROF_TEST_CGROUP_ROOT")
	helper := os.Getenv("ISUTOOLS_PPROF_TEST_OOM_HELPER")
	if root == "" || helper == "" {
		t.Skip("set delegated cgroup root and OOM helper for the Linux integration fixture")
	}
	body := encodedProfile(t, syntheticProfile(17), true)
	path := t.TempDir() + "/profile.pprof"
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer profile.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	job := WorkerJob{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: []*os.File{profile}}
	_, err = LaunchWorker(context.Background(), helper, job, WorkerOptions{
		CgroupRoot: root, MemoryMaxBytes: 128 << 20, AddressSpaceMaxBytes: 1 << 30, Timeout: 10 * time.Second,
	})
	if err == nil || errors.Is(err, ErrHardIsolationUnavailable) || !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("OOM worker error = %v", err)
	}
	if offset, seekErr := profile.Seek(0, io.SeekCurrent); seekErr != nil || offset != 0 {
		t.Fatalf("OOM worker consumed profile descriptor: offset=%d err=%v", offset, seekErr)
	}

	// Prove that the parent and delegated root survived by immediately running
	// a normal analysis under a newly-created child cgroup.
	result, err := LaunchWorker(context.Background(), executable, job, WorkerOptions{CgroupRoot: root, Timeout: 10 * time.Second})
	if err != nil || result.ErrorCode != "" || len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 17 {
		t.Fatalf("post-OOM worker result=%#v err=%v", result, err)
	}
}
