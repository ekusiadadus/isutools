package queryplan

import "strings"

// Privilege verification.
//
// SHOW GRANTS alone is not an answer. It lists what was granted to the account
// directly plus the *names* of the roles it holds; the INSERT, UPDATE, DELETE
// and EXECUTE a role carries are not in that output. An account that reaches
// DML through a role would therefore read as harmless. So the check has two
// halves that are deliberately independent:
//
//   - neutralisation: SET ROLE NONE on the very connection the EXPLAIN will
//     run on, verified by reading CURRENT_ROLE() back rather than by assuming
//     the statement took effect;
//   - detection: every granted role is expanded with SHOW GRANTS ... USING and
//     judged by the same allowlist, so a dangerous role is a skip even when it
//     is currently inactive. A role that is inactive today is one connection
//     pool change away from being active tomorrow.
//
// The allowlist is closed: SELECT on the measured schema, SELECT on
// performance_schema, and UPDATE only on performance_schema.threads for
// de-instrumentation, USAGE, and role membership itself. EXECUTE is the one to
// keep out above all others — without it a SQL SECURITY DEFINER function
// cannot be reached through EXPLAIN at all, which is the whole reason EXPLAIN
// needs a dedicated user.

// performanceSchema is the only schema besides the measured one that a plan
// credential may read.
const performanceSchema = "performance_schema"

// threadsTable is the one table this package may write to, and only to turn
// its own session's instrumentation off.
const threadsTable = "threads"

// maxGrantLines bounds SHOW GRANTS output. A least-privilege account has a
// handful of lines; anything beyond this is not the account this feature is
// meant to run as, and the target is skipped rather than parsed at length.
const maxGrantLines = 256

// objectRef is the object a grant applies to. "*" stands for the wildcard in
// either position.
type objectRef struct {
	db    string
	table string
	// routine marks a FUNCTION or PROCEDURE grant. Routine privileges are
	// never in the allowlist, but the distinction is kept so the parser does
	// not have to pretend a routine is a table.
	routine bool
}

// grantLine is one parsed row of SHOW GRANTS.
type grantLine struct {
	// roles is non-empty for a "GRANT `r`@`%` TO `u`@`%`" row, which grants
	// membership rather than privileges.
	roles []account
	// privs are upper-cased privilege names, column lists stripped.
	privs []string
	obj   objectRef
	// grantOption records WITH GRANT OPTION, which is never allowed: an
	// account that can hand its privileges on is not least-privilege.
	grantOption bool
}

// parseGrants parses every line of SHOW GRANTS output. ok is false when the
// output was empty, and when any single line could not be parsed — an
// allowlist cannot judge syntax it does not understand, so one unknown line
// disqualifies the whole verdict.
//
// Empty is not "an account with no privileges". Every MySQL account has at
// least GRANT USAGE ON *.*, so a grant read that produced no lines is a read
// that did not happen: a result set stripped by a proxy, a statement the server
// answered with nothing, a driver returning an empty set for a form it did not
// recognise. That case has to be caught here, because everything downstream
// treats it as good news — an empty grant list yields no roles to expand and
// passes the allowlist vacuously, which would let EXPLAIN run on a credential
// whose privileges were never established. It is the one input that fails open,
// so it is rejected by construction.
func parseGrants(lines []string) (grants []grantLine, roles []account, ok bool) {
	if len(lines) == 0 || len(lines) > maxGrantLines {
		return nil, nil, false
	}
	for _, line := range lines {
		grant, parsed := parseGrant(line)
		if !parsed {
			return nil, nil, false
		}
		grants = append(grants, grant)
		roles = append(roles, grant.roles...)
	}
	return grants, roles, true
}

// parseGrant parses one SHOW GRANTS row.
func parseGrant(line string) (grantLine, bool) {
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ";"))
	if !strings.HasPrefix(strings.ToUpper(body), "GRANT ") {
		// REVOKE rows (partial revokes) and anything else unexpected land
		// here and make the verdict unverifiable.
		return grantLine{}, false
	}
	body = body[len("GRANT "):]

	on := indexTopLevel(body, " ON ")
	if on < 0 {
		return parseRoleGrant(body)
	}
	privs, ok := parsePrivileges(body[:on])
	if !ok {
		return grantLine{}, false
	}
	rest := body[on+len(" ON "):]
	to := indexTopLevel(rest, " TO ")
	if to < 0 {
		return grantLine{}, false
	}
	obj, ok := parseObject(rest[:to])
	if !ok {
		return grantLine{}, false
	}
	option, ok := parseGrantee(rest[to+len(" TO "):])
	if !ok {
		return grantLine{}, false
	}
	return grantLine{privs: privs, obj: obj, grantOption: option}, true
}

// parseRoleGrant parses "GRANT `r`@`%`, `s`@`%` TO `u`@`%`".
func parseRoleGrant(body string) (grantLine, bool) {
	to := indexTopLevel(body, " TO ")
	if to < 0 {
		return grantLine{}, false
	}
	roles, ok := parseAccountList(body[:to])
	if !ok {
		return grantLine{}, false
	}
	if _, ok := parseGrantee(body[to+len(" TO "):]); !ok {
		return grantLine{}, false
	}
	return grantLine{roles: roles}, true
}

