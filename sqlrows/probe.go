package sqlrows

import (
	"context"
	"fmt"
	"strings"

	"github.com/ekusiadadus/isutools/sqlstats"
)

// probeResult is one target's cached capability verdict.
//
// Probing costs five statements, so it is paid once per target per process
// rather than once per boundary — the per-boundary cost is unchanged. The
// cache is invalidated when the server UUID changes, because that is a
// different server with possibly different capabilities, on a connection whose
// hygiene has to be established again.
type probeResult struct {
	// supported reports that the target can be measured. When false, reason
	// says why in a form suitable for a health message.
	supported bool
	reason    string
	// failed distinguishes "the server answered no" from "the question could
	// not be asked". A definite no is a skip, an error is a failure: only the
	// latter may make a boundary uncommitted.
	failed bool
	// unsafeConn reports that the connection this verdict was taken on could
	// not be proven free of a default database — it has one, or the question
	// could not be answered. Either way this collector's own statements may be
	// recorded under the measured schema, so the target is skipped rather than
	// measured, and never connected to again. It is not a failure: a
	// misconfigured target must not cost the run its other targets.
	unsafeConn bool
	// useSHOW routes the uptime read through SHOW GLOBAL STATUS.
	useSHOW bool
	// hasQuerySampleText records QUERY_SAMPLE_TEXT for plan 09, which cannot
	// probe it separately without paying for another statement.
	hasQuerySampleText bool
	// serverUUID is the identity the probe verdict belongs to. Empty until the
	// first metadata read of the same boundary fills it in.
	serverUUID string
	// probed marks a real verdict, so the zero value cannot be mistaken for a
	// cached "unsupported".
	probed bool
}

// runProbe executes the five capability statements. It never returns an error:
// the verdict, including "the probe itself failed", is the return value, so a
// caller can record it on the target sample instead of aborting the boundary.
func runProbe(ctx context.Context, q sqlstats.Querier) probeResult {
	result := probeResult{probed: true}

	var enabled any
	if err := q.QueryRowContext(ctx, probePerformanceSchema).Scan(&enabled); err != nil {
		return probeFailed(fmt.Errorf("read @@performance_schema: %w", err))
	}
	if !truthy(enabled) {
		result.reason = "performance_schema is OFF"
		return result
	}

	// Connection hygiene is checked second, right after the one statement that
	// says whether the question can even be asked: everything below is issued
	// on this connection, so a connection that files them under the measured
	// schema has to be found before the collector adds to that schema's rows.
	if verdict, ok := defaultSchemaVerdict(ctx, q); !ok {
		return verdict
	}

	consumer, err := scanSingle(ctx, q, probeDigestConsumer)
	if err != nil {
		return probeFailed(fmt.Errorf("read setup_consumers.statements_digest: %w", err))
	}
	if consumer == nil {
		result.reason = "setup_consumers has no statements_digest row"
		return result
	}
	if !truthy(consumer) {
		result.reason = "the statements_digest consumer is disabled"
		return result
	}

	columns, err := scanColumnSet(ctx, q, probeColumns)
	if err != nil {
		return probeFailed(fmt.Errorf("read digest table columns: %w", err))
	}
	if missing := missingColumns(columns); len(missing) > 0 {
		result.reason = "digest table is missing " + strings.Join(missing, ", ")
		return result
	}
	result.hasQuerySampleText = columns[optionalQuerySampleColumn]

	// A failing uptime read is not a capability problem: MariaDB and locked
	// down installations expose Uptime only through SHOW.
	if _, err := scanSingle(ctx, q, probeUptime); err != nil {
		result.useSHOW = true
	}

	result.supported = true
	return result
}

// probeFailed builds the verdict for a probe that could not be completed. It
// is deliberately not "unsupported": a permission error must degrade the run,
// while performance_schema=OFF is a legitimate configuration.
func probeFailed(err error) probeResult {
	return probeResult{probed: true, failed: true, reason: err.Error()}
}

// defaultSchemaVerdict checks that the inspection connection carries no default
// database. It returns ok=false together with the verdict to record when the
// connection cannot be proven clean.
//
// "Cannot be proven" is treated exactly like "is dirty". The alternative is to
// measure a schema whose rows may include this collector's own statements,
// which is the failure plan 04 exists to prevent and which is invisible in the
// output — a skipped target with a reason is the only honest answer.
func defaultSchemaVerdict(ctx context.Context, q sqlstats.Querier) (probeResult, bool) {
	schema, known, err := readDefaultSchema(ctx, q)
	switch {
	case err != nil:
		return probeUnsafeConn("the inspection connection's default database could not be read, "+
			"so this collector's statements may be recorded under the measured schema: %v", err), false
	case !known:
		return probeUnsafeConn("performance_schema.threads has no row for this connection, " +
			"so its default database could not be verified"), false
	case schema != "":
		return probeUnsafeConn("the inspection connection has default database %q: "+
			"this collector's own statements would be recorded as that schema's digests "+
			"and counted in its own interval", schema), false
	}
	return probeResult{}, true
}

// probeUnsafeConn builds the verdict for a connection that may contaminate the
// schema it measures.
func probeUnsafeConn(format string, args ...any) probeResult {
	return probeResult{probed: true, unsafeConn: true, reason: fmt.Sprintf(format, args...)}
}

// readDefaultSchema reads the session's default database from
// performance_schema. known is false when the server returned no row at all,
// which is a different thing from a row saying NULL: the first means the
// question went unanswered, the second is the answer this collector needs.
func readDefaultSchema(ctx context.Context, q sqlstats.Querier) (schema string, known bool, err error) {
	rows, err := q.QueryContext(ctx, probeDefaultSchema)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", false, rows.Err()
	}
	var value any
	if err := rows.Scan(&value); err != nil {
		return "", false, err
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	name, notNull := nullableString(value)
	if !notNull {
		return "", true, nil
	}
	return strings.TrimSpace(name), true, nil
}

// scanSingle reads one value, returning (nil, nil) when the query matched no
// row — "no such setting" and "the setting is off" need different reasons.
func scanSingle(ctx context.Context, q sqlstats.Querier, query string, args ...any) (any, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var value any
	if err := rows.Scan(&value); err != nil {
		return nil, err
	}
	return value, rows.Err()
}

// scanColumnSet reads a single-column result set into an upper-cased set.
func scanColumnSet(ctx context.Context, q sqlstats.Querier, query string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name any
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[strings.ToUpper(strings.TrimSpace(toString(name)))] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// missingColumns lists the required columns the server does not have.
func missingColumns(present map[string]bool) []string {
	var missing []string
	for _, name := range requiredColumns {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	return missing
}
