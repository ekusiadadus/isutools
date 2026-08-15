package slowlog

import (
	"strings"
	"testing"
)

func FuzzParseBounded(f *testing.F) {
	f.Add("# Query_time: 0.1 Lock_time: 0 Rows_sent: 1 Rows_examined: 2\nSELECT 1;\n")
	f.Add("# User@Host: private[private] @ localhost\npartial")
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 64<<10 {
			t.Skip()
		}
		_, _ = Parse(strings.NewReader(input), Options{MaxInputBytes: 64 << 10, MaxLineBytes: 8 << 10, MaxEvents: 1000, MaxClasses: 100, MaxQueryBytes: 8 << 10})
	})
}
