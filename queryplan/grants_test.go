package queryplan

import (
	"strings"
	"testing"
)

// The grant fixtures below are real SHOW GRANTS output shapes: MySQL 8
// backtick-quotes both identifiers and account names, puts role membership on
// its own GRANT ... TO line, and reports column-level grants with a
// parenthesised column list.

func TestParseGrantAcceptsRealOutput(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		privs []string
		db    string
		table string
		roles []account
		grant bool
	}{
		{
			name:  "usage",
			line:  "GRANT USAGE ON *.* TO `isutools_explain`@`%`",
			privs: []string{"USAGE"},
			db:    "*", table: "*",
		},
		{
			name:  "schema select",
			line:  "GRANT SELECT ON `isuconp`.* TO `isutools_explain`@`%`",
			privs: []string{"SELECT"},
			db:    "isuconp", table: "*",
		},
		{
			name:  "table select",
			line:  "GRANT SELECT ON `performance_schema`.`threads` TO `u`@`localhost`",
			privs: []string{"SELECT"},
			db:    "performance_schema", table: "threads",
		},
		{
			name:  "column select keeps the privilege, drops the columns",
			line:  "GRANT SELECT (`id`, `title`) ON `isuconp`.`posts` TO `u`@`%`",
			privs: []string{"SELECT"},
			db:    "isuconp", table: "posts",
		},
		{
			name:  "several privileges",
			line:  "GRANT SELECT, INSERT, CREATE TEMPORARY TABLES ON `isuconp`.* TO `u`@`%`",
			privs: []string{"SELECT", "INSERT", "CREATE TEMPORARY TABLES"},
			db:    "isuconp", table: "*",
		},
		{
			name:  "single quoted account",
			line:  "GRANT SELECT ON `isuconp`.* TO 'u'@'%'",
			privs: []string{"SELECT"},
			db:    "isuconp", table: "*",
		},
		{
			name:  "with grant option",
			line:  "GRANT SELECT ON `isuconp`.* TO `u`@`%` WITH GRANT OPTION",
			privs: []string{"SELECT"},
			db:    "isuconp", table: "*",
			grant: true,
		},
		{
			name:  "role membership",
			line:  "GRANT `r_dml`@`%`,`r_ro`@`%` TO `isutools_explain`@`%`",
			roles: []account{{name: "r_dml", host: "%"}, {name: "r_ro", host: "%"}},
		},
		{
			name:  "backticks are doubled inside identifiers",
			line:  "GRANT SELECT ON `we``ird`.* TO `u`@`%`",
			privs: []string{"SELECT"},
			db:    "we`ird", table: "*",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseGrant(tc.line)
			if !ok {
				t.Fatalf("parseGrant(%q) reported the line as unparseable", tc.line)
			}
			if len(tc.roles) > 0 {
				if len(got.roles) != len(tc.roles) {
					t.Fatalf("roles = %+v, want %+v", got.roles, tc.roles)
				}
				for i, role := range tc.roles {
					if got.roles[i] != role {
						t.Fatalf("role %d = %+v, want %+v", i, got.roles[i], role)
					}
				}
				return
			}
			if strings.Join(got.privs, "|") != strings.Join(tc.privs, "|") {
				t.Fatalf("privs = %v, want %v", got.privs, tc.privs)
			}
			if got.obj.db != tc.db || got.obj.table != tc.table {
				t.Fatalf("object = %q.%q, want %q.%q", got.obj.db, got.obj.table, tc.db, tc.table)
			}
			if got.grantOption != tc.grant {
				t.Fatalf("grantOption = %v, want %v", got.grantOption, tc.grant)
			}
		})
	}
}

func TestParseGrantRejectsWhatItCannotJudge(t *testing.T) {
	// An allowlist may only be applied to syntax it fully understands, so a
	// line outside the known grammar has to disqualify the whole verdict
	// rather than be skipped.
	lines := []string{
		"REVOKE INSERT ON `mysql`.* FROM `u`@`%`",
		"GRANT SELECT ON `isuconp`.*",
		"GRANT ON `isuconp`.* TO `u`@`%`",
		"GRANT SELECT ON isuconp TO `u`@`%`",
		"GRANT SELECT ON `isuconp`.* TO `u`@`%` IDENTIFIED BY PASSWORD 'secret'",
		"GRANT SELECT ON `isuconp`.* TO `u`@`%` WITH MAX_QUERIES_PER_HOUR 10",
		"GRANT `r_dml`@`%`",
		"",
	}
	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			if _, ok := parseGrant(line); ok {
				t.Fatalf("parseGrant(%q) accepted a line it cannot judge", line)
			}
		})
	}
}

