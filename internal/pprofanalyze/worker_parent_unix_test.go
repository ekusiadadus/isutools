//go:build darwin || linux

package pprofanalyze

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

var parentProtocolTestIsolation = profilemodel.WorkerIsolation{
	Mode: profilemodel.IsolationDarwinRLIMIT, Bootstrap: profilemodel.BootstrapRLIMITSIGSTOP,
	AddressSpaceMaxBytes: defaultAddressSpaceMax, HardLimitVerified: true, StoppedVerified: true,
}

type parentProtocolTestState struct{ closeErr error }

func (*parentProtocolTestState) Configure(cmd *exec.Cmd) {
	cmd.Env = append(cmd.Env, "ISUTOOLS_PPROF_PARENT_PROTOCOL_TEST=1")
}

func (*parentProtocolTestState) VerifyStopped(ctx context.Context, process *os.Process, reported profilemodel.WorkerIsolation) (profilemodel.WorkerIsolation, error) {
	if err := waitForSIGSTOP(ctx, process.Pid); err != nil {
		return profilemodel.WorkerIsolation{}, fmt.Errorf("test worker was not stopped: %w", err)
	}
	if reported.StoppedVerified {
		return profilemodel.WorkerIsolation{}, errors.New("test worker claimed stop before observation")
	}
	return parentProtocolTestIsolation, nil
}

func (*parentProtocolTestState) Resume(process *os.Process) error {
	return process.Signal(syscall.SIGCONT)
}
func (*parentProtocolTestState) Kill(process *os.Process) error {
	if process == nil {
		return nil
	}
	err := process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
func (s *parentProtocolTestState) Close() error { return s.closeErr }

func runParentProtocolTestWorker() int {
	ready := os.NewFile(workerReadyFD, "test-ready")
	result := os.NewFile(workerResultFD, "test-result")
	if ready == nil || result == nil {
		return 4
	}
	reported := parentProtocolTestIsolation
	reported.StoppedVerified = false
	if json.NewEncoder(ready).Encode(workerReady{PID: os.Getpid(), Isolation: reported}) != nil {
		return 4
	}
	_ = ready.Close()
	if err := childSelfStop(); err != nil {
		return 4
	}
	control := os.NewFile(workerControlFD, "test-control")
	command, err := bufio.NewReader(io.LimitReader(control, 16)).ReadString('\n')
	_ = control.Close()
	if err != nil || command != "START\n" {
		return 4
	}
	requestFile := os.NewFile(workerRequestFD, "test-request")
	requestBody, err := io.ReadAll(io.LimitReader(requestFile, workerMaxRequestBytes+1))
	if err != nil {
		return 4
	}
	_ = requestFile.Close()
	var request workerRequest
	if err := decodeStrictJSON(requestBody, &request); err != nil {
		return 4
	}
	response := executeWorkerRequest(request, parentProtocolTestIsolation, newVerifiedIsolationProof(), func(index int) *os.File {
		return os.NewFile(uintptr(workerFirstProfileFD+index), "test-profile")
	})
	if json.NewEncoder(result).Encode(response) != nil {
		return 4
	}
	_ = result.Close()
	return 0
}

func TestLaunchWorkerParentProtocolSuccess(t *testing.T) {
	profile, err := os.CreateTemp(t.TempDir(), "profile")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Write(encodedProfile(t, syntheticProfile(7), true)); err != nil {
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
	result, err := launchWorkerWithState(context.Background(), executable, job, options, request, &parentProtocolTestState{})
	if err != nil {
		t.Fatalf("launchWorkerWithState: %v", err)
	}
	if result.Isolation != parentProtocolTestIsolation || len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 7 {
		t.Fatalf("result = %#v", result)
	}

	if _, err := profile.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("worker cgroup cleanup failed")
	result, err = launchWorkerWithState(context.Background(), executable, job, options, request, &parentProtocolTestState{closeErr: cleanupErr})
	if !errors.Is(err, cleanupErr) || len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 7 {
		t.Fatalf("cleanup result=%#v err=%v", result, err)
	}
}
