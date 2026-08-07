//go:build darwin

package pprofanalyze

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
	"golang.org/x/sys/unix"
)

type darwinWorkerState struct {
	addressSpaceMax uint64
}

func newWorkerPlatformState(options WorkerOptions) (workerPlatformState, error) {
	return &darwinWorkerState{addressSpaceMax: options.AddressSpaceMaxBytes}, nil
}

func (s *darwinWorkerState) Configure(*exec.Cmd) {}

func (s *darwinWorkerState) VerifyStopped(ctx context.Context, process *os.Process, reported profilemodel.WorkerIsolation) (profilemodel.WorkerIsolation, error) {
	if err := waitForSIGSTOP(ctx, process.Pid); err != nil {
		return profilemodel.WorkerIsolation{}, err
	}
	if reported.Mode != profilemodel.IsolationDarwinRLIMIT || reported.Bootstrap != profilemodel.BootstrapRLIMITSIGSTOP ||
		reported.AddressSpaceMaxBytes != s.addressSpaceMax || !reported.HardLimitVerified || reported.StoppedVerified || reported.MembershipVerified {
		return profilemodel.WorkerIsolation{}, errors.New("worker reported an invalid Darwin hard-limit bootstrap")
	}
	reported.StoppedVerified = true
	return reported, nil
}

func waitForSIGSTOP(ctx context.Context, pid int) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var status unix.WaitStatus
		got, err := unix.Wait4(pid, &status, unix.WUNTRACED|unix.WNOHANG, nil)
		if err != nil {
			return fmt.Errorf("wait for worker SIGSTOP: %w", err)
		}
		if got == pid {
			if waitStatusIsSIGSTOP(status) {
				return nil
			}
			return errors.New("worker exited or changed state before SIGSTOP")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *darwinWorkerState) Resume(process *os.Process) error {
	return process.Signal(syscall.SIGCONT)
}

func (s *darwinWorkerState) Kill(process *os.Process) error {
	if process == nil {
		return nil
	}
	err := process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (s *darwinWorkerState) Close() error { return nil }

func childPrepareIsolation() (profilemodel.WorkerIsolation, error) {
	limit, err := workerLimitFromEnv("ISUTOOLS_PPROF_WORKER_AS_MAX", defaultAddressSpaceMax)
	if err != nil {
		return profilemodel.WorkerIsolation{}, err
	}
	return prepareDarwinIsolation(limit, unix.Setrlimit, unix.Getrlimit)
}

func prepareDarwinIsolation(limit uint64, set func(int, *unix.Rlimit) error, get func(int, *unix.Rlimit) error) (profilemodel.WorkerIsolation, error) {
	want := unix.Rlimit{Cur: limit, Max: limit}
	if err := set(unix.RLIMIT_AS, &want); err != nil {
		return profilemodel.WorkerIsolation{}, err
	}
	var got unix.Rlimit
	if err := get(unix.RLIMIT_AS, &got); err != nil {
		return profilemodel.WorkerIsolation{}, err
	}
	if got.Cur != limit || got.Max != limit {
		return profilemodel.WorkerIsolation{}, errors.New("RLIMIT_AS read-back mismatch")
	}
	return profilemodel.WorkerIsolation{
		Mode: profilemodel.IsolationDarwinRLIMIT, Bootstrap: profilemodel.BootstrapRLIMITSIGSTOP,
		AddressSpaceMaxBytes: limit, HardLimitVerified: true,
	}, nil
}

func childSelfStop() error {
	return unix.Kill(unix.Getpid(), unix.SIGSTOP)
}

func waitStatusIsSIGSTOP(status unix.WaitStatus) bool {
	// Darwin uses 0x7f in the low byte for a stopped child. The Go syscall
	// helper classifies the raw SIGSTOP code as Continued, so inspect the
	// kernel wait status directly instead of relying on WaitStatus.Stopped.
	return uint32(status)&0x7f == 0x7f && (uint32(status)>>8)&0xff == uint32(unix.SIGSTOP)
}
