package hoststats

import "errors"

// Sentinel errors. Callers match them with errors.Is; wrap them with %w rather
// than replacing them, so that "this host cannot be measured at all" stays
// distinguishable from "this particular read failed".
var (
	// ErrUnsupportedOS reports that there is no procfs to read: a non-Linux
	// host, or a Linux host where /proc is not mounted. New returns it so the
	// caller can skip registration entirely instead of registering a collector
	// that would fail every boundary.
	ErrUnsupportedOS = errors.New("hoststats: unsupported OS or missing procfs")

	// ErrNoSource reports that even the required source (/proc/meminfo) could
	// not be read, so no sample exists. Every other source is optional and
	// degrades to a "not-captured:<source>" code instead.
	ErrNoSource = errors.New("hoststats: no source readable")
)
