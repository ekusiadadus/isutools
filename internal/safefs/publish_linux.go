//go:build linux

package safefs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func publishNoReplace(dirfd int, temp, final string) (bool, error) {
	if err := unix.Renameat2(dirfd, temp, dirfd, final, unix.RENAME_NOREPLACE); err != nil {
		return false, err
	}
	return true, nil
}

func requireStrongFilesystem(fd int) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return fmt.Errorf("safefs: fstatfs: %w", err)
	}
	switch uint64(stat.Type) {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC:
		return nil
	default:
		return fmt.Errorf("%w: linux f_type %#x", ErrUnsupportedFilesystem, uint64(stat.Type))
	}
}

func platformDurability() Durability { return DurabilityDurable }
