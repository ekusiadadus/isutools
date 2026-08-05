//go:build linux

package hoststats

import (
	"fmt"
	"syscall"
)

// osStatfs reports a filesystem's size through statfs(2). It uses the
// available-to-unprivileged count rather than the free count, because the
// reserved blocks a database cannot use are not space it has.
func osStatfs(path string) (FSRaw, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSRaw{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	blockSize := uint64(st.Bsize)
	return FSRaw{
		TotalBytes: mulSaturate(uint64(st.Blocks), blockSize),
		AvailBytes: mulSaturate(uint64(st.Bavail), blockSize),
	}, nil
}
