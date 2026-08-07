//go:build !darwin && !linux

package pprofanalyze

import (
	"context"
	"os"
	"os/exec"

	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

func runParentProtocolTestWorker() int { return 4 }

type parentProtocolTestState struct{}

func (*parentProtocolTestState) Configure(*exec.Cmd) {}
func (*parentProtocolTestState) VerifyStopped(context.Context, *os.Process, profilemodel.WorkerIsolation) (profilemodel.WorkerIsolation, error) {
	return profilemodel.WorkerIsolation{}, ErrHardIsolationUnavailable
}
func (*parentProtocolTestState) Resume(*os.Process) error { return ErrHardIsolationUnavailable }
func (*parentProtocolTestState) Kill(*os.Process) error   { return nil }
func (*parentProtocolTestState) Close() error             { return nil }
