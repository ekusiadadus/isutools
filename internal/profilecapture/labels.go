package profilecapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	PrivateCaptureLabel    = "isutools.v1.capture"
	PrivateTupleLabel      = "isutools.v1.tuple"
	MaxLabelTuples         = 256
	MaxConcreteLabelTuples = MaxLabelTuples - 1
)

type SafeLabelTuple struct {
	TupleID  string `json:"tuple_id"`
	Method   string `json:"method"`
	Route    string `json:"route"`
	Scenario string `json:"scenario,omitempty"`
	Region   string `json:"region,omitempty"`
	Overflow bool   `json:"overflow,omitempty"`
}

type LabelDictionary struct {
	RunID     string           `json:"run_id"`
	Epoch     uint64           `json:"epoch"`
	CaptureID string           `json:"capture_id"`
	Sealed    bool             `json:"sealed"`
	Tuples    []SafeLabelTuple `json:"tuples"`
	SHA256    string           `json:"sha256"`
}

type labelContextKey struct{}

func LogicalLabels(ctx context.Context) (SafeLabelTuple, bool) {
	if ctx == nil {
		return SafeLabelTuple{}, false
	}
	labels, ok := ctx.Value(labelContextKey{}).(SafeLabelTuple)
	return labels, ok
}

// LabelScope is bound to exactly one CPU capture. Stop seals it before the
// runtime profiler is asked to flush, so later generations cannot append new
// tuples to the stopping profile.
type LabelScope struct {
	mu        sync.Mutex
	captureID string
	sealed    bool
	tuples    map[labelLogical]SafeLabelTuple
	overflow  *SafeLabelTuple
}

type labelLogical struct {
	method, route, scenario, region string
}

func newLabelScope(captureID string) *LabelScope {
	return &LabelScope{captureID: captureID, tuples: make(map[labelLogical]SafeLabelTuple, MaxConcreteLabelTuples)}
}

// NewStandaloneLabelScope creates a scope for offline fixtures and adapters.
// It does not activate runtime profiling; production capture ownership remains
// exclusively in Coordinator.
func NewStandaloneLabelScope(captureID string) *LabelScope {
	if len(captureID) != 32 || !lowerHex(captureID) {
		return nil
	}
	return newLabelScope(captureID)
}

func (s *LabelScope) CaptureID() string {
	if s == nil {
		return ""
	}
	return s.captureID
}

// Do always invokes fn. Its return value reports whether private pprof labels
// were applied. Invalid or sealed scopes fail open for application traffic.
func (s *LabelScope) Do(ctx context.Context, logical SafeLabelTuple, fn func(context.Context)) bool {
	if fn == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || !validLogicalTuple(logical) {
		fn(ctx)
		return false
	}
	tuple, ok := s.intern(logical)
	if !ok {
		fn(ctx)
		return false
	}
	logical.TupleID = ""
	logical.Overflow = false
	ctx = context.WithValue(ctx, labelContextKey{}, logical)
	labels := pprof.Labels(PrivateCaptureLabel, s.captureID, PrivateTupleLabel, tuple.TupleID)
	pprof.Do(ctx, labels, fn)
	return true
}

func (s *LabelScope) intern(logical SafeLabelTuple) (SafeLabelTuple, bool) {
	key := labelLogical{method: logical.Method, route: logical.Route, scenario: logical.Scenario, region: logical.Region}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return SafeLabelTuple{}, false
	}
	if tuple, ok := s.tuples[key]; ok {
		return tuple, true
	}
	if len(s.tuples) < MaxConcreteLabelTuples {
		logical.TupleID = newCaptureID()
		logical.Overflow = false
		s.tuples[key] = logical
		return logical, true
	}
	if s.overflow == nil {
		overflow := SafeLabelTuple{TupleID: newCaptureID(), Method: "OTHER", Route: "(overflow)", Overflow: true}
		s.overflow = &overflow
	}
	return *s.overflow, true
}

func (s *LabelScope) Seal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
}

func (s *LabelScope) Dictionary(runID string, epoch uint64) LabelDictionary {
	if s == nil {
		return LabelDictionary{}
	}
	s.mu.Lock()
	dictionary := LabelDictionary{RunID: runID, Epoch: epoch, CaptureID: s.captureID, Sealed: s.sealed}
	dictionary.Tuples = make([]SafeLabelTuple, 0, len(s.tuples)+1)
	for _, tuple := range s.tuples {
		dictionary.Tuples = append(dictionary.Tuples, tuple)
	}
	if s.overflow != nil {
		dictionary.Tuples = append(dictionary.Tuples, *s.overflow)
	}
	s.mu.Unlock()
	sort.Slice(dictionary.Tuples, func(i, j int) bool {
		a, b := dictionary.Tuples[i], dictionary.Tuples[j]
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		if a.Route != b.Route {
			return a.Route < b.Route
		}
		if a.Scenario != b.Scenario {
			return a.Scenario < b.Scenario
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		return a.TupleID < b.TupleID
	})
	body, _ := json.Marshal(dictionary)
	sum := sha256.Sum256(body)
	dictionary.SHA256 = hex.EncodeToString(sum[:])
	return dictionary
}

func validLogicalTuple(tuple SafeLabelTuple) bool {
	if tuple.TupleID != "" || tuple.Overflow || !validLabelMethod(tuple.Method) || !validLabelText(tuple.Route, 128) {
		return false
	}
	return validOptionalLabelToken(tuple.Scenario) && validOptionalLabelToken(tuple.Region)
}

func validLabelMethod(method string) bool {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE", "OTHER":
		return true
	default:
		return false
	}
}

func validLabelText(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) < 0
}

func validOptionalLabelToken(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
