package sqlrows

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ekusiadadus/isutools/internal/runctl"
	"github.com/ekusiadadus/isutools/sqlstats"
)

// captureOutcome is what one boundary produced across every target.
type captureOutcome struct {
	sample *Sample
	// probes carries verdicts learned during this boundary, to be merged into
	// the collector's cache once the fan-out has joined.
	probes map[string]probeResult
	// captured counts targets with real numbers; failed counts targets that
	// could not be read. Targets that were skipped for a configuration reason
	// count as neither: see ErrNoTargetCaptured.
	captured int
	failed   int
}

// sampleTargets reads every registered target, in waves of at most
// concurrency, in TargetID order.
//
// The order is fixed rather than incidental so that a boundary which runs out
// of budget always drops the same targets: a run whose missing targets shuffle
// between runs cannot be compared with the previous one.
func (c *Collector) sampleTargets(ctx context.Context, phase runctl.Phase, probes map[string]probeResult, baseline *Sample) captureOutcome {
	infos := c.targets()
	samples := make([]*TargetSample, len(infos))
	learned := make([]*probeResult, len(infos))

	width := c.concurrency
	if width < 1 {
		width = 1
	}

	for start := 0; start < len(infos); {
		if !c.waveFits(ctx) {
			// Refusing to start a wave that cannot finish is what keeps the
			// drop deterministic: the alternative is every target racing the
			// deadline and losing at a different point every run.
			for i := start; i < len(infos); i++ {
				samples[i] = skipped(infos[i], CodeBudgetExhausted,
					"boundary budget was exhausted before this target's wave started")
			}
			break
		}
		end := min(start+width, len(infos))
		var wg sync.WaitGroup
		for i := start; i < end; i++ {
			info := infos[i]
			if info.Schema == "" {
				samples[i] = skipped(info, CodeNoSchema,
					"target has no schema to bind to WHERE SCHEMA_NAME = ?")
				continue
			}
			wg.Add(1)
			go func(idx int, info sqlstats.TargetInfo) {
				defer wg.Done()
				// Fail-open: a panic inside measurement must degrade one
				// target, never take the instrumented application down.
				defer func() {
					if r := recover(); r != nil {
						samples[idx] = skipped(info, CodeQueryError, fmt.Sprintf("panic while sampling: %v", r))
					}
				}()
				samples[idx], learned[idx] = c.captureTarget(ctx, info, phase, probes[info.ID], baseline)
			}(i, info)
		}
		wg.Wait()
		start = end
	}

	out := captureOutcome{
		sample: &Sample{Targets: make(map[string]*TargetSample, len(infos))},
		probes: map[string]probeResult{},
	}
	for i, info := range infos {
		sample := samples[i]
		if sample == nil {
			sample = skipped(info, CodeQueryError, "target was never visited")
		}
		out.sample.Targets[info.ID] = sample
		switch {
		case sample.Captured:
			out.captured++
		case sample.Code == CodeQueryError || sample.Code == CodeBudgetExhausted:
			out.failed++
		}
		if learned[i] != nil {
			out.probes[info.ID] = *learned[i]
		}
	}
	return out
}

// waveFits reports whether a whole wave still fits in the boundary budget.
func (c *Collector) waveFits(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return deadline.Sub(c.now()) >= c.perTarget
}

// captureTarget reads one target on its own connection, under its own slice of
// the boundary budget.
func (c *Collector) captureTarget(ctx context.Context, info sqlstats.TargetInfo, phase runctl.Phase, cached probeResult, baseline *Sample) (*TargetSample, *probeResult) {
	// A connection already known to carry a default database is never opened
	// again. Every statement on it — the registry's own session initialisation
	// included — lands in the measured schema, so re-asking the question would
	// keep paying the contamination this verdict exists to report.
	if cached.probed && cached.unsafeConn {
		return skipped(info, CodeInspectorDefaultDB, cached.reason), nil
	}

	targetCtx, cancel := context.WithTimeout(ctx, c.perTarget)
	defer cancel()

	var (
		sample  *TargetSample
		learned *probeResult
	)
	// PurposeStats is what guarantees the connection has no default database.
	// Nothing in this package may open a connection any other way.
	err := c.inspect(targetCtx, info.ID, sqlstats.PurposeStats, func(ctx context.Context, q sqlstats.Querier) error {
		sample, learned = captureOnConn(ctx, q, info, phase, cached, baseline)
		return nil
	})
	if err != nil {
		return skipped(info, CodeQueryError, fmt.Sprintf("inspect target: %v", err)), learned
	}
	if sample == nil {
		return skipped(info, CodeQueryError, "inspect returned without producing a sample"), learned
	}
	return sample, learned
}

