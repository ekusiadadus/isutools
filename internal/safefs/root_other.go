//go:build !darwin && !linux

package safefs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type rootPlatform struct{ portable *os.Root }

func Open(path string, options Options) (*Root, error) {
	if options.RequireStrongVisibility || options.Exclusive {
		return nil, ErrUnsupportedFilesystem
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("safefs: open data root: %w", err)
	}
	return &Root{rootPlatform: rootPlatform{portable: root}}, nil
}

func (r *Root) Close() error {
	if r == nil || r.portable == nil {
		return nil
	}
	err := r.portable.Close()
	r.portable = nil
	return err
}

func (r *Root) CreateExclusive(name string, mode os.FileMode) (*os.File, error) {
	if err := validatePortableName(name); err != nil {
		return nil, err
	}
	if mode.Perm()&0o077 != 0 {
		return nil, fmt.Errorf("safefs: create mode %o is not private", mode.Perm())
	}
	file, err := r.portable.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, os.ErrExist) {
		return nil, ErrExists
	}
	return file, err
}

func (r *Root) OpenRegular(name string) (*os.File, error) {
	if err := validatePortableName(name); err != nil {
		return nil, err
	}
	file, err := r.portable.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrNotRegular
	}
	return file, nil
}

func (r *Root) ReadFile(name string, limit int64) ([]byte, error) {
	file, err := r.OpenRegular(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrTooLarge
	}
	return body, nil
}

func (r *Root) ReadDir() ([]os.DirEntry, error) {
	dir, err := r.portable.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

func (r *Root) AvailableBytes() (uint64, error) { return 0, ErrUnsupportedFilesystem }

func (r *Root) PublicationDurability() Durability { return DurabilityUnknown }

func (r *Root) Remove(name string) error {
	if err := validatePortableName(name); err != nil {
		return err
	}
	return r.portable.Remove(name)
}

func (r *Root) RemoveTemp(name string) error {
	if err := validatePortableName(name); err != nil {
		return err
	}
	return r.portable.Remove(name)
}

func (r *Root) PublishNoReplace(string, string) (Publication, error) {
	return Publication{}, ErrUnsupportedFilesystem
}

func (r *Root) Replace(string, string) (Publication, error) {
	return Publication{}, ErrUnsupportedFilesystem
}

func (r *Root) TryLock(string) (*Lock, error) {
	return nil, ErrUnsupportedFilesystem
}

func validatePortableName(name string) error {
	if validateCommonName(name) != nil || filepath.Base(name) != name {
		return ErrInvalidName
	}
	return nil
}
