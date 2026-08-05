package netstats

import (
	"sort"
	"strings"
)

const (
	// maxRawDetail bounds how much of a file's contents a health detail
	// repeats. A sysfs attribute is a handful of bytes; anything longer is
	// already evidence enough that the file is not what we expected.
	maxRawDetail = 32
	// maxErrDetail bounds an error string. It is larger than maxRawDetail
	// because "permission denied" style messages carry their meaning at the
	// end, and truncating them to 32 bytes would hide it.
	maxErrDetail = 96
)

// truncateRaw shortens file contents for inclusion in a health detail.
func truncateRaw(raw string) string { return truncate(raw, maxRawDetail) }

// truncateErr shortens an error message for inclusion in a health detail.
func truncateErr(message string) string { return truncate(message, maxErrDetail) }

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// noteSet aggregates degradations into one HealthNote per key. Emitting one
// note per interface would bury the run's health under veth devices, so the
// details are joined instead and capped at maxHealthDetails.
type noteSet struct {
	details map[string][]string
	seen    map[string]struct{}
}

func newNoteSet() *noteSet {
	return &noteSet{
		details: make(map[string][]string),
		seen:    make(map[string]struct{}),
	}
}

// add records one detail under a key. Duplicates are dropped so that a
// condition observed at both boundaries — the usual case for an unreadable
// attribute — is reported once.
func (n *noteSet) add(key, detail string) {
	if key == "" {
		return
	}
	dedupe := key + "\x00" + detail
	if _, duplicate := n.seen[dedupe]; duplicate {
		return
	}
	n.seen[dedupe] = struct{}{}
	if _, present := n.details[key]; present && len(n.details[key]) >= maxHealthDetails {
		// The key is already reported; further details would only lengthen a
		// string nobody reads.
		return
	}
	n.details[key] = append(n.details[key], detail)
}

// notes returns the aggregated notes sorted by key so a snapshot diff of two
// identical runs is empty.
func (n *noteSet) notes() []HealthNote {
	if len(n.details) == 0 {
		return nil
	}
	keys := make([]string, 0, len(n.details))
	for key := range n.details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]HealthNote, 0, len(keys))
	for _, key := range keys {
		out = append(out, HealthNote{Key: key, Detail: strings.Join(n.details[key], ",")})
	}
	return out
}
