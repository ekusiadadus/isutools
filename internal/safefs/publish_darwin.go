//go:build darwin

package safefs

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func publishNoReplace(dirfd int, temp, final string) (bool, error) {
	if err := unix.Linkat(dirfd, temp, dirfd, final, 0); err != nil {
		return false, err
	}
	if err := unix.Unlinkat(dirfd, temp, 0); err != nil {
		return true, fmt.Errorf("final visible but temporary unlink failed: %w", err)
	}
	return true, nil
}

func requireStrongFilesystem(fd int) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return fmt.Errorf("safefs: fstatfs: %w", err)
	}
	name := strings.TrimRight(string(stat.Fstypename[:]), "\x00")
	if name == "apfs" || name == "hfs" {
		return nil
	}
	return fmt.Errorf("%w: darwin filesystem %q", ErrUnsupportedFilesystem, name)
}

// Ordinary fsync is not F_FULLFSYNC and cannot support a durable claim on
// Darwin. Visibility remains atomic, but durability is explicitly unknown.
func platformDurability() Durability { return DurabilityUnknown }
