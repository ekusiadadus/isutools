//go:build !darwin && !linux

package pprofanalyze

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

type unsupportedWorkerState struct{}

func newWorkerPlatformState(WorkerOptions) (workerPlatformState, error) {
	return nil, errors.New("platform has no approved hard memory primitive")
}

func (*unsupportedWorkerState) Configure(*exec.Cmd) {}
func (*unsupportedWorkerState) VerifyStopped(context.Context, *os.Process, profilemodel.WorkerIsolation) (profilemodel.WorkerIsolation, error) {
	return profilemodel.WorkerIsolation{}, ErrHardIsolationUnavailable
}
func (*unsupportedWorkerState) Resume(*os.Process) error { return ErrHardIsolationUnavailable }
func (*unsupportedWorkerState) Kill(*os.Process) error   { return nil }
func (*unsupportedWorkerState) Close() error             { return nil }

func childPrepareIsolation() (profilemodel.WorkerIsolation, error) {
	return profilemodel.WorkerIsolation{}, ErrHardIsolationUnavailable
}

func childSelfStop() error { return ErrHardIsolationUnavailable }
