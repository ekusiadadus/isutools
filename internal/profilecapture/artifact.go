package profilecapture

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

var (
	ErrArtifactTooLarge = errors.New("profilecapture: CPU artifact exceeds byte ceiling")
	ErrArtifactSettled  = errors.New("profilecapture: CPU artifact is already settled")
)

type FileArtifactFactory struct {
	root     *safefs.Root
	maxBytes int64
}

func NewFileArtifactFactory(root *safefs.Root, maxBytes int64) *FileArtifactFactory {
	return &FileArtifactFactory{root: root, maxBytes: maxBytes}
}

func (f *FileArtifactFactory) New(_ StartRequest, captureID string) (Artifact, error) {
	if f == nil || f.root == nil || f.maxBytes <= 0 {
		return nil, errors.New("profilecapture: invalid file artifact factory")
	}
	if len(captureID) != 32 || !lowerHex(captureID) {
		return nil, errors.New("profilecapture: invalid capture id")
	}
	final := "cpu_" + captureID + ".pprof"
	temp := final + ".tmp"
	file, err := f.root.CreateExclusive(temp, 0o600)
	if err != nil {
		return nil, err
	}
	hasher := sha256.New()
	a := &fileArtifact{
		root: f.root, file: file, temp: temp, final: final,
		hash: hasher, maxBytes: f.maxBytes,
	}
	a.writer = &boundedHashWriter{file: file, hash: hasher, max: f.maxBytes, owner: a}
	return a, nil
}

type fileArtifact struct {
	mu       sync.Mutex
	root     *safefs.Root
	file     *os.File
	temp     string
	final    string
	hash     hash.Hash
	writer   *boundedHashWriter
	maxBytes int64
	bytes    int64
	writeErr error
	settled  bool
}

func (a *fileArtifact) Writer() io.Writer { return a.writer }

func (a *fileArtifact) Publish() (PublishedArtifact, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settled {
		return PublishedArtifact{}, ErrArtifactSettled
	}
	if a.writeErr != nil {
		_ = a.closeAndRemoveLocked()
		a.settled = true
		return PublishedArtifact{}, a.writeErr
	}
	if err := a.file.Sync(); err != nil {
		_ = a.closeAndRemoveLocked()
		a.settled = true
		return PublishedArtifact{}, fmt.Errorf("profilecapture: sync CPU temp: %w", err)
	}
	if err := a.file.Close(); err != nil {
		_ = a.root.Remove(a.temp)
		a.file = nil
		a.settled = true
		return PublishedArtifact{}, fmt.Errorf("profilecapture: close CPU temp: %w", err)
	}
	a.file = nil
	publication, err := a.root.PublishNoReplace(a.temp, a.final)
	if err != nil && !publication.Visible {
		_ = a.root.Remove(a.temp)
		a.settled = true
		return PublishedArtifact{}, err
	}
	a.settled = true
	result := PublishedArtifact{
		File: a.final, SHA256: hex.EncodeToString(a.hash.Sum(nil)), Bytes: a.bytes,
		Visible: publication.Visible, Durability: string(publication.Durability),
	}
	return result, err
}

func (a *fileArtifact) Abort() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settled {
		return nil
	}
	a.settled = true
	return a.closeAndRemoveLocked()
}

func (a *fileArtifact) closeAndRemoveLocked() error {
	var closeErr error
	if a.file != nil {
		closeErr = a.file.Close()
		a.file = nil
	}
	removeErr := a.root.Remove(a.temp)
	if os.IsNotExist(removeErr) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

type boundedHashWriter struct {
	file  io.Writer
	hash  io.Writer
	max   int64
	owner *fileArtifact
}

func (w *boundedHashWriter) Write(p []byte) (int, error) {
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	if w.owner.settled {
		return 0, ErrArtifactSettled
	}
	if w.owner.writeErr != nil {
		return 0, w.owner.writeErr
	}
	if int64(len(p)) > w.max-w.owner.bytes {
		w.owner.writeErr = ErrArtifactTooLarge
		return 0, ErrArtifactTooLarge
	}
	n, err := io.MultiWriter(w.file, w.hash).Write(p)
	w.owner.bytes += int64(n)
	if err != nil {
		w.owner.writeErr = err
	}
	return n, err
}

func lowerHex(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) < 0
}
