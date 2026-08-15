package analysisartifact

import (
	"bytes"
	"testing"
)

func FuzzInspectAndDecodeBounded(f *testing.F) {
	f.Add([]byte(`{"schema":"future/v2","kind":"future","status":"ready"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		if int64(len(body)) > MaxManifestBytes {
			t.Skip()
		}
		_, _ = Inspect(bytes.NewReader(body))
		_, _ = Decode(bytes.NewReader(body))
	})
}
