package sqlrows

import "strings"

// Every statement this package issues is listed here, because the exact set is
// part of the contract: plan 04 counts them per boundary and the ABBA overhead
// gate asserts the count. Two rules hold for all of them.
//
//  1. No statement may call DATABASE() — nor its synonym SCHEMA(). The
//     connection has no default database, so DATABASE() is NULL; more
//     importantly, a connection that did have one would file this collector's
//     own statements under the application's schema and they would appear in
//     every interval. probeDefaultSchema exists to catch exactly that, and it
//     reads the session's default database from performance_schema rather than
//     from DATABASE() so this rule needs no exception.
//  2. The schema is a bound parameter. It is never interpolated into the text,
//     both to keep one digest per statement and because the inspector DSN
//     disables interpolateParams.
const (
	// probePerformanceSchema answers whether performance_schema exists at all.
	probePerformanceSchema = `SELECT @@performance_schema`

	// probeDefaultSchema answers whether the connection this collector was
	// handed really has no default database.
	//
	// The registry can only strip the database from a DSN it can rebuild; a
	// URL-form DSN is passed to the driver unchanged and keeps the
	// application's schema. Every statement below would then be recorded
	// against that schema and counted in the very interval this package
	// measures, so the property is verified on the connection instead of
	// assumed from the DSN — which also catches a driver, proxy or connection
	// hook that selects a database behind the registry's back.
	//
	// PROCESSLIST_DB is the session's default database as performance_schema
	// itself sees it. It is NULL exactly when there is none.
	probeDefaultSchema = `SELECT PROCESSLIST_DB FROM performance_schema.threads ` +
		`WHERE PROCESSLIST_ID = CONNECTION_ID()`

	// probeDigestConsumer answers whether statement digests are being
	// recorded. The table exists and stays empty when this consumer is off,
	// which would otherwise look like "no traffic".
	probeDigestConsumer = `SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME = 'statements_digest'`

	// probeColumns lists the digest table's columns. MariaDB and older MySQL
	// releases lack some of them, and plan 09 needs to know whether
	// QUERY_SAMPLE_TEXT exists.
	probeColumns = `SELECT COLUMN_NAME FROM information_schema.COLUMNS ` +
		`WHERE TABLE_SCHEMA = 'performance_schema' AND TABLE_NAME = 'events_statements_summary_by_digest'`

	// probeUptime decides how Uptime is read. When it fails the collector
	// falls back to SHOW GLOBAL STATUS, which cannot be folded into the
	// metadata statement and therefore costs one statement more per boundary.
	probeUptime = `SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Uptime'`

	// metaPFS folds server identity, uptime and the pre-read database clock
	// into a single statement.
	metaPFS = `SELECT @@server_uuid AS server_uuid, ` +
		`(SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Uptime') AS uptime_sec, ` +
		`UTC_TIMESTAMP(6) AS db_utc_before`

	// metaSHOW is the same statement without the uptime sub-select, used when
	// performance_schema.global_status is unavailable.
	metaSHOW = `SELECT @@server_uuid AS server_uuid, UTC_TIMESTAMP(6) AS db_utc_before`

	// uptimeSHOW is the fallback uptime read.
	uptimeSHOW = `SHOW GLOBAL STATUS LIKE 'Uptime'`

	// digestRows reads every digest of the bound schema plus the overflow row.
	// There is no LIMIT: truncating here would attribute a digest's whole
	// history to the interval the first time it enters the top of the table.
	//
	// SCHEMA_NAME is selected even though the WHERE clause already constrains
	// it, so the both-NULL overflow rule is applied to the returned rows in Go
	// rather than inferred from the predicate.
	digestRows = `SELECT SCHEMA_NAME, DIGEST, COUNT_STAR, SUM_TIMER_WAIT, ` +
		`SUM_ROWS_EXAMINED, SUM_ROWS_SENT, SUM_ROWS_AFFECTED, ` +
		`SUM_CREATED_TMP_DISK_TABLES, SUM_SORT_MERGE_PASSES, ` +
		`SUM_NO_INDEX_USED, SUM_NO_GOOD_INDEX_USED ` +
		`FROM performance_schema.events_statements_summary_by_digest ` +
		`WHERE SCHEMA_NAME = ? OR (SCHEMA_NAME IS NULL AND DIGEST IS NULL)`

	// clockAfter closes the boundary's database-clock bracket.
	clockAfter = `SELECT UTC_TIMESTAMP(6) AS db_utc_after`

	// digestTextPrefix begins the digest-text read. SCHEMA_NAME is repeated
	// because the table's primary key is (SCHEMA_NAME, DIGEST): the same
	// digest executed from another default database is a different row, and
	// without the schema the text of a foreign row could be joined onto our
	// numbers.
	digestTextPrefix = `SELECT DIGEST, LEFT(DIGEST_TEXT, 512) AS digest_text ` +
		`FROM performance_schema.events_statements_summary_by_digest ` +
		`WHERE SCHEMA_NAME = ? AND DIGEST IN (`
)

// digestTextQuery builds the digest-text read for n digests. n is bounded by
// DigestTextFetchLimit, and the statement is only issued when n > 0.
func digestTextQuery(n int) string {
	var b strings.Builder
	b.Grow(len(digestTextPrefix) + 3*n + 1)
	b.WriteString(digestTextPrefix)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("?")
	}
	b.WriteString(")")
	return b.String()
}

// requiredColumns are the digest-table columns the interval is built from. A
// missing one makes the target unmeasurable rather than partially measurable,
// because every advisor check reads several of them together.
var requiredColumns = []string{
	"SCHEMA_NAME",
	"DIGEST",
	"DIGEST_TEXT",
	"COUNT_STAR",
	"SUM_TIMER_WAIT",
	"SUM_ROWS_EXAMINED",
	"SUM_ROWS_SENT",
	"SUM_ROWS_AFFECTED",
	"SUM_CREATED_TMP_DISK_TABLES",
	"SUM_SORT_MERGE_PASSES",
	"SUM_NO_INDEX_USED",
	"SUM_NO_GOOD_INDEX_USED",
}

// optionalQuerySampleColumn is not required here; plan 09 uses it for EXPLAIN
// freshness and only needs to know whether it exists.
const optionalQuerySampleColumn = "QUERY_SAMPLE_TEXT"
