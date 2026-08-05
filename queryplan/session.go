package queryplan

import (
	"context"
	"strings"

	"github.com/ekusiadadus/isutools/sqlstats"
)

// session is what establish produced: either a reason to skip the target, or
// the one server setting the digest loop needs.
type session struct {
	// code is a reason ID, empty when the session may be used.
	code string
	// maxSQLTextLength is performance_schema_max_sql_text_length.
	maxSQLTextLength int
}

// establish prepares the pinned connection for EXPLAIN and returns a reason to
// skip the target if it cannot be prepared.
//
// All of it happens inside one Inspect call, on one connection, every time.
// Session state — the neutralised roles, the instrumentation flag, the default
// database — lives exactly as long as the connection does, so a verdict taken
// once at startup would be a verdict about a connection that no longer exists.
// The order is fixed:
//
//  1. roles and privileges, because everything after them is only safe if they
//     hold;
//  2. de-instrumentation, before USE, so that a session which fails the check
//     never had a default database and its statements were filed under a NULL
//     schema instead of the measured one;
//  3. the capability probe;
//  4. USE, which is what makes unqualified table names in a sample resolvable.
func establish(ctx context.Context, q sqlstats.Querier, schema string) session {
	if code := verifyPrivileges(ctx, q, schema); code != "" {
		return session{code: code}
	}
	if code := deinstrument(ctx, q); code != "" {
		return session{code: code}
	}
	maxLength, code := probe(ctx, q)
	if code != "" {
		return session{code: code}
	}
	if code := useSchema(ctx, q, schema); code != "" {
		return session{code: code}
	}
	return session{maxSQLTextLength: maxLength}
}

// verifyPrivileges neutralises roles and judges the effective privileges. See
// grants.go for why neutralisation alone is not enough.
func verifyPrivileges(ctx context.Context, q sqlstats.Querier, schema string) string {
	// A server too old for roles rejects this statement; the CURRENT_ROLE()
	// read below is what turns that into a verdict, so the error is not fatal
	// on its own.
	_, _ = q.ExecContext(ctx, stmtSetRoleNone)

	active, code := activeRoles(ctx, q)
	if code != "" {
		return code
	}
	lines, err := readGrants(ctx, q, stmtShowGrants)
	if err != nil {
		return CodeGrantsUnverifiable
	}
	grants, granted, ok := parseGrants(lines)
	if !ok {
		return CodeGrantsUnverifiable
	}
	// Granted roles are expanded even when none of them is active: a role
	// holding DML is a misconfiguration that must be reported now, not the
	// first time something re-activates it.
	if roles := dedupeAccounts(granted, active); len(roles) > 0 {
		expanded, code := expandRoles(ctx, q, roles)
		if code != "" {
			return code
		}
		grants = append(grants, expanded...)
	}
	if !allowed(grants, schema) {
		return CodeGrantsTooBroad
	}
	return ""
}

// maxRoleExpansions bounds the fixpoint below. Three nested roles is already
// more structure than a least-privilege EXPLAIN account has any reason to
// carry, and the bound is what keeps a cyclic or adversarial role graph from
// turning grant verification into an unbounded statement loop.
const maxRoleExpansions = 4

// expandRoles expands granted roles until the set of named roles stops growing
// and returns the privileges they carry.
//
// One round is not enough. SHOW GRANTS ... USING reports what the named roles
// carry, and a role may itself hold another role; whether the server walks that
// chain unasked is not something this check may assume, because the assumption
// is load bearing. An r_outer that holds nothing but r_dml would expand to a
// single membership row, and an expansion read only once would judge that as
// harmless — the DML would never appear in any output this package looked at.
// So every role a round names is fed back into the next USING list until a
// round adds nothing new.
//
// A graph that has not closed within maxRoleExpansions leaves some role
// unexpanded, and an unexpanded role is a privilege nobody has looked at:
// unverifiable, not allowed.
func expandRoles(ctx context.Context, q sqlstats.Querier, roles []account) ([]grantLine, string) {
	var grants []grantLine
	using := roles
	for round := 0; round < maxRoleExpansions; round++ {
		lines, err := readGrants(ctx, q, grantsUsing(using))
		if err != nil {
			return nil, CodeGrantsUnverifiable
		}
		expanded, named, ok := parseGrants(lines)
		if !ok {
			return nil, CodeGrantsUnverifiable
		}
		grants = append(grants, expanded...)
		next := dedupeAccounts(using, named)
		if len(next) == len(using) {
			return grants, ""
		}
		using = next
	}
	return nil, CodeGrantsUnverifiable
}

