// Package profileowner serializes process-wide runtime diagnostic facilities.
package profileowner

import "sync"

type Registry struct {
	mu    sync.Mutex
	owner string
}

func (r *Registry) Acquire(owner string) bool {
	if r == nil || owner == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner != "" {
		return false
	}
	r.owner = owner
	return true
}

func (r *Registry) Release(owner string) bool {
	if r == nil || owner == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owner != owner {
		return false
	}
	r.owner = ""
	return true
}

func (r *Registry) Active() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner
}

var Default Registry
