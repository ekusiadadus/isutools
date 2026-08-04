package sqlstats

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "collapses whitespace",
			in:   "SELECT *\n\tFROM   users\n WHERE id = ?",
			want: "SELECT * FROM users WHERE id = ?",
		},
		{
			name: "extracts tag comment",
			in:   "SELECT 1 /* getIndex */ FROM dual",
			want: "[getIndex] SELECT ? FROM dual",
		},
		{
			name: "drops non allowlisted tag and every ordinary comment",
			in:   "SELECT /* user@example.com */ 1 /* password=secret */ FROM dual -- bearer token\n",
			want: "SELECT ? FROM dual",
		},
		{
			name: "masks postgres dollar quoted literals",
			in:   "SELECT $$super-secret$$, $memo$private value$memo$ FROM users WHERE id = 42",
			want: "SELECT ?, ? FROM users WHERE id = ?",
		},
		{
			name: "masks hexadecimal and binary literals",
			in:   "SELECT 0xdeadbeef, X'cafebabe', B'101010'",
			want: "SELECT ?, ?, ?",
		},
		{
			name: "masks decimal and scientific literals",
			in:   "SELECT 1.25e+10, .5, 42e-3",
			want: "SELECT ?, ?, ?",
		},
		{
			name: "masks numeric literals",
			in:   "SELECT * FROM posts LIMIT 20 OFFSET 100",
			want: "SELECT * FROM posts LIMIT ? OFFSET ?",
		},
		{
			name: "keeps digits inside identifiers",
			in:   "SELECT col1 FROM t2 WHERE a = 5.5",
			want: "SELECT col1 FROM t2 WHERE a = ?",
		},
		{
			name: "masks string literals",
			in:   "SELECT * FROM users WHERE name = 'alice' AND memo = 'it''s ok'",
			want: "SELECT * FROM users WHERE name = ? AND memo = ?",
		},
		{
			name: "masks string literals with backslash escape",
			in:   `SELECT 1 FROM t WHERE a = 'it\'s'`,
			want: "SELECT ? FROM t WHERE a = ?",
		},
		{
			name: "truncates long queries",
			in:   "SELECT " + strings.Repeat("x", 2000),
			want: ("SELECT " + strings.Repeat("x", 2000))[:maxQueryLen],
		},
		{
			name: "placeholders unchanged",
			in:   "UPDATE t SET c = ?",
			want: "UPDATE t SET c = ?",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalize(tt.in); got != tt.want {
				t.Errorf("normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeNeverExceedsMaximumLengthIncludingTag(t *testing.T) {
	got := normalize("/* safe-tag */ SELECT " + strings.Repeat("x", maxQueryLen*2))
	if len(got) > maxQueryLen {
		t.Fatalf("normalized length = %d, want <= %d", len(got), maxQueryLen)
	}
}

func TestNormalizeCacheReturnsSameResult(t *testing.T) {
	q := "SELECT   cached\nFROM t"
	first := normalize(q)
	second := normalize(q)
	if first != second {
		t.Errorf("cache mismatch: %q vs %q", first, second)
	}
	if first != "SELECT cached FROM t" {
		t.Errorf("normalize = %q", first)
	}
}
