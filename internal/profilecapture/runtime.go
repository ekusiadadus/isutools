package profilecapture

import (
	"errors"
	"io"
	"runtime/pprof"

	"github.com/ekusiadadus/isutools/internal/profileowner"
)

// RuntimeBackend is the only production adapter that calls the process-wide
// runtime CPU profiler. Ownership and liveness remain in Coordinator.
type RuntimeBackend struct{}

func (RuntimeBackend) StartCPUProfile(writer io.Writer) error {
	if !profileowner.Default.Acquire("managed-cpu") {
		return errors.New("runtime profiler is busy")
	}
	if err := pprof.StartCPUProfile(writer); err != nil {
		profileowner.Default.Release("managed-cpu")
		return err
	}
	return nil
}

func (RuntimeBackend) StopCPUProfile() {
	pprof.StopCPUProfile()
	profileowner.Default.Release("managed-cpu")
}
