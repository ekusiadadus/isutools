//go:build darwin

package pprofanalyze

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
	"golang.org/x/sys/unix"
)

type darwinParentProtocolState struct{ *darwinWorkerState }

func (s *darwinParentProtocolState) Configure(cmd *exec.Cmd) {
	s.darwinWorkerState.Configure(cmd)
	cmd.Env = append(cmd.Env, "ISUTOOLS_PPROF_PARENT_PROTOCOL_TEST=1")
}

func TestPrepareDarwinIsolationRequiresSetAndReadback(t *testing.T) {
	limit := uint64(768 << 20)
	got, err := prepareDarwinIsolation(limit,
		func(resource int, value *unix.Rlimit) error {
			if resource != unix.RLIMIT_AS || value.Cur != limit || value.Max != limit {
				t.Fatalf("set resource=%d value=%#v", resource, value)
			}
			return nil
		},
		func(resource int, value *unix.Rlimit) error {
			value.Cur, value.Max = limit, limit
			return nil
		},
	)
	if err != nil || got.Mode != profilemodel.IsolationDarwinRLIMIT || !got.HardLimitVerified || got.AddressSpaceMaxBytes != limit {
		t.Fatalf("isolation=%#v err=%v", got, err)
	}
	if _, err := prepareDarwinIsolation(limit, func(int, *unix.Rlimit) error { return errors.New("set") }, func(int, *unix.Rlimit) error { return nil }); err == nil {
		t.Fatal("accepted failed setrlimit")
	}
	if _, err := prepareDarwinIsolation(limit, func(int, *unix.Rlimit) error { return nil }, func(int, *unix.Rlimit) error { return errors.New("get") }); err == nil {
		t.Fatal("accepted failed getrlimit")
	}
	if _, err := prepareDarwinIsolation(limit, func(int, *unix.Rlimit) error { return nil }, func(_ int, value *unix.Rlimit) error {
		value.Cur, value.Max = limit-1, limit
		return nil
	}); err == nil {
		t.Fatal("accepted mismatched RLIMIT_AS readback")
	}
}

func TestDarwinWorkerStateVerifiesActualStoppedChild(t *testing.T) {
	profile, err := os.CreateTemp(t.TempDir(), "profile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Write(encodedProfile(t, syntheticProfile(13), true)); err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = profile.Close() }()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	job := WorkerJob{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: []*os.File{profile}}
	request, err := validateWorkerJob(job)
	if err != nil {
		t.Fatal(err)
	}
	options, err := normalizedWorkerOptions(WorkerOptions{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	state := &darwinParentProtocolState{&darwinWorkerState{addressSpaceMax: defaultAddressSpaceMax}}
	result, err := launchWorkerWithState(context.Background(), executable, job, options, request, state)
	if err != nil {
		t.Fatalf("Darwin parent bootstrap: %v", err)
	}
	if len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 13 || !result.Isolation.StoppedVerified {
		t.Fatalf("result = %#v", result)
	}
}
