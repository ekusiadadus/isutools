//go:build darwin || linux

package buildinfo

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openInputFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "analysis-binary")
	if file == nil {
		_ = unix.Close(fd)
		return nil, syscall.EBADF
	}
	return file, nil
}

func hasSingleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