func TestAllowedEnforcesTheAllowlist(t *testing.T) {
	const schema = "isuconp"
	tests := []struct {
		name  string
		lines []string
		want  bool
	}{
		{
			name:  "documented least-privilege setup",
			lines: leastPrivilegeGrants,
			want:  true,
		},
		{
			name: "select on a single performance_schema table",
			lines: []string{
				"GRANT USAGE ON *.* TO `u`@`%`",
				"GRANT SELECT ON `isuconp`.* TO `u`@`%`",
				"GRANT SELECT ON `performance_schema`.`events_statements_summary_by_digest` TO `u`@`%`",
			},
			want: true,
		},
		{
			name:  "column level select on the measured schema",
			lines: []string{"GRANT SELECT (`id`) ON `isuconp`.`posts` TO `u`@`%`"},
			want:  true,
		},
		{
			name:  "role membership alone confers nothing",
			lines: []string{"GRANT `r_ro`@`%` TO `u`@`%`"},
			want:  true,
		},
		{
			name:  "select on every schema reaches the credential store",
			lines: []string{"GRANT SELECT ON *.* TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "a wildcard database is *.* spelled differently",
			lines: []string{"GRANT SELECT ON `isu%`.* TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "insert",
			lines: []string{"GRANT SELECT, INSERT ON `isuconp`.* TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "update outside performance_schema.threads",
			lines: []string{"GRANT UPDATE ON `isuconp`.* TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "update on all performance_schema tables",
			lines: []string{"GRANT UPDATE ON `performance_schema`.* TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "delete",
			lines: []string{"GRANT DELETE ON `isuconp`.`posts` TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "execute reaches a definer function through EXPLAIN",
			lines: []string{"GRANT EXECUTE ON `isuconp`.* TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "execute on one routine",
			lines: []string{"GRANT EXECUTE ON FUNCTION `isuconp`.`touch_rows` TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "all privileges",
			lines: []string{"GRANT ALL PRIVILEGES ON `isuconp`.* TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "grant option",
			lines: []string{"GRANT SELECT ON `isuconp`.* TO `u`@`%` WITH GRANT OPTION"},
			want:  false,
		},
		{
			name:  "dynamic privileges",
			lines: []string{"GRANT BACKUP_ADMIN,SYSTEM_VARIABLES_ADMIN ON *.* TO `u`@`%`"},
			want:  false,
		},
		{
			name:  "another schema",
			lines: []string{"GRANT SELECT ON `otherdb`.* TO `u`@`%`"},
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grants, _, ok := parseGrants(tc.lines)
			if !ok {
				t.Fatalf("parseGrants(%v) failed", tc.lines)
			}
			if got := allowed(grants, schema); got != tc.want {
				t.Fatalf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseGrantsCollectsRolesForExpansion(t *testing.T) {
	lines := append(append([]string(nil), leastPrivilegeGrants...),
		"GRANT `r_dml`@`%` TO `isutools_explain`@`%`",
		"GRANT `r_ro`@`localhost` TO `isutools_explain`@`%`",
	)
	grants, roles, ok := parseGrants(lines)
	if !ok {
		t.Fatal("parseGrants failed on realistic output")
	}
	if !allowed(grants, "isuconp") {
		t.Fatal("membership rows alone must not disqualify the account")
	}
	if len(roles) != 2 || roles[0].name != "r_dml" || roles[1].host != "localhost" {
		t.Fatalf("roles = %+v, want r_dml@%% and r_ro@localhost", roles)
	}
	// The expansion statement re-quotes what the server reported rather than
	// pasting the line back.
	want := "SHOW GRANTS FOR CURRENT_USER() USING `r_dml`@`%`, `r_ro`@`localhost`"
	if got := grantsUsing(roles); got != want {
		t.Fatalf("grantsUsing = %q, want %q", got, want)
	}
}

// TestParseGrantsRejectsNoLines: an empty grant read is the one input that
// would fail open. Every MySQL account holds at least GRANT USAGE ON *.*, so no
// lines means the read did not happen — but an empty list yields no roles to
// expand and satisfies the allowlist vacuously, which would let EXPLAIN run on
// a credential whose privileges were never established.
func TestParseGrantsRejectsNoLines(t *testing.T) {
	for _, lines := range [][]string{nil, {}} {
		grants, roles, ok := parseGrants(lines)
		if ok {
			t.Fatalf("parseGrants(%#v) reported an empty grant read as verified", lines)
		}
		if grants != nil || roles != nil {
			t.Fatalf("parseGrants(%#v) = %+v / %+v, want nothing to judge", lines, grants, roles)
		}
	}
	// And the allowlist really would have passed it, which is why the check
	// belongs at the parse step rather than at the judgement.
	if !allowed(nil, "isuconp") {
		t.Fatal("allowed(nil) is false: this test no longer guards what it was written for")
	}
}

func TestParseGrantsRejectsTooManyLines(t *testing.T) {
	lines := make([]string, maxGrantLines+1)
	for i := range lines {
		lines[i] = "GRANT USAGE ON *.* TO `u`@`%`"
	}
	if _, _, ok := parseGrants(lines); ok {
		t.Fatal("an account with hundreds of grant lines is not the least-privilege account this feature runs as")
	}
}

func TestParseAccountList(t *testing.T) {
	tests := []struct {
		in    string
		want  []account
		valid bool
	}{
		{in: "`r_dml`@`%`", want: []account{{name: "r_dml", host: "%"}}, valid: true},
		{in: "`r_a`@`%`,`r_b`@`%`", want: []account{{name: "r_a", host: "%"}, {name: "r_b", host: "%"}}, valid: true},
		{in: "`r_a`@`%`, `r_b`@`localhost`", want: []account{{name: "r_a", host: "%"}, {name: "r_b", host: "localhost"}}, valid: true},
		{in: "'r_a'@'%'", want: []account{{name: "r_a", host: "%"}}, valid: true},
		{in: "r_a", want: []account{{name: "r_a"}}, valid: true},
		{in: "", valid: false},
		{in: "`r_a`@`%` `r_b`@`%`", valid: false},
		{in: "`unterminated", valid: false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseAccountList(tc.in)
			if ok != tc.valid {
				t.Fatalf("parseAccountList(%q) ok = %v, want %v", tc.in, ok, tc.valid)
			}
			if !tc.valid {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("accounts = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("account %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestIndexTopLevelIgnoresQuotedText(t *testing.T) {
	// A table called "SEASON ON ICE" contains the separator the grant parser
	// splits on; splitting there would cut an identifier in half.
	line := "GRANT SELECT ON `isuconp`.`season on ice` TO `u`@`%`"
	grant, ok := parseGrant(line)
	if !ok {
		t.Fatalf("parseGrant(%q) failed", line)
	}
	if grant.obj.table != "season on ice" {
		t.Fatalf("table = %q, want %q", grant.obj.table, "season on ice")
	}
	if !allowed([]grantLine{grant}, "isuconp") {
		t.Fatal("select on one table of the measured schema is inside the allowlist")
	}
}

func TestValidSchemaRejectsIdentifiersItCannotQuote(t *testing.T) {
	valid := []string{"isuconp", "isu_conp", "DB$1", strings.Repeat("a", maxSchemaLen)}
	invalid := []string{"", "isu conp", "isu`conp", "isu-conp", "isu.conp", "isu\\conp", strings.Repeat("a", maxSchemaLen+1)}
	for _, name := range valid {
		if !validSchema(name) {
			t.Fatalf("validSchema(%q) = false, want true", name)
		}
	}
	for _, name := range invalid {
		if validSchema(name) {
			t.Fatalf("validSchema(%q) = true, want false", name)
		}
	}
	if got := useStatement("isuconp"); got != "USE `isuconp`" {
		t.Fatalf("useStatement = %q", got)
	}
}
