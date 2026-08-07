//go:build linux

package pprofanalyze

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
	"golang.org/x/sys/unix"
)

type linuxWorkerState struct {
	rootFD          int
	childFD         int
	childName       string
	pidfd           int
	memoryMax       uint64
	addressSpaceMax uint64
}

func newWorkerPlatformState(options WorkerOptions) (workerPlatformState, error) {
	if options.CgroupRoot == "" || !filepath.IsAbs(options.CgroupRoot) {
		return nil, errors.New("an absolute delegated cgroup v2 root is required")
	}
	rootFD, err := unix.Open(options.CgroupRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open delegated cgroup root: %w", err)
	}
	state := &linuxWorkerState{
		rootFD: rootFD, childFD: -1, pidfd: -1,
		memoryMax: options.MemoryMaxBytes, addressSpaceMax: options.AddressSpaceMaxBytes,
	}
	fail := func(err error) (workerPlatformState, error) {
		_ = state.Close()
		return nil, err
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(rootFD, &stat); err != nil {
		return fail(fmt.Errorf("stat delegated cgroup root: %w", err))
	}
	if uint64(stat.Type) != uint64(unix.CGROUP2_SUPER_MAGIC) {
		return fail(errors.New("delegated root is not cgroup v2"))
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fail(fmt.Errorf("create cgroup nonce: %w", err))
	}
	state.childName = "isutools-pprof-" + hex.EncodeToString(nonce[:])
	if err := unix.Mkdirat(rootFD, state.childName, 0o700); err != nil {
		return fail(fmt.Errorf("create worker cgroup: %w", err))
	}
	childFD, err := unix.Openat(rootFD, state.childName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fail(fmt.Errorf("open worker cgroup: %w", err))
	}
	state.childFD = childFD
	settings := map[string]string{
		"memory.max":       strconv.FormatUint(options.MemoryMaxBytes, 10),
		"memory.swap.max":  "0",
		"pids.max":         "32",
		"memory.oom.group": "1",
	}
	for name, value := range settings {
		if err := writeCgroupValue(childFD, name, value); err != nil {
			return fail(err)
		}
		got, err := readCgroupValue(childFD, name, 128)
		if err != nil || got != value {
			return fail(fmt.Errorf("cgroup %s read-back mismatch", name))
		}
	}
	return state, nil
}

func (s *linuxWorkerState) Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    s.childFD,
		PidFD:       &s.pidfd,
		Pdeathsig:   syscall.SIGKILL,
	}
}

func (s *linuxWorkerState) VerifyStopped(ctx context.Context, process *os.Process, reported profilemodel.WorkerIsolation) (profilemodel.WorkerIsolation, error) {
	if err := waitForSIGSTOP(ctx, process.Pid); err != nil {
		return profilemodel.WorkerIsolation{}, err
	}
	if s.pidfd < 0 {
		return profilemodel.WorkerIsolation{}, errors.New("clone did not return a pidfd")
	}
	if reported.Mode != profilemodel.IsolationLinuxCgroupV2 || reported.Bootstrap != profilemodel.BootstrapCgroupFDSIGSTOP ||
		reported.MemoryMaxBytes != s.memoryMax || reported.AddressSpaceMaxBytes != s.addressSpaceMax ||
		reported.HardLimitVerified || reported.StoppedVerified || reported.MembershipVerified {
		return profilemodel.WorkerIsolation{}, errors.New("worker reported an invalid Linux bootstrap")
	}
	want := unix.Rlimit{Cur: s.addressSpaceMax, Max: s.addressSpaceMax}
	if err := unix.Prlimit(process.Pid, unix.RLIMIT_AS, &want, nil); err != nil {
		return profilemodel.WorkerIsolation{}, fmt.Errorf("set worker RLIMIT_AS: %w", err)
	}
	var got unix.Rlimit
	if err := unix.Prlimit(process.Pid, unix.RLIMIT_AS, nil, &got); err != nil || got.Cur != want.Cur || got.Max != want.Max {
		return profilemodel.WorkerIsolation{}, errors.New("worker RLIMIT_AS read-back mismatch")
	}
	for name, value := range map[string]string{
		"memory.max": strconv.FormatUint(s.memoryMax, 10), "memory.swap.max": "0", "pids.max": "32",
	} {
		got, err := readCgroupValue(s.childFD, name, 128)
		if err != nil || got != value {
			return profilemodel.WorkerIsolation{}, fmt.Errorf("worker cgroup %s changed before release", name)
		}
	}
	procs, err := readCgroupValue(s.childFD, "cgroup.procs", 64<<10)
	if err != nil || !lineSetContains(procs, strconv.Itoa(process.Pid)) {
		return profilemodel.WorkerIsolation{}, errors.New("worker cgroup membership was not verified")
	}
	reported.HardLimitVerified = true
	reported.StoppedVerified = true
	reported.MembershipVerified = true
	return reported, nil
}

