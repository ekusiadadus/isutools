package runctl

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

// newID mints an opaque identifier. It never fails: a measurement toolkit must
// not take the application down because the entropy source hiccupped, so a
// timestamp is a good enough fallback for an identifier whose only job is to
// be distinct within a process.
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
