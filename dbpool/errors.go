package dbpool

import (
	"errors"

	"github.com/ekusiadadus/isutools/sqlstats"
)

// MaxPools bounds the watch set. It tracks the registry's target limit because
// a target has at most one application pool, so the registry normally reaches
// its own limit first; this bound exists so that a caller looping over dynamic
// configuration cannot grow the watch set without end.
const MaxPools = sqlstats.MaxTargets

// Watch errors. They are sentinels so a caller can tell "my argument was
// wrong" from "this build has more pools than the toolkit supports" with
// errors.Is. An unregistered target ID is reported as sqlstats.ErrUnknownTarget
// rather than as a dbpool-specific alias: there is one target namespace, and a
// second name for the same condition would only make it harder to match on.
var (
	// ErrNilDB means Watch was handed a nil handle. It is reported instead of
	// being silently ignored so that an argument bug does not masquerade as a
	// pool that simply never saw traffic.
	ErrNilDB = errors.New("isutools: WatchDBPool: db is nil")
	// ErrDuplicatePool means the target is already watched. Two handles for
	// one logical database would produce two rows that both claim to be it, so
	// the second registration is rejected; recreating a pool is
	// UnwatchDBPool followed by WatchDBPool.
	ErrDuplicatePool = errors.New("isutools: WatchDBPool: target already watched")
	// ErrTooManyPools means the watch set is at MaxPools.
	ErrTooManyPools = errors.New("isutools: WatchDBPool: too many pools (max 16)")
)
