//go:build !linux

package hoststats

import "fmt"

// osStatfs reports that filesystem usage cannot be read here. Everything in
// this package targets Linux; the non-Linux build exists so the parsers and
// the interval logic can be developed and tested on any machine.
func osStatfs(path string) (FSRaw, error) {
	return FSRaw{}, fmt.Errorf("%w: statfs %s", ErrUnsupportedOS, path)
}
