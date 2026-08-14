package sqlstats

import (
	"strings"
	"testing"
)

func TestCommentTagPolicyOffMergesSafeTagsAndScrubsSecrets(t *testing.T) {
	queries := []string{
		"SELECT * FROM users WHERE id=42 /* getUser */",
		"SELECT * FROM users WHERE id=99 /* getAdmin */",
		"SELECT * FROM users WHERE id=7 /* password=secret */",
	}
	for _, query := range queries {
		got := computeNormalizeWithPolicy(query, CommentTagsOff)
		if got != "SELECT * FROM users WHERE id=?" {
			t.Fatalf("normalize(%q) = %q", query, got)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "getUser") {
			t.Fatalf("normalized query leaked comment: %q", got)
		}
	}
}

func TestCommentTagPolicyOnIsBackwardCompatible(t *testing.T) {
	if got := computeNormalizeWithPolicy("SELECT 1 /* getIndex */", CommentTagsOn); got != "[getIndex] SELECT ?" {
		t.Fatalf("tag-on normalize = %q", got)
	}
}

func TestResolveCommentTagPolicy(t *testing.T) {
	for _, value := range []string{"off", "0", "false", "disabled"} {
		policy, code := ResolveCommentTagPolicy(func(string) string { return value })
		if policy != CommentTagsOff || code != "configured-off" {
			t.Fatalf("value %q = (%q,%q)", value, policy, code)
		}
	}
	policy, code := ResolveCommentTagPolicy(func(string) string { return "unsafe-secret" })
	if policy != CommentTagsOn || code != "invalid-value" || strings.Contains(code, "unsafe-secret") {
		t.Fatalf("invalid policy = (%q,%q)", policy, code)
	}
}
