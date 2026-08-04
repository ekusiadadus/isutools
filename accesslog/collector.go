package accesslog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

const (
	StatusOK      = "ok"
	StatusPartial = "partial"
	StatusError   = "error"

	defaultMaxLineBytes    = 1 << 20
	defaultMaxCollectBytes = 64 << 20
)

// ErrCollectLimit means one collection pass reached its configured byte cap.
// A later pass resumes from the retained offset.
var ErrCollectLimit = errors.New("accesslog: per-collect byte limit reached")

// Health exposes collection failures without making them fatal to the host
// application. Counters apply to the current generation.
type Health struct {
	Status        string `json:"status"`
	Message       string `json:"message,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Errors        int64  `json:"errors"`
	Dropped       int64  `json:"dropped"`
	Partial       int64  `json:"partial"`
	Rotations     int64  `json:"rotations"`
	CopyTruncates int64  `json:"copy_truncates"`
	Offset        int64  `json:"offset"`
}

// File is the seekable file boundary used by Collector.
type File interface {
	io.Reader
	io.Seeker
	io.Closer
	Stat() (fs.FileInfo, error)
}

// FileSystem permits deterministic collector tests and non-OS-backed log
// sources. Implementations should return stable FileInfo values suitable for
// SameFile comparisons.
type FileSystem interface {
	Open(name string) (File, error)
	Stat(name string) (fs.FileInfo, error)
}

type osFileSystem struct{}

func (osFileSystem) Open(name string) (File, error)        { return os.Open(name) }
func (osFileSystem) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// Option customizes a Collector.
type Option func(*Collector)

// WithFileSystem replaces the OS filesystem boundary.
func WithFileSystem(fsys FileSystem) Option {
	return func(c *Collector) {
		if fsys != nil {
			c.fs = fsys
		}
	}
}

// WithSameFile replaces inode-equivalence detection for an injected filesystem.
func WithSameFile(same func(fs.FileInfo, fs.FileInfo) bool) Option {
	return func(c *Collector) {
		if same != nil {
			c.sameFile = same
		}
	}
}

// WithMaxLineBytes bounds one pending log line. Oversized lines are discarded
// and reported through Health.
func WithMaxLineBytes(n int) Option {
	return func(c *Collector) {
		if n > 0 {
			c.maxLineBytes = n
		}
	}
}

// WithMaxCollectBytes bounds bytes read in one Collect/CollectContext call.
func WithMaxCollectBytes(n int64) Option {
	return func(c *Collector) {
		if n > 0 {
			c.maxCollectBytes = n
		}
	}
}

// WithMaxKeys bounds distinct method/path aggregates.
func WithMaxKeys(n int) Option {
	return func(c *Collector) {
		if n > 0 {
			c.agg = NewAggregator(n)
		}
	}
}

// Collector tails a log from a generation baseline. It retains an open file
// descriptor so an inode-rotated file can be drained before the new path.
type Collector struct {
	mu sync.Mutex

	path            string
	fs              FileSystem
	sameFile        func(fs.FileInfo, fs.FileInfo) bool
	maxLineBytes    int
	maxCollectBytes int64
	agg             *Aggregator

	file     File
	offset   int64
	pending  []byte
	dropping bool
	closed   bool
	health   Health
}

// New creates a collector and sets its generation baseline to the current EOF.
// Startup failures are recorded in Health and do not panic or stop the caller.
func New(path string, opts ...Option) *Collector {
	c := &Collector{
		path: path, fs: osFileSystem{}, sameFile: os.SameFile,
		maxLineBytes: defaultMaxLineBytes, maxCollectBytes: defaultMaxCollectBytes,
		agg:    NewAggregator(DefaultMaxKeys),
		health: Health{Status: StatusOK},
	}
	for _, opt := range opts {
		opt(c)
	}
	c.mu.Lock()
	if err := c.openLocked(true); err != nil {
		c.recordErrorLocked(err)
	}
	c.mu.Unlock()
	return c
}

// Collect consumes all currently available complete lines. It returns only I/O
// failures; malformed records are dropped and reported through Health.
func (c *Collector) Collect() error {
	return c.CollectContext(context.Background())
}

// CollectContext is Collect with cancellation between bounded file reads.
func (c *Collector) CollectContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.maxCollectBytes
	if c.closed {
		return errors.New("accesslog: collector is closed")
	}
	if c.file == nil {
		if err := c.openLocked(false); err != nil {
			c.recordErrorLocked(err)
			return err
		}
	}

	pathInfo, err := c.fs.Stat(c.path)
	if err != nil {
		c.recordErrorLocked(fmt.Errorf("stat %s: %w", c.path, err))
		return err
	}
	heldInfo, err := c.file.Stat()
	if err != nil {
		c.recordErrorLocked(fmt.Errorf("stat open %s: %w", c.path, err))
		return err
	}

	if !c.sameFile(heldInfo, pathInfo) {
		if err := c.readToEOFLocked(ctx, &remaining); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if errors.Is(err, ErrCollectLimit) {
				c.health.LastError = err.Error()
				c.recordPartialLocked("per-collect byte limit reached while draining rotated log")
				return err
			}
			c.recordErrorLocked(fmt.Errorf("drain rotated %s: %w", c.path, err))
			return err
		}
		c.discardPendingLocked("incomplete line at rotated EOF")
		_ = c.file.Close()
		c.file = nil
		c.offset = 0
		c.health.Rotations++
		c.recordPartialLocked("log rotation observed; drain is best effort")
		if err := c.openLocked(false); err != nil {
			c.recordErrorLocked(err)
			return err
		}
	} else if pathInfo.Size() < c.offset {
		c.discardPendingLocked("incomplete line discarded after copytruncate")
		if _, err := c.file.Seek(0, io.SeekStart); err != nil {
			c.recordErrorLocked(fmt.Errorf("seek copytruncated %s: %w", c.path, err))
			return err
		}
		c.offset = 0
		c.health.CopyTruncates++
		c.recordPartialLocked("copytruncate observed; loss or duplication is possible")
	}

	if err := c.readToEOFLocked(ctx, &remaining); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, ErrCollectLimit) {
			c.health.LastError = err.Error()
			c.recordPartialLocked("per-collect byte limit reached; call collect again to continue")
			return err
		}
		c.recordErrorLocked(fmt.Errorf("read %s: %w", c.path, err))
		return err
	}
	c.recordSuccessLocked()
	return nil
}

// CollectUntilStable polls until no newly flushed bytes are observed for
// quietFor. It is intended for explicitly buffered nginx logs at snapshot
// time; callers bound the wait with ctx. A non-positive quietFor performs one
// ordinary Collect.
func (c *Collector) CollectUntilStable(ctx context.Context, quietFor, pollEvery time.Duration) error {
	if quietFor <= 0 {
		return c.CollectContext(ctx)
	}
	if pollEvery <= 0 {
		pollEvery = quietFor / 4
		if pollEvery <= 0 {
			pollEvery = time.Millisecond
		}
	}
	if err := c.CollectContext(ctx); err != nil {
		return err
	}
	last := c.progressMarker()
	stableSince := time.Now()
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("accesslog: wait for buffered log flush: %w", ctx.Err())
		case now := <-ticker.C:
			if err := c.CollectContext(ctx); err != nil {
				return err
			}
			current := c.progressMarker()
			if current != last {
				last = current
				stableSince = now
			}
			if now.Sub(stableSince) >= quietFor {
				return nil
			}
		}
	}
}

// Snapshot collects available lines and returns an immutable aggregate copy.
// Any collection error is represented in the embedded Health value.
func (c *Collector) Snapshot() Snapshot {
	_ = c.Collect()
	c.mu.Lock()
	defer c.mu.Unlock()
	snapshot := c.agg.Snapshot()
	c.health.Offset = c.offset
	snapshot.Health = c.health
	return snapshot
}

// Reset clears aggregates and baselines the next generation at current EOF.
func (c *Collector) Reset() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("accesslog: collector is closed")
	}
	if c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
	c.offset = 0
	c.pending = nil
	c.dropping = false
	c.agg.Reset()
	c.health = Health{Status: StatusOK}
	if err := c.openLocked(true); err != nil {
		c.recordErrorLocked(err)
		return err
	}
	return nil
}

// Health returns a copy of the current generation's collector health.
func (c *Collector) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Offset = c.offset
	return c.health
}

// Close releases the retained log descriptor. It is idempotent.
func (c *Collector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
}

func (c *Collector) openLocked(baselineEOF bool) error {
	f, err := c.fs.Open(c.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", c.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat %s: %w", c.path, err)
	}
	offset := int64(0)
	if baselineEOF {
		offset = info.Size()
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return fmt.Errorf("seek %s: %w", c.path, err)
	}
	c.file = f
	c.offset = offset
	return nil
}

func (c *Collector) readToEOFLocked(ctx context.Context, remaining *int64) error {
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *remaining <= 0 {
			return ErrCollectLimit
		}
		readBuf := buf
		if int64(len(readBuf)) > *remaining {
			readBuf = readBuf[:*remaining]
		}
		n, err := c.file.Read(readBuf)
		if n > 0 {
			*remaining -= int64(n)
			c.offset += int64(n)
			c.consumeLocked(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

func (c *Collector) consumeLocked(data []byte) {
	for _, b := range data {
		if c.dropping {
			if b == '\n' {
				c.dropping = false
			}
			continue
		}
		if b == '\n' {
			line := string(c.pending)
			c.pending = c.pending[:0]
			rec, err := ParseLine(line)
			if err != nil {
				c.health.Dropped++
				c.health.LastError = err.Error()
				c.recordPartialLocked("malformed access-log records were dropped")
				continue
			}
			c.agg.Observe(rec)
			if rec.Partial {
				c.health.Partial++
				c.recordPartialLocked("some access-log fields were only partially usable")
			}
			continue
		}
		c.pending = append(c.pending, b)
		if len(c.pending) > c.maxLineBytes {
			c.pending = nil
			c.dropping = true
			c.health.Dropped++
			c.health.LastError = "accesslog: line exceeds configured size limit"
			c.recordPartialLocked("oversized access-log records were dropped")
		}
	}
}

func (c *Collector) discardPendingLocked(message string) {
	if len(c.pending) == 0 && !c.dropping {
		return
	}
	c.pending = nil
	c.dropping = false
	c.health.Dropped++
	c.recordPartialLocked(message)
}

func (c *Collector) recordPartialLocked(message string) {
	if c.health.Status == StatusOK || c.health.Status == "" {
		c.health.Status = StatusPartial
	}
	if c.health.Message == "" {
		c.health.Message = message
	}
}

func (c *Collector) recordErrorLocked(err error) {
	c.health.Status = StatusError
	c.health.Errors++
	c.health.LastError = err.Error()
	if c.health.Message == "" {
		c.health.Message = "access-log collection failed; application execution is unaffected"
	}
}

func (c *Collector) recordSuccessLocked() {
	c.health.LastError = ""
	if c.health.Dropped > 0 || c.health.Partial > 0 || c.health.Rotations > 0 || c.health.CopyTruncates > 0 {
		c.health.Status = StatusPartial
		return
	}
	c.health.Status = StatusOK
	c.health.Message = ""
}

type progress struct {
	offset        int64
	rotations     int64
	copyTruncates int64
}

func (c *Collector) progressMarker() progress {
	c.mu.Lock()
	defer c.mu.Unlock()
	return progress{offset: c.offset, rotations: c.health.Rotations, copyTruncates: c.health.CopyTruncates}
}
