package queryplan

import "strings"

// Every statement this package can issue is listed here, because the exact set
// is part of the contract: the ABBA overhead gate counts them, and each one is
// paid by the measured database. Three rules hold for all of them.
//
//  1. The schema is a bound parameter, never interpolated. The inspection
//     connection has no default database on purpose, so DATABASE() is NULL and
//     unusable, and interpolateParams is off.
//  2. The one exception is USE, because an identifier cannot be a bind
//     parameter. Its argument passes validSchema first and is backtick-quoted.
//  3. Everything from stmtDeinstrument onwards runs on a session that has been
//     proven uninstrumented, so none of it appears in the digest table the
//     next run measures.
const (
	// stmtSetRoleNone drops every active role, so only directly granted
	// privileges remain in force for this session.
	stmtSetRoleNone = `SET ROLE NONE`

	// stmtCurrentRole verifies that. mandatory_roles and
	// activate_all_roles_on_login can put roles back, so the effect is read
	// rather than assumed.
	stmtCurrentRole = `SELECT CURRENT_ROLE()`

	// stmtShowGrants lists directly granted privileges and role membership.
	stmtShowGrants = `SHOW GRANTS FOR CURRENT_USER()`

	// stmtShowGrantsUsing expands what the granted roles carry. Without it a
	// DML privilege held through a role is invisible.
	stmtShowGrantsUsing = `SHOW GRANTS FOR CURRENT_USER() USING `

	// stmtDeinstrument stops performance_schema from recording this session.
	// It is allowed to fail: the operator may have used setup_actors instead,
	// and stmtInstrumented is what decides either way.
	stmtDeinstrument = `UPDATE performance_schema.threads SET INSTRUMENTED = 'NO' ` +
		`WHERE PROCESSLIST_ID = CONNECTION_ID()`

	// stmtInstrumented verifies the session really is uninstrumented. It runs
	// before USE, so that a session which fails the check has still never had
	// a default database and its statements were filed under a NULL schema.
	stmtInstrumented = `SELECT INSTRUMENTED FROM performance_schema.threads ` +
		`WHERE PROCESSLIST_ID = CONNECTION_ID()`

	// stmtSampleColumn probes for QUERY_SAMPLE_TEXT, which is what makes this
	// feature possible at all and what MySQL 5.7 and MariaDB lack.
	stmtSampleColumn = `SELECT COLUMN_NAME FROM information_schema.COLUMNS ` +
		`WHERE TABLE_SCHEMA = 'performance_schema' ` +
		`AND TABLE_NAME = 'events_statements_summary_by_digest' ` +
		`AND COLUMN_NAME = 'QUERY_SAMPLE_TEXT'`

	// stmtMaxSQLTextLength reads the length at which the server truncates a
	// recorded sample. A sample that reached it may end mid-statement.
	stmtMaxSQLTextLength = `SELECT @@performance_schema_max_sql_text_length`

	// samplePrefix begins the one statement that reads every sample.
	//
	// SCHEMA_NAME is bound alongside the digests because (SCHEMA_NAME, DIGEST)
	// is the table's primary key: the same statement text executed against
	// another database is a different row, and a DIGEST-only condition would
	// happily return that other schema's sample — its literals included — to
	// be explained against this one.
	samplePrefix = `SELECT DIGEST, QUERY_SAMPLE_TEXT, QUERY_SAMPLE_SEEN ` +
		`FROM performance_schema.events_statements_summary_by_digest ` +
		`WHERE SCHEMA_NAME = ? AND DIGEST IN (`

	// explainPrefix is prepended to the sample text. The result of that
	// concatenation is a local variable in the digest loop and is stored
	// nowhere.
	explainPrefix = `EXPLAIN `
)

// sampleQuery builds the sample read for n digests. n is bounded by MaxTop and
// the statement is only issued when n > 0.
func sampleQuery(n int) string {
	var b strings.Builder
	b.Grow(len(samplePrefix) + 3*n + 1)
	b.WriteString(samplePrefix)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("?")
	}
	b.WriteString(")")
	return b.String()
}

// useStatement builds "USE `schema`". The caller has already checked the name
// with validSchema; quoting is the second line of defence, not the first.
func useStatement(schema string) string { return "USE " + quoteIdent(schema) }

// grantsUsing builds the role expansion statement. Role names come from the
// server's own output and are re-quoted rather than pasted back.
func grantsUsing(roles []account) string {
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		parts = append(parts, role.quoted())
	}
	return stmtShowGrantsUsing + strings.Join(parts, ", ")
}
