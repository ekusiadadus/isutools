package sqlrows

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		text string
		want StatementKind
	}{
		{
			name: "plain select",
			text: "SELECT * FROM `posts` WHERE `id` = ?",
			want: KindSelect,
		},
		{
			name: "lower case select",
			text: "select `id` from `users`",
			want: KindSelect,
		},
		{
			name: "leading whitespace and newlines",
			text: "\n\t  SELECT ? FROM DUAL",
			want: KindSelect,
		},
		{
			name: "leading block comment",
			text: "/* app: isuconp */ SELECT * FROM `posts`",
			want: KindSelect,
		},
		{
			name: "leading line comment",
			text: "-- warm the cache\nSELECT * FROM `posts`",
			want: KindSelect,
		},
		{
			name: "hash comment",
			text: "# legacy\nSELECT * FROM `posts`",
			want: KindSelect,
		},
		{
			name: "parenthesised union",
			text: "( SELECT ? ) UNION ( SELECT ? )",
			want: KindSelect,
		},
		{
			name: "cte feeding a select is a select",
			text: "WITH `recent` AS ( SELECT * FROM `posts` WHERE `created_at` > ? ) SELECT * FROM `recent` JOIN `users` ON ...",
			want: KindSelect,
		},
		{
			name: "recursive cte feeding a select",
			text: "WITH RECURSIVE `tree` AS ( SELECT ? UNION ALL SELECT ? FROM `tree` ) SELECT COUNT (*) FROM `tree`",
			want: KindSelect,
		},
		{
			name: "several ctes feeding a select",
			text: "WITH `a` AS ( SELECT ? ) , `b` AS ( SELECT ? ) SELECT * FROM `a` , `b`",
			want: KindSelect,
		},
		{
			name: "cte feeding an insert is dml",
			text: "WITH `rows` AS ( SELECT ? ) INSERT INTO `t` SELECT * FROM `rows`",
			want: KindDML,
		},
		{
			name: "cte whose alias looks like a keyword",
			text: "WITH `delete_me` AS ( SELECT ? ) SELECT * FROM `delete_me`",
			want: KindSelect,
		},
		{
			name: "cte with nothing after the definition",
			text: "WITH `a` AS ( SELECT ? )",
			want: KindOther,
		},
		{name: "insert", text: "INSERT INTO `comments` (`body`) VALUES (?)", want: KindDML},
		{name: "update", text: "UPDATE `users` SET `name` = ? WHERE `id` = ?", want: KindDML},
		{name: "delete", text: "DELETE FROM `comments` WHERE `id` = ?", want: KindDML},
		{name: "replace", text: "REPLACE INTO `t` VALUES (?)", want: KindDML},
		{name: "ddl is other", text: "ALTER TABLE `posts` ADD INDEX (`user_id`)", want: KindOther},
		{name: "commit is other", text: "COMMIT", want: KindOther},
		{name: "empty text", text: "", want: KindOther},
		{name: "whitespace only", text: "   \n ", want: KindOther},
		{name: "unavailable digest text", text: MissingQueryText, want: KindOther},
		{
			name: "quoted identifier that spells a keyword",
			text: "`select` FROM `t`",
			want: KindOther,
		},
		{
			name: "unterminated block comment",
			text: "/* never closed SELECT",
			want: KindOther,
		},
		{
			name: "unterminated quote",
			text: "`unterminated",
			want: KindOther,
		},
		{
			name: "escaped quote inside a literal",
			text: "SELECT 'it\\'s fine' FROM `t`",
			want: KindSelect,
		},
		{
			name: "doubled quote inside an identifier",
			text: "SELECT * FROM `we``ird`",
			want: KindSelect,
		},
		{
			name: "multi byte identifier",
			text: "SELECT * FROM `投稿`",
			want: KindSelect,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.text); got != tc.want {
				t.Fatalf("Classify(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestClassifyIsBounded(t *testing.T) {
	// A pathological text must not make classification unbounded work.
	long := "WITH "
	for i := 0; i < 10000; i++ {
		long += "x "
	}
	if got := Classify(long); got != KindOther {
		t.Fatalf("Classify(long) = %q, want %q", got, KindOther)
	}
	if tokens := tokenizeSQL(long, classifyTokenLimit); len(tokens) != classifyTokenLimit {
		t.Fatalf("tokenizer produced %d tokens, want the limit of %d", len(tokens), classifyTokenLimit)
	}
}
