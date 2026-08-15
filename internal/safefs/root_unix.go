//go:build darwin || linux

package safefs

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type rootPlatform struct{ dir *os.File }

func Open(path string, options Options) (*Root, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("safefs: open data directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), path)
	root := &Root{rootPlatform: rootPlatform{dir: dir}}
	fail := func(err error) (*Root, error) {
		_ = dir.Close()
		return nil, err
	}
	if options.RequireStrongVisibility {
		if err := requireStrongFilesystem(fd); err != nil {
			return fail(err)
		}
	}
	if options.Exclusive {
		if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			if errors.Is(err, unix.EWOULDBLOCK) {
				return fail(ErrLocked)
			}
			return fail(fmt.Errorf("safefs: lock data directory: %w", err))
		}
		root.exclusive = true
	}
	return root, nil
}

func (r *Root) Close() error {
	if r == nil || r.dir == nil {
		return nil
	}
	if r.exclusive {
		_ = unix.Flock(int(r.dir.Fd()), unix.LOCK_UN)
	}
	err := r.dir.Close()
	r.dir = nil
	return err
}

func (r *Root) CreateExclusive(name string, mode os.FileMode) (*os.File, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if mode.Perm()&0o077 != 0 {
		return nil, fmt.Errorf("safefs: create mode %o is not private", mode.Perm())
	}
	fd, err := unix.Openat(r.fd(), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil, ErrExists
		}
		return nil, fmt.Errorf("safefs: create %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := requireRegular(file); err != nil {
		_ = file.Close()
		_ = unix.Unlinkat(r.fd(), name, 0)
		return nil, err
	}
	return file, nil
}

func (r *Root) OpenRegular(name string) (*os.File, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(r.fd(), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("safefs: open %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := requireRegular(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (r *Root) ReadFile(name string, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, ErrTooLarge
	}
	file, err := r.OpenRegular(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("safefs: stat %s: %w", name, err)
	}
	if info.Size() > limit {
		return nil, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("safefs: read %s: %w", name, err)
	}
	if int64(len(body)) > limit {
		return nil, ErrTooLarge
	}
	return body, nil
}

func (r *Root) ReadDir() ([]os.DirEntry, error) {
	fd, err := unix.Openat(r.fd(), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("safefs: open data directory for listing: %w", err)
	}
	dir := os.NewFile(uintptr(fd), r.dir.Name())
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("safefs: read data directory: %w", err)
	}
	return entries, nil
}

func (r *Root) AvailableBytes() (uint64, error) {
	if r == nil || r.dir == nil {
		return 0, errors.New("safefs: data root is closed")
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(r.fd(), &stat); err != nil {
		return 0, fmt.Errorf("safefs: available bytes: %w", err)
	}
	blocks, blockSize := uint64(stat.Bavail), uint64(stat.Bsize)
	if blocks > uint64(^uint64(0)>>1) || blockSize == 0 || blockSize > 1<<30 {
		return 0, errors.New("safefs: invalid filesystem capacity")
	}
	high, low := bits.Mul64(blocks, blockSize)
	if high != 0 {
		return 0, errors.New("safefs: filesystem capacity overflow")
	}
	return low, nil
}

func (r *Root) PublicationDurability() Durability { return platformDurability() }

func (r *Root) Remove(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := unix.Unlinkat(r.fd(), name, 0); err != nil {
		return fmt.Errorf("safefs: remove %s: %w", name, err)
	}
	return nil
}

// RemoveTemp removes one exact, basename-scoped temporary entry without
// following it. Empty directories are accepted so a crashed or adversarially
// obstructed temp name cannot become permanent debris.
func (r *Root) RemoveTemp(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(r.fd(), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("safefs: stat temp %s: %w", name, err)
	}
	flags := 0
	if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(r.fd(), name, flags); err != nil {
		return fmt.Errorf("safefs: remove temp %s: %w", name, err)
	}
	return nil
}

func (r *Root) TryLock(name string) (*Lock, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(r.fd(), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("safefs: open named lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := requireRegular(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("safefs: lock named file: %w", err)
	}
	return &Lock{release: func() error {
		unlockErr := unix.Flock(fd, unix.LOCK_UN)
		return errors.Join(unlockErr, file.Close())
	}}, nil
}

func (r *Root) PublishNoReplace(temp, final string) (Publication, error) {
	if err := validateName(temp); err != nil {
		return Publication{}, err
	}
	if err := validateName(final); err != nil {
		return Publication{}, err
	}
	visible, err := publishNoReplace(r.fd(), temp, final)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return Publication{}, ErrExists
		}
		publication := Publication{Visible: visible, Durability: DurabilityUnknown}
		return publication, fmt.Errorf("safefs: publish %s: %w", final, err)
	}
	publication := Publication{Visible: visible, Durability: platformDurability()}
	if err := unix.Fsync(r.fd()); err != nil {
		publication.Durability = DurabilityUnknown
		return publication, fmt.Errorf("%w: sync directory: %v", ErrDurabilityUnknown, err)
	}
	return publication, nil
}

// Replace is reserved for mutable activation markers. Unlike
// PublishNoReplace, rename is allowed to replace an existing regular file.
// Visibility changes at rename; a later directory fsync failure therefore
// returns Visible=true with unknown durability and must never be reported as
// "not published" by callers.
func (r *Root) Replace(temp, final string) (Publication, error) {
	if err := validateName(temp); err != nil {
		return Publication{}, err
	}
	if err := validateName(final); err != nil {
		return Publication{}, err
	}
	if err := unix.Renameat(r.fd(), temp, r.fd(), final); err != nil {
		return Publication{}, fmt.Errorf("safefs: replace %s: %w", final, err)
	}
	publication := Publication{Visible: true, Durability: platformDurability()}
	if err := unix.Fsync(r.fd()); err != nil {
		publication.Durability = DurabilityUnknown
		return publication, fmt.Errorf("%w: sync directory after replacing %s: %v", ErrDurabilityUnknown, final, err)
	}
	return publication, nil
}

func (r *Root) fd() int { return int(r.dir.Fd()) }

func validateName(name string) error {
	if validateCommonName(name) != nil || filepath.Base(name) != name {
		return ErrInvalidName
	}
	return nil
}

func requireRegular(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("safefs: fstat: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrNotRegular
	}
	if stat.Nlink != 1 {
		return ErrAmbiguousLink
	}
	return nil
}