// captureOnConn issues one boundary's statements on an already pinned
// connection: probe (first time only), metadata plus the pre-read clock, the
// full digest read, the post-read clock, and — at the closing boundary only —
// the digest texts of the leading rows.
func captureOnConn(ctx context.Context, q sqlstats.Querier, info sqlstats.TargetInfo, phase runctl.Phase, cached probeResult, baseline *Sample) (*TargetSample, *probeResult) {
	probe := cached
	var learned *probeResult
	if !probe.probed {
		probe = runProbe(ctx, q)
		learned = &probe
	}
	if sample := probeVerdict(info, probe); sample != nil {
		return sample, learned
	}

	meta, err := readMeta(ctx, q, probe.useSHOW)
	if err != nil {
		return skipped(info, CodeQueryError, fmt.Sprintf("read server metadata: %v", err)), learned
	}

	// A different server behind the same target is a different set of
	// capabilities, so the cached verdict cannot be carried over to it.
	if meta.serverUUID != "" && probe.serverUUID != "" && probe.serverUUID != meta.serverUUID {
		probe = runProbe(ctx, q)
		learned = &probe
		if sample := probeVerdict(info, probe); sample != nil {
			return sample, learned
		}
	}
	if meta.serverUUID != "" && probe.serverUUID != meta.serverUUID {
		probe.serverUUID = meta.serverUUID
		learned = &probe
	}

	digests, overflow, hasOverflow, err := readDigests(ctx, q, info.Schema)
	if err != nil {
		return skipped(info, CodeQueryError, fmt.Sprintf("read digest rows: %v", err)), learned
	}

	after, err := readClock(ctx, q)
	if err != nil {
		return skipped(info, CodeQueryError, fmt.Sprintf("read database clock: %v", err)), learned
	}

	sample := &TargetSample{
		TargetID:    info.ID,
		Schema:      info.Schema,
		ServerUUID:  meta.serverUUID,
		UptimeSec:   meta.uptimeSec,
		UTCBefore:   meta.utcBefore,
		UTCAfter:    after,
		Digests:     digests,
		Overflow:    overflow,
		HasOverflow: hasOverflow,
		Captured:    true,
	}
	if phase == runctl.PhaseFinishFinal {
		// The texts are fetched here, not in Collect, because Collect is
		// forbidden to do I/O — and which texts are worth fetching is only
		// known once the interval is approximately known.
		sample.Texts = captureTexts(ctx, q, info, baseline, digests)
	}
	return sample, learned
}

// probeVerdict turns an unusable probe result into a target sample, or returns
// nil when the target can be measured.
func probeVerdict(info sqlstats.TargetInfo, probe probeResult) *TargetSample {
	switch {
	case probe.unsafeConn:
		// Checked before the others: a connection that files this collector's
		// statements under the measured schema is a configuration fault of its
		// own, and reporting it as a plain probe skip would hide the one thing
		// the operator has to fix.
		return skipped(info, CodeInspectorDefaultDB, probe.reason)
	case probe.failed:
		return skipped(info, CodeQueryError, "capability probe failed: "+probe.reason)
	case !probe.supported:
		return skipped(info, CodeProbeSkip, probe.reason)
	default:
		return nil
	}
}

// captureTexts fetches DIGEST_TEXT for the digests that lead the provisional
// interval — the same ranking Collect will apply, computed here because Collect
// may not do I/O. A missing or unusable baseline is not an error: the section
// then shows the digests without their text rather than losing the numbers.
func captureTexts(ctx context.Context, q sqlstats.Querier, info sqlstats.TargetInfo, baseline *Sample, current map[string]DigestRow) map[string]string {
	base, ok := baseline.target(info.ID)
	if !ok || !base.Captured {
		return nil
	}
	top := rankDigests(intervalDigests(base.Digests, current), DigestTextFetchLimit)
	texts, err := readTexts(ctx, q, info.Schema, top)
	if err != nil {
		// The text is descriptive only; failing to read it must not cost the
		// interval it describes.
		return nil
	}
	return texts
}

// serverMeta is the metadata read that opens a boundary.
type serverMeta struct {
	serverUUID string
	uptimeSec  int64
	utcBefore  time.Time
}

// readMeta reads server identity, uptime and the pre-read database clock. On
// the performance_schema route that is one statement; on the SHOW route it is
// two, because SHOW cannot be folded into a select list.
func readMeta(ctx context.Context, q sqlstats.Querier, useSHOW bool) (serverMeta, error) {
	var meta serverMeta
	if useSHOW {
		var uuid, before any
		if err := q.QueryRowContext(ctx, metaSHOW).Scan(&uuid, &before); err != nil {
			return meta, fmt.Errorf("server identity: %w", err)
		}
		meta.serverUUID = toString(uuid)
		meta.utcBefore, _ = toTime(before)
		uptime, err := readUptimeSHOW(ctx, q)
		if err != nil {
			return meta, fmt.Errorf("uptime: %w", err)
		}
		meta.uptimeSec = uptime
		return meta, nil
	}
	var uuid, uptime, before any
	if err := q.QueryRowContext(ctx, metaPFS).Scan(&uuid, &uptime, &before); err != nil {
		return meta, fmt.Errorf("server identity and uptime: %w", err)
	}
	meta.serverUUID = toString(uuid)
	meta.uptimeSec = toInt64(uptime)
	meta.utcBefore, _ = toTime(before)
	return meta, nil
}

