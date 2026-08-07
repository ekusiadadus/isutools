package safefs

import (
	"errors"
	"strings"
	"sync"
	"unicode/utf8"
)

func validateCommonName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) ||
		strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return ErrInvalidName
	}
	return nil
}

var (
	ErrInvalidName           = errors.New("safefs: invalid basename")
	ErrExists                = errors.New("safefs: immutable target exists")
	ErrTooLarge              = errors.New("safefs: file exceeds read limit")
	ErrNotRegular            = errors.New("safefs: target is not a regular file")
	ErrUnsupportedFilesystem = errors.New("safefs: filesystem lacks approved publication semantics")
	ErrLocked                = errors.New("safefs: data directory is already owned by another process")
	ErrDurabilityUnknown     = errors.New("safefs: artifact visible but crash durability is unknown")
)

type Durability string

const (
	DurabilityDurable Durability = "durable"
	DurabilityUnknown Durability = "unknown"
	DurabilityFailed  Durability = "failed"
)

type Publication struct {
	Visible    bool
	Durability Durability
}

type Options struct {
	RequireStrongVisibility bool
	Exclusive               bool
}

// Root owns an open directory descriptor. All artifact paths are basename-only
// and all opens use the descriptor with O_NOFOLLOW, so pathname replacement or
// a symlink inside DataDir cannot redirect an operation elsewhere.
type Root struct {
	rootPlatform
	exclusive bool
}

type Lock struct {
	once    sync.Once
	release func() error
	err     error
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.release != nil {
			l.err = l.release()
		}
	})
	return l.err
}