func (s *linuxWorkerState) Resume(*os.Process) error {
	if s.pidfd < 0 {
		return errors.New("pidfd is unavailable")
	}
	return unix.PidfdSendSignal(s.pidfd, unix.SIGCONT, nil, 0)
}

func (s *linuxWorkerState) Kill(process *os.Process) error {
	if s.pidfd >= 0 {
		err := unix.PidfdSendSignal(s.pidfd, unix.SIGKILL, nil, 0)
		if err == nil || errors.Is(err, unix.ESRCH) {
			return nil
		}
		return err
	}
	if process == nil {
		return nil
	}
	err := process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (s *linuxWorkerState) Close() error {
	var errs []error
	if s.pidfd >= 0 {
		errs = append(errs, unix.Close(s.pidfd))
		s.pidfd = -1
	}
	if s.childFD >= 0 {
		errs = append(errs, unix.Close(s.childFD))
		s.childFD = -1
	}
	if s.rootFD >= 0 && s.childName != "" {
		errs = append(errs, unix.Unlinkat(s.rootFD, s.childName, unix.AT_REMOVEDIR))
		s.childName = ""
	}
	if s.rootFD >= 0 {
		errs = append(errs, unix.Close(s.rootFD))
		s.rootFD = -1
	}
	return errors.Join(errs...)
}

func childPrepareIsolation() (profilemodel.WorkerIsolation, error) {
	memoryMax, err := workerLimitFromEnv("ISUTOOLS_PPROF_WORKER_MEMORY_MAX", defaultMemoryMax)
	if err != nil {
		return profilemodel.WorkerIsolation{}, err
	}
	addressSpaceMax, err := workerLimitFromEnv("ISUTOOLS_PPROF_WORKER_AS_MAX", defaultAddressSpaceMax)
	if err != nil {
		return profilemodel.WorkerIsolation{}, err
	}
	return profilemodel.WorkerIsolation{
		Mode: profilemodel.IsolationLinuxCgroupV2, Bootstrap: profilemodel.BootstrapCgroupFDSIGSTOP,
		MemoryMaxBytes: memoryMax, AddressSpaceMaxBytes: addressSpaceMax,
	}, nil
}

func childSelfStop() error {
	return unix.Kill(unix.Getpid(), unix.SIGSTOP)
}

func waitStatusIsSIGSTOP(status unix.WaitStatus) bool {
	return status.Stopped() && status.StopSignal() == unix.SIGSTOP
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

func writeCgroupValue(dirfd int, name, value string) error {
	fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open cgroup %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	_, writeErr := io.WriteString(file, value)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return fmt.Errorf("write cgroup %s: %w", name, errors.Join(writeErr, closeErr))
	}
	return nil
}

func readCgroupValue(dirfd int, name string, limit int64) (string, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), name)
	body, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return "", errors.Join(readErr, closeErr)
	}
	if int64(len(body)) > limit {
		return "", errors.New("cgroup value exceeds read limit")
	}
	return strings.TrimSpace(string(body)), nil
}

func lineSetContains(value, want string) bool {
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
