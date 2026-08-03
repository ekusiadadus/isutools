package accesslog

import (
	"strings"
	"testing"
)

func FuzzParseNginxLTSVNeverPanics(f *testing.F) {
	f.Add(strings.TrimSpace(lineA))
	f.Add(strings.TrimSpace(lineB))
	f.Add("")
	f.Add("method:GET\turi:/\x00\tstatus:200")
	f.Fuzz(func(t *testing.T, line string) {
		rec, err := ParseNginxLTSV(line)
		if err == nil {
			if rec.Method == "" || rec.URI == "" {
				t.Fatalf("successful parse omitted identity: %#v", rec)
			}
			if rec.RequestTime < 0 || rec.Bytes < 0 {
				t.Fatalf("successful parse returned negative values: %#v", rec)
			}
		}
	})
}
