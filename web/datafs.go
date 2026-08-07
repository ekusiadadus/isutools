package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/ekusiadadus/isutools/internal/safefs"
)

func (h *handler) openDataRoot() (*safefs.Root, error) {
	if h.p.DataDir == "" {
		return nil, fmt.Errorf("data directory is not configured")
	}
	return safefs.Open(h.p.DataDir, safefs.Options{RequireStrongVisibility: false, Exclusive: false})
}

func (h *handler) serveDataFile(w http.ResponseWriter, r *http.Request, name string) {
	root, err := h.openDataRoot()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenRegular(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (h *handler) dataEntries() []dataEntry {
	root, err := h.openDataRoot()
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	entries, err := root.ReadDir()
	if err != nil {
		return nil
	}
	out := make([]dataEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, dataEntry{name: entry.Name(), modTime: info.ModTime()})
	}
	return out
}

type dataEntry struct {
	name    string
	modTime time.Time
}
