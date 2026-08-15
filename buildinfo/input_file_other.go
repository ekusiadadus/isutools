//go:build !darwin && !linux

package buildinfo

import "os"

func openInputFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	return os.Open(path)
}

// Portable targets lack the descriptor-relative hard-link guarantees required
// for strong publication. Keep the regular-file check and report the weaker
// platform semantics through safefs capability detection.
func hasSingleLink(os.FileInfo) bool { return true }
