// Package buildinfo resolves the git revision and dirty state of the
// running binary: from Go's embedded VCS stamps when available, otherwise
// from ldflags-injected variables or environment variables.
package buildinfo

import (
	"os"
	"runtime/debug"
)

// Injectable via:
//
//	go build -ldflags "-X github.com/ekusiadadus/isutools/buildinfo.Revision=$(git rev-parse HEAD) \
//	                   -X github.com/ekusiadadus/isutools/buildinfo.DirtyFlag=$(test -n "$(git status --porcelain)" && echo dirty)"
var (
	Revision  string
	DirtyFlag string
)

const (
	envHash  = "ISUTOOLS_GIT_HASH"
	envDirty = "ISUTOOLS_GIT_DIRTY"
	shortLen = 7
)

// Info describes the resolved build revision.
type Info struct {
	Revision string `json:"revision"`
	Dirty    bool   `json:"dirty"`
	Source   string `json:"source"` // vcs | ldflags | env | unknown
}

// Get resolves the revision with precedence: embedded VCS stamp,
// ldflags variables, environment variables.
func Get() Info { return get(debug.ReadBuildInfo, os.Getenv) }

func get(read func() (*debug.BuildInfo, bool), getenv func(string) string) Info {
	if bi, ok := read(); ok && bi != nil {
		var rev string
		var dirty bool
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			return Info{Revision: rev, Dirty: dirty, Source: "vcs"}
		}
	}
	if Revision != "" {
		return Info{Revision: Revision, Dirty: DirtyFlag != "", Source: "ldflags"}
	}
	if v := getenv(envHash); v != "" {
		return Info{Revision: v, Dirty: getenv(envDirty) != "", Source: "env"}
	}
	return Info{Source: "unknown"}
}

// Short renders the revision as "f4fdb31" or "f4fdb31 (dirty)".
func (i Info) Short() string {
	r := i.Revision
	if r == "" {
		r = "unknown"
	}
	if len(r) > shortLen {
		r = r[:shortLen]
	}
	if i.Dirty {
		return r + " (dirty)"
	}
	return r
}
