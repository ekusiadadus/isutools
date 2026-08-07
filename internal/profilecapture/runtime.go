package profilecapture

import (
	"io"
	"runtime/pprof"
)

// RuntimeBackend is the only production adapter that calls the process-wide
// runtime CPU profiler. Ownership and liveness remain in Coordinator.
type RuntimeBackend struct{}

func (RuntimeBackend) StartCPUProfile(writer io.Writer) error { return pprof.StartCPUProfile(writer) }
func (RuntimeBackend) StopCPUProfile()                        { pprof.StopCPUProfile() }
