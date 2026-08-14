package accesslog

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEveryDocumentedProxyFixture(t *testing.T) {
	file, err := os.Open("../examples/proxies/fixtures.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close fixtures: %v", err)
		}
	}()
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			t.Fatalf("malformed fixture line: %q", line)
		}
		format, err := ParseFormat(parts[1])
		if err != nil {
			t.Fatalf("%s: %v", parts[0], err)
		}
		record, err := ParseLineFormat(format, parts[2])
		if err != nil {
			t.Fatalf("%s: %v", parts[0], err)
		}
		if record.Method != "GET" || record.URI != "/items/42" || record.Status != 200 || record.RequestTime != 25*time.Millisecond || record.Bytes != 123 {
			t.Errorf("%s: %#v", parts[0], record)
		}
		seen[parts[0]] = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 12 {
		t.Fatalf("covered products = %d, want 12: %#v", len(seen), seen)
	}
}

func TestProxyExamplesUseTrustedResponseLabels(t *testing.T) {
	proxyDir := "../examples/proxies"
	tests := []struct {
		file      string
		required  []string
		forbidden []string
	}{
		{
			file: "nginx.conf",
			required: []string{
				"$upstream_http_x_isutools_session", "proxy_hide_header X-Isutools-Session",
			},
			forbidden: []string{"$http_x_isutools_session"},
		},
		{
			file:      "apache.conf",
			required:  []string{"%{X-Isutools-Session}o", "Header always unset X-Isutools-Session"},
			forbidden: []string{"%{X-Isutools-Session}i\""},
		},
	}
	for _, test := range tests {
		t.Run(test.file, func(t *testing.T) {
			contents, err := os.ReadFile(filepath.Join(proxyDir, test.file))
			if err != nil {
				t.Fatal(err)
			}
			text := string(contents)
			for _, value := range test.required {
				if !strings.Contains(text, value) {
					t.Errorf("required trusted-response contract missing: %q", value)
				}
			}
			for _, value := range test.forbidden {
				if strings.Contains(text, value) {
					t.Errorf("public request label used by logging contract: %q", value)
				}
			}
		})
	}
}