// parsePrivileges splits the privilege list and strips column lists, so
// "SELECT (id, title)" is judged as the SELECT it is.
func parsePrivileges(list string) ([]string, bool) {
	var out []string
	for _, item := range splitTopLevel(list, ',') {
		name := strings.TrimSpace(item)
		if open := strings.IndexByte(name, '('); open >= 0 {
			name = strings.TrimSpace(name[:open])
		}
		if name == "" {
			return nil, false
		}
		out = append(out, strings.ToUpper(strings.Join(strings.Fields(name), " ")))
	}
	return out, len(out) > 0
}

// objectKinds are the object-type prefixes a grant may carry. They are
// mutually exclusive and listed in a fixed order so parsing is deterministic.
var objectKinds = []struct {
	prefix  string
	routine bool
}{
	{"TABLE ", false},
	{"FUNCTION ", true},
	{"PROCEDURE ", true},
}

// parseObject parses the db.table reference, including the optional TABLE,
// FUNCTION or PROCEDURE object type.
func parseObject(ref string) (objectRef, bool) {
	ref = strings.TrimSpace(ref)
	obj := objectRef{}
	for _, kind := range objectKinds {
		if strings.HasPrefix(strings.ToUpper(ref), kind.prefix) {
			obj.routine = kind.routine
			ref = strings.TrimSpace(ref[len(kind.prefix):])
			break
		}
	}
	db, next, ok := scanObjectPart(ref, 0)
	if !ok || next >= len(ref) || ref[next] != '.' {
		return objectRef{}, false
	}
	table, next, ok := scanObjectPart(ref, next+1)
	if !ok || skipSpace(ref, next) != len(ref) {
		return objectRef{}, false
	}
	obj.db, obj.table = db, table
	return obj, true
}

// scanObjectPart reads one side of a db.table reference, where "*" is legal
// and every other form is an identifier.
func scanObjectPart(s string, i int) (string, int, bool) {
	i = skipSpace(s, i)
	if i < len(s) && s[i] == '*' {
		return "*", i + 1, true
	}
	return scanIdent(s, i)
}

// parseGrantee reads the account a grant was made to and reports whether it
// carries WITH GRANT OPTION. Anything else trailing the account — an
// IDENTIFIED BY clause, say — makes the line unparseable rather than ignored.
func parseGrantee(rest string) (grantOption bool, ok bool) {
	_, next, ok := parseAccount(rest, 0)
	if !ok {
		return false, false
	}
	tail := strings.ToUpper(strings.Join(strings.Fields(rest[next:]), " "))
	switch tail {
	case "":
		return false, true
	case "WITH GRANT OPTION":
		return true, true
	default:
		return false, false
	}
}

// allowed reports whether every parsed grant stays inside the allowlist.
func allowed(grants []grantLine, schema string) bool {
	for _, grant := range grants {
		if !grantAllowed(grant, schema) {
			return false
		}
	}
	return true
}

func grantAllowed(grant grantLine, schema string) bool {
	switch {
	case len(grant.roles) > 0:
		// Membership alone confers nothing; the role's own privileges are
		// judged from the SHOW GRANTS ... USING expansion.
		return true
	case grant.grantOption, grant.obj.routine:
		return false
	}
	for _, priv := range grant.privs {
		if !privAllowed(priv, grant.obj, schema) {
			return false
		}
	}
	return true
}

// privAllowed is the allowlist itself. Everything not named here — INSERT,
// UPDATE outside performance_schema.threads, DELETE, EXECUTE, ALL PRIVILEGES,
// the dynamic *_ADMIN privileges, PROXY — is a reason to skip the target.
func privAllowed(priv string, obj objectRef, schema string) bool {
	switch priv {
	case "USAGE":
		// USAGE is "no privileges"; it is the row every account has.
		return true
	case "SELECT":
		return matchesDB(obj, schema) || matchesDB(obj, performanceSchema)
	case "UPDATE":
		return matchesDB(obj, performanceSchema) &&
			strings.EqualFold(obj.table, threadsTable)
	default:
		return false
	}
}

// matchesDB reports whether obj names exactly the given database.
//
// "*.*" never matches: SELECT on every schema reaches mysql.user, which is a
// credential store, and reading it is not what EXPLAIN needs. A database part
// containing "%" never matches either — it is a LIKE pattern in a grant, and
// `%`.* is "*.*" spelled differently.
func matchesDB(obj objectRef, db string) bool {
	if obj.db == "*" || strings.Contains(obj.db, "%") {
		return false
	}
	return strings.EqualFold(obj.db, db)
}

// dedupeAccounts merges role lists, keeping first-seen order so the generated
// USING clause is deterministic.
func dedupeAccounts(lists ...[]account) []account {
	seen := map[account]bool{}
	var out []account
	for _, list := range lists {
		for _, acct := range list {
			if seen[acct] {
				continue
			}
			seen[acct] = true
			out = append(out, acct)
		}
	}
	return out
}