// readUptimeSHOW reads Uptime through SHOW GLOBAL STATUS, which returns the
// variable name alongside its value.
func readUptimeSHOW(ctx context.Context, q sqlstats.Querier) (int64, error) {
	rows, err := q.QueryContext(ctx, uptimeSHOW)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, value any
		if err := rows.Scan(&name, &value); err != nil {
			return 0, err
		}
		if strings.EqualFold(strings.TrimSpace(toString(name)), "Uptime") {
			return toInt64(value), rows.Err()
		}
	}
	return 0, rows.Err()
}

// readDigests reads every digest row of the bound schema plus the overflow
// row.
//
// The overflow row is identified by SCHEMA_NAME *and* DIGEST both being NULL.
// A NULL schema with a real digest is an ordinary statement executed without a
// default database — this collector's own statements look exactly like that —
// and is discarded rather than counted as overflow or as application traffic.
func readDigests(ctx context.Context, q sqlstats.Querier, schema string) (map[string]DigestRow, DigestRow, bool, error) {
	rows, err := q.QueryContext(ctx, digestRows, schema)
	if err != nil {
		return nil, DigestRow{}, false, err
	}
	defer func() { _ = rows.Close() }()

	digests := map[string]DigestRow{}
	var (
		overflow    DigestRow
		hasOverflow bool
	)
	for rows.Next() {
		var schemaName, digest any
		var counters [9]any
		dest := []any{
			&schemaName, &digest,
			&counters[0], &counters[1], &counters[2], &counters[3], &counters[4],
			&counters[5], &counters[6], &counters[7], &counters[8],
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, DigestRow{}, false, err
		}
		row := DigestRow{
			CountStar:            toUint64(counters[0]),
			TimerWait:            toUint64(counters[1]),
			RowsExamined:         toUint64(counters[2]),
			RowsSent:             toUint64(counters[3]),
			RowsAffected:         toUint64(counters[4]),
			CreatedTmpDiskTables: toUint64(counters[5]),
			SortMergePasses:      toUint64(counters[6]),
			NoIndexUsed:          toUint64(counters[7]),
			NoGoodIndexUsed:      toUint64(counters[8]),
		}
		schemaValue, schemaOK := nullableString(schemaName)
		digestValue, digestOK := nullableString(digest)
		switch {
		case !schemaOK && !digestOK:
			overflow, hasOverflow = row, true
		case schemaOK && digestOK && strings.EqualFold(schemaValue, schema):
			// EqualFold rather than ==: the server matched the row with its
			// own collation, and a case difference must not silently drop a
			// row it deliberately returned.
			digests[digestValue] = row
		default:
			// NULL schema with a digest, or another schema's row: not ours.
		}
	}
	if err := rows.Err(); err != nil {
		return nil, DigestRow{}, false, err
	}
	return digests, overflow, hasOverflow, nil
}

// readClock closes the boundary's database-clock bracket.
func readClock(ctx context.Context, q sqlstats.Querier) (time.Time, error) {
	var value any
	if err := q.QueryRowContext(ctx, clockAfter).Scan(&value); err != nil {
		return time.Time{}, err
	}
	stamp, _ := toTime(value)
	return stamp, nil
}

// readTexts fetches DIGEST_TEXT for the given digests. SCHEMA_NAME is bound
// alongside because (SCHEMA_NAME, DIGEST) is the table's primary key.
func readTexts(ctx context.Context, q sqlstats.Querier, schema string, digests []string) (map[string]string, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(digests)+1)
	args = append(args, schema)
	for _, digest := range digests {
		args = append(args, digest)
	}
	rows, err := q.QueryContext(ctx, digestTextQuery(len(digests)), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	texts := make(map[string]string, len(digests))
	for rows.Next() {
		var digest, text any
		if err := rows.Scan(&digest, &text); err != nil {
			return nil, err
		}
		key, ok := nullableString(digest)
		if !ok {
			continue
		}
		texts[key] = truncateBytes(toString(text), digestTextMaxBytes)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return texts, nil
}

// skipped builds a target sample that carries a reason instead of numbers.
func skipped(info sqlstats.TargetInfo, code, reason string) *TargetSample {
	return &TargetSample{
		TargetID: info.ID,
		Schema:   info.Schema,
		Code:     code,
		Err:      reason,
	}
}