// roleNone is what CURRENT_ROLE() answers when no role is active. It is a
// literal string rather than a NULL, which is what makes the check below a
// positive one.
const roleNone = "NONE"

// activeRoles reads CURRENT_ROLE() back.
//
// Only the literal NONE establishes that SET ROLE NONE took effect. Anything
// else is either a role list — the documented fallback, where active roles are
// tolerated as long as the allowlist accepts what they carry — or an answer
// this package cannot interpret, and the empty answer belongs in that second
// group. A NULL CURRENT_ROLE() scans as "", and reading "" as "no roles are
// active" would be the same mistake as reading an empty SHOW GRANTS as "no
// privileges": absence of evidence taken for evidence of safety.
func activeRoles(ctx context.Context, q sqlstats.Querier) ([]account, string) {
	var value any
	if err := q.QueryRowContext(ctx, stmtCurrentRole).Scan(&value); err != nil {
		return nil, CodeRolesActive
	}
	current := strings.TrimSpace(toString(value))
	if current == roleNone {
		return nil, ""
	}
	roles, ok := parseAccountList(current)
	if !ok {
		return nil, CodeRolesActive
	}
	return roles, ""
}

// readGrants runs a SHOW GRANTS statement and returns its lines.
func readGrants(ctx context.Context, q sqlstats.Querier, statement string) ([]string, error) {
	rows, err := q.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var value any
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		lines = append(lines, toString(value))
		if len(lines) > maxGrantLines {
			break
		}
	}
	return lines, rows.Err()
}

// deinstrument turns this session's instrumentation off and verifies it.
//
// The UPDATE is allowed to fail — the recommended setup is a
// performance_schema.setup_actors row, which needs no privilege at run time —
// but the verification is not: an instrumented session would file its own
// EXPLAIN and USE statements as digests of the measured schema, and the next
// run would report this package's work as the application's.
func deinstrument(ctx context.Context, q sqlstats.Querier) string {
	if rows, err := q.QueryContext(ctx, stmtDeinstrument); err == nil {
		_ = rows.Close()
	}
	var value any
	if err := q.QueryRowContext(ctx, stmtInstrumented).Scan(&value); err != nil {
		return CodeSessionInstrumented
	}
	if !strings.EqualFold(strings.TrimSpace(toString(value)), "NO") {
		return CodeSessionInstrumented
	}
	return ""
}

// probe answers the two server-configuration questions: is there a recorded
// sample to explain at all, and at what length is it truncated.
func probe(ctx context.Context, q sqlstats.Querier) (int, string) {
	rows, err := q.QueryContext(ctx, stmtSampleColumn)
	if err != nil {
		return 0, CodeUnsupported
	}
	present := rows.Next()
	_ = rows.Close()
	if !present {
		return 0, CodeUnsupported
	}

	var value any
	if err := q.QueryRowContext(ctx, stmtMaxSQLTextLength).Scan(&value); err != nil {
		return defaultMaxSQLTextLength, ""
	}
	length := int(toInt64(value))
	if length <= 0 {
		// Assuming the documented default is the conservative choice: it makes
		// long samples look truncated and be skipped, rather than sending a
		// possibly incomplete statement to the server.
		return defaultMaxSQLTextLength, ""
	}
	return length, ""
}

// useSchema gives the connection a default database, which is what lets a
// sample's unqualified table names resolve. Without it every such sample fails
// with 1046 "No database selected".
//
// It is issued through QueryContext rather than ExecContext because the
// registry restricts ExecContext to session settings; USE returns an empty
// result set on the text protocol, so the result is simply closed.
func useSchema(ctx context.Context, q sqlstats.Querier, schema string) string {
	if !validSchema(schema) {
		return CodeNoSchema
	}
	rows, err := q.QueryContext(ctx, useStatement(schema))
	if err != nil {
		return CodeNoSchema
	}
	_ = rows.Close()
	return ""
}
