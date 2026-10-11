package httpstats

import (
	"sort"
	"time"

	"github.com/ekusiadadus/isutools/internal/requestwork"
)

// RequestCall summarizes a statically labelled outgoing HTTP dependency.
type RequestCall = requestwork.Call

// RequestWork summarizes dependency timing on an HTTP identity. Wall-time
// components only include CompleteRequests; call totals include all tracked
// requests. Missing instrumentation is not detectable from a zero call count.
type RequestWork struct {
	TrackedRequests   int64         `json:"tracked_requests"`
	CompleteRequests  int64         `json:"complete_requests"`
	TruncatedRequests int64         `json:"truncated_requests"`
	Total             time.Duration `json:"total_ns"`
	CompleteTotal     time.Duration `json:"complete_total_ns"`
	SQLTotal          time.Duration `json:"sql_total_ns"`
	ExternalTotal     time.Duration `json:"external_total_ns"`
	SQLOnly           time.Duration `json:"sql_only_ns"`
	ExternalOnly      time.Duration `json:"external_only_ns"`
	Overlap           time.Duration `json:"overlap_ns"`
	Other             time.Duration `json:"other_ns"`
	ExternalCount     int64         `json:"external_count"`
	ExternalErrors    int64         `json:"external_errors"`
	Calls             []RequestCall `json:"calls,omitempty"`
}

// OverflowDependency is used when the per-generation endpoint/label budget is exhausted.
const OverflowDependency = "(other dependencies)"
const maxWorkKeys = 2048

func (t *table) mergeWork(dst *RequestWork, src requestwork.Summary, duration time.Duration) {
	dst.TrackedRequests++
	dst.Total += duration
	dst.SQLTotal += src.SQLTotal
	dst.ExternalTotal += src.ExternalTotal
	dst.ExternalCount += src.ExternalCount
	dst.ExternalErrors += src.ExternalErrors
	if src.Truncated {
		dst.TruncatedRequests++
	} else {
		dst.CompleteRequests++
		dst.CompleteTotal += duration
		dst.SQLOnly += src.SQLOnly
		dst.ExternalOnly += src.ExternalOnly
		dst.Overlap += src.Overlap
		dst.Other += src.Other
	}
	for _, call := range src.Calls {
		idx := -1
		for i := range dst.Calls {
			if dst.Calls[i].Label == call.Label {
				idx = i
				break
			}
		}
		if idx < 0 && call.Label != OverflowDependency {
			if !t.reserveWorkKey() {
				call.Label = OverflowDependency
			}
		}
		if idx < 0 {
			for i := range dst.Calls {
				if dst.Calls[i].Label == call.Label {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			dst.Calls = append(dst.Calls, RequestCall{Label: call.Label})
			idx = len(dst.Calls) - 1
		}
		addRequestCall(&dst.Calls[idx], call)
	}
}

func (t *table) reserveWorkKey() bool {
	for {
		n := t.workKeys.Load()
		if n >= maxWorkKeys {
			return false
		}
		if t.workKeys.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func addRequestCall(dst *RequestCall, src RequestCall) {
	dst.Count += src.Count
	dst.Errors += src.Errors
	dst.Reused += src.Reused
	dst.Total += src.Total
	dst.Acquire += src.Acquire
	dst.DNS += src.DNS
	dst.Connect += src.Connect
	dst.TLS += src.TLS
	dst.Wait += src.Wait
	dst.Headers += src.Headers
	dst.Body += src.Body
}

func cloneWork(src *RequestWork) *RequestWork {
	if src == nil {
		return nil
	}
	dst := *src
	dst.Calls = append([]RequestCall(nil), src.Calls...)
	sort.Slice(dst.Calls, func(i, j int) bool { return dst.Calls[i].Label < dst.Calls[j].Label })
	return &dst
}
