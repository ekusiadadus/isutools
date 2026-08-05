package advisor

import (
	"context"
	"io/fs"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func somaxconnFS(value string) fstest.MapFS {
	return fstest.MapFS{"proc/sys/net/core/somaxconn": {Data: []byte(value + "\n")}}
}

func TestNginxUpstreamUDS(t *testing.T) {
	tests := []struct {
		name       string
		conf       string
		want       Status
		wantDetail []string
	}{
		{
			name: "loopback http proxy_pass is an opportunity",
			conf: "http { server { location / { proxy_pass http://127.0.0.1:8080; } } }",
			want: StatusInfo, wantDetail: []string{"127.0.0.1:8080", "proxy_pass"},
		},
		{
			name: "localhost with port is loopback",
			conf: "http { server { location / { proxy_pass http://localhost:8080/; } } }",
			want: StatusInfo, wantDetail: []string{"localhost:8080"},
		},
		{
			name: "loopback without port is loopback",
			conf: "http { server { location / { proxy_pass http://127.0.0.1; } } }",
			want: StatusInfo, wantDetail: []string{"127.0.0.1"},
		},
		{
			name: "127.0.0.2 is still loopback",
			conf: "http { server { location / { proxy_pass http://127.0.0.2:8080; } } }",
			want: StatusInfo, wantDetail: []string{"127.0.0.2:8080"},
		},
		{
			name: "IPv6 loopback literal",
			conf: "http { server { location / { proxy_pass http://[::1]:8080; } } }",
			want: StatusInfo, wantDetail: []string{"[::1]:8080"},
		},
		{
			name: "routable IPv6 literal is another host",
			conf: "http { server { location / { proxy_pass http://[2001:db8::1]:8080; } } }",
			want: StatusSkip, wantDetail: []string{"同一ホストではない"},
		},
		{
			name: "unterminated IPv6 bracket is not assumed to be local",
			conf: "http { server { location / { proxy_pass http://[::1:8080; } } }",
			want: StatusSkip, wantDetail: []string{"同一ホストではない"},
		},
		{
			name: "empty authority cannot be resolved",
			conf: "http { server { location / { proxy_pass http://; } } }",
			want: StatusSkip, wantDetail: []string{"静的に解決できない"},
		},
		{
			name: "unnamed upstream block still reports its loopback server",
			conf: "http { upstream { server 127.0.0.1:8080; } }",
			want: StatusInfo, wantDetail: []string{"127.0.0.1:8080(upstream)"},
		},
		{
			name: "variable in an upstream server address",
			conf: "http { upstream app { server $backend:8080; } }",
			want: StatusSkip, wantDetail: []string{"静的に解決できない"},
		},
		{
			name: "https to loopback is excluded because UDS drops TLS",
			conf: "http { server { location / { proxy_pass https://localhost:8443; } } }",
			want: StatusSkip, wantDetail: []string{"https"},
		},
		{
			name: "upstream block indirection to loopback",
			conf: `http {
  upstream app { server 127.0.0.1:8080; keepalive 64; }
  server { location / { proxy_pass http://app; } }
}`,
			want: StatusInfo, wantDetail: []string{"127.0.0.1:8080", "upstream app"},
		},
		{
			name: "https through an upstream name is excluded",
			conf: `http {
  upstream app { server 127.0.0.1:8443; }
  server { location / { proxy_pass https://app; } }
}`,
			want: StatusSkip, wantDetail: []string{"https"},
		},
		{
			name: "unix socket upstream is already done",
			conf: `http {
  upstream app { server unix:/run/isuconapp/app.sock; }
  server { location / { proxy_pass http://app; } }
}`,
			want: StatusOK, wantDetail: []string{"unix:"},
		},
		{
			name: "unix socket written inline in proxy_pass",
			conf: "http { server { location / { proxy_pass http://unix:/run/isuconapp/app.sock:/; } } }",
			want: StatusOK, wantDetail: []string{"unix:"},
		},
		{
			name: "container service name is not the same host",
			conf: `http {
  upstream app_backend { server app:8080; }
  server { location / { proxy_pass http://app_backend; } }
}`,
			want: StatusSkip, wantDetail: []string{"同一ホストではない"},
		},
		{
			name: "another host by IP is not the same host",
			conf: "http { server { location / { proxy_pass http://192.168.0.12:8080; } } }",
			want: StatusSkip, wantDetail: []string{"同一ホストではない"},
		},
		{
			name: "commented out proxy_pass is ignored",
			conf: "http { server { location / { # proxy_pass http://127.0.0.1:8080;\n root /var/www; } } }",
			want: StatusSkip, wantDetail: []string{"proxy_pass がなく対象外"},
		},
		{
			name: "multiple server blocks report the loopback one",
			conf: `http {
  upstream sock { server unix:/run/isuconapp/app.sock; }
  server { listen 80; location / { proxy_pass http://sock; } }
  server { listen 8080; location /api { proxy_pass http://127.0.0.1:9000; } }
}`,
			want: StatusInfo, wantDetail: []string{"127.0.0.1:9000"},
		},
		{
			name: "variable destination cannot be resolved statically",
			conf: "http { server { location / { proxy_pass http://$backend; } } }",
			want: StatusSkip, wantDetail: []string{"静的に解決できない"},
		},
		{
			name: "schemeless proxy_pass cannot be resolved statically",
			conf: "http { server { location / { proxy_pass 127.0.0.1:8080; } } }",
			want: StatusSkip, wantDetail: []string{"静的に解決できない"},
		},
		{
			name: "no conf at all",
			conf: "",
			want: StatusSkip, wantDetail: []string{"ISUTOOLS_NGINX_CONF"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkUpstreamUDS(Options{NginxConf: []byte(tt.conf)})
			if got.ID != "nginx-upstream-uds" {
				t.Fatalf("id = %q", got.ID)
			}
			if got.Status != tt.want {
				t.Errorf("status = %q (%s), want %q", got.Status, got.Detail, tt.want)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail = %q, want it to contain %q", got.Detail, want)
				}
			}
		})
	}
}

func TestNginxUpstreamUDSRecommendsRunNotTmp(t *testing.T) {
	c := checkUpstreamUDS(Options{
		NginxConf: []byte("http { server { location / { proxy_pass http://127.0.0.1:8080; } } }"),
	})
	if c.Status != StatusInfo {
		t.Fatalf("status = %q, want info", c.Status)
	}
	if strings.Contains(c.Recommendation, "/tmp") {
		t.Errorf("recommendation must not point at /tmp (world-writable, PrivateTmp): %q", c.Recommendation)
	}
	for _, want := range []string{"/run/", "0750", "0660", "前提"} {
		if !strings.Contains(c.Recommendation, want) {
			t.Errorf("recommendation = %q, want it to contain %q", c.Recommendation, want)
		}
	}
}

func TestNginxListenBacklog(t *testing.T) {
	tests := []struct {
		name       string
		conf       string
		fsys       fs.FS
		want       Status
		wantDetail []string
		wantRec    string
	}{
		{
			name: "no backlog with modern somaxconn is an opportunity",
			conf: "http { server { listen 80; } }",
			fsys: somaxconnFS("4096"),
			want: StatusInfo, wantDetail: []string{"somaxconn=4096", "0.0.0.0:80 backlog 未指定", "511"},
			wantRec: "backlog=4096",
		},
		{
			name: "no backlog with low somaxconn is not reported twice",
			conf: "http { server { listen 80; } }",
			fsys: somaxconnFS("1024"),
			want: StatusOK, wantDetail: []string{"0.0.0.0:80 backlog 未指定"},
		},
		{
			name: "explicit backlog matching somaxconn is ok",
			conf: "http { server { listen 80 backlog=8192; } }",
			fsys: somaxconnFS("4096"),
			want: StatusOK, wantDetail: []string{"0.0.0.0:80 backlog=8192"},
		},
		{
			name: "backlog far below somaxconn is an opportunity",
			conf: "http { server { listen 80 backlog=256; } }",
			fsys: somaxconnFS("4096"),
			want: StatusInfo, wantDetail: []string{"0.0.0.0:80 backlog=256"},
		},
		{
			name: "wildcard forms normalize to the same endpoint",
			conf: `http {
  server { listen 80 default_server backlog=8192; }
  server { listen *:80; server_name isu.example.com; }
}`,
			fsys: somaxconnFS("4096"),
			want: StatusOK, wantDetail: []string{"0.0.0.0:80 backlog=8192"},
		},
		{
			name: "IPv6 wildcard is a separate socket",
			conf: "http { server { listen 80; listen [::]:80 backlog=8192; } }",
			fsys: somaxconnFS("4096"),
			want: StatusInfo, wantDetail: []string{"0.0.0.0:80 backlog 未指定", "[::]:80 backlog=8192"},
		},
		{
			name: "explicit IPv4 address keeps its address",
			conf: "http { server { listen 127.0.0.1:8080 backlog=8192; } }",
			fsys: somaxconnFS("4096"),
			want: StatusOK, wantDetail: []string{"127.0.0.1:8080 backlog=8192"},
		},
		{
			name: "listen on a unix socket is judged too",
			conf: "http { server { listen unix:/run/nginx/http.sock; } }",
			fsys: somaxconnFS("4096"),
			want: StatusInfo, wantDetail: []string{"unix:/run/nginx/http.sock backlog 未指定"},
		},
		{
			name: "duplicate backlog on one endpoint is not judged",
			conf: `http {
  server { listen 80 backlog=8192; }
  server { listen 80 backlog=4096; }
  server { listen 8443; }
}`,
			fsys: somaxconnFS("4096"),
			want: StatusInfo, wantDetail: []string{"判定対象外: 1 件", "backlog= が 2 箇所", "0.0.0.0:8443"},
		},
		{
			name: "only a duplicated endpoint leaves nothing to judge",
			conf: "http { server { listen 80 backlog=8192; } server { listen 80 backlog=4096; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解析できる listen が見つかりませんでした", "backlog= が 2 箇所"},
		},
		{
			name: "unreadable backlog value is not judged",
			conf: "http { server { listen 80 backlog=auto; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解釈できない backlog="},
		},
		{
			name: "commented out listen is ignored",
			conf: "http { server {\n# listen 80 backlog=8192;\nlisten 80;\n} }",
			fsys: somaxconnFS("4096"),
			want: StatusInfo, wantDetail: []string{"0.0.0.0:80 backlog 未指定"},
		},
		{
			name: "quoted semicolons do not end a directive",
			conf: "http { log_format main '$remote_addr \"$request\";'; server { listen 80 backlog=8192; } }",
			fsys: somaxconnFS("4096"),
			want: StatusOK, wantDetail: []string{"0.0.0.0:80 backlog=8192"},
		},
		{
			name: "stream listen is out of scope",
			conf: "stream { server { listen 3306; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解析できる listen", "stream/mail"},
		},
		{
			name: "mail listen is out of scope",
			conf: "mail { server { listen 143; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"stream/mail"},
		},
		{
			name: "listen outside a server block is not judged",
			conf: "http { listen 80; }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"server ブロック外"},
		},
		{
			name: "listen with no enclosing block at all",
			conf: "listen 80;",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"server ブロック外"},
		},
		{
			name: "listen with no argument",
			conf: "http { server { listen; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"引数のない listen"},
		},
		{
			name: "unix listen without a path",
			conf: "http { server { listen unix:; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解釈できない listen"},
		},
		{
			name: "bracketed address without a port",
			conf: "http { server { listen [::]; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解釈できない listen"},
		},
		{
			name: "non numeric port",
			conf: "http { server { listen 127.0.0.1:http; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解釈できない listen"},
		},
		{
			name: "address without a port is not guessed",
			conf: "http { server { listen 127.0.0.1; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解釈できない listen"},
		},
		{
			name: "hostname listen is not guessed",
			conf: "http { server { listen isu.example.com:80; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解釈できない listen"},
		},
		{
			name: "variable listen is not guessed",
			conf: "http { server { listen $port; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解釈できない listen"},
		},
		{
			name: "out of range port is not judged",
			conf: "http { server { listen 70000; } }",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"解釈できない listen"},
		},
		{
			name: "somaxconn file missing",
			conf: "http { server { listen 80; } }",
			fsys: fstest.MapFS{},
			want: StatusSkip, wantDetail: []string{"somaxconn を読めない"},
		},
		{
			name: "no filesystem at all",
			conf: "http { server { listen 80; } }",
			want: StatusSkip, wantDetail: []string{"somaxconn を読めない"},
		},
		{
			name: "no conf at all",
			fsys: somaxconnFS("4096"),
			want: StatusSkip, wantDetail: []string{"ISUTOOLS_NGINX_CONF"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkListenBacklog(Options{NginxConf: []byte(tt.conf), FS: tt.fsys})
			if got.ID != "nginx-listen-backlog" {
				t.Fatalf("id = %q", got.ID)
			}
			if got.Status != tt.want {
				t.Errorf("status = %q (%s), want %q", got.Status, got.Detail, tt.want)
			}
			for _, want := range tt.wantDetail {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			if tt.wantRec != "" && !strings.Contains(got.Recommendation, tt.wantRec) {
				t.Errorf("recommendation = %q, want it to contain %q", got.Recommendation, tt.wantRec)
			}
			if got.Status != StatusInfo && got.Recommendation != "" {
				t.Errorf("only an opportunity carries a recommendation; got %q for %q", got.Recommendation, got.Status)
			}
		})
	}
}

func TestNginxListenBacklogSummarizesManyEndpoints(t *testing.T) {
	var b strings.Builder
	b.WriteString("http {")
	for _, port := range []int{80, 81, 82, 83, 84, 85, 86, 87} {
		b.WriteString("server { listen " + strconv.Itoa(port) + "; }")
	}
	b.WriteString("}")
	c := checkListenBacklog(Options{NginxConf: []byte(b.String()), FS: somaxconnFS("4096")})
	if c.Status != StatusInfo {
		t.Fatalf("status = %q (%s), want info", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "他 2 件") {
		t.Errorf("detail = %q, want the tail to be summarized", c.Detail)
	}
}

func TestPGOClassification(t *testing.T) {
	buildInfo := func(settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Settings: settings}, true
		}
	}
	tests := []struct {
		name       string
		read       func() (*debug.BuildInfo, bool)
		want       Status
		wantDetail string
		wantRec    string
	}{
		{
			name:       "profile applied",
			read:       buildInfo(debug.BuildSetting{Key: "-pgo", Value: "/src/app/default.pgo"}),
			want:       StatusOK,
			wantDetail: "-pgo=/src/app/default.pgo",
		},
		{
			name:       "explicitly disabled",
			read:       buildInfo(debug.BuildSetting{Key: "-pgo", Value: "off"}),
			want:       StatusInfo,
			wantDetail: "PGO なし",
			wantRec:    "default.pgo",
		},
		{
			name:       "empty value counts as absent",
			read:       buildInfo(debug.BuildSetting{Key: "-pgo", Value: ""}),
			want:       StatusInfo,
			wantDetail: "PGO なし",
		},
		{
			name:       "no pgo setting at all",
			read:       buildInfo(debug.BuildSetting{Key: "-trimpath", Value: "true"}),
			want:       StatusInfo,
			wantDetail: "PGO なし",
			wantRec:    "*_cpu.pprof",
		},
		{
			name:       "buildinfo unavailable",
			read:       func() (*debug.BuildInfo, bool) { return nil, false },
			want:       StatusSkip,
			wantDetail: "buildinfo を読めない",
		},
		{
			name:       "buildinfo nil despite ok",
			read:       func() (*debug.BuildInfo, bool) { return nil, true },
			want:       StatusSkip,
			wantDetail: "buildinfo を読めない",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkPGO(tt.read)
			if got.ID != "go-pgo" {
				t.Fatalf("id = %q", got.ID)
			}
			if got.Status != tt.want {
				t.Errorf("status = %q (%s), want %q", got.Status, got.Detail, tt.want)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tt.wantDetail)
			}
			if tt.wantRec != "" && !strings.Contains(got.Recommendation, tt.wantRec) {
				t.Errorf("recommendation = %q, want it to contain %q", got.Recommendation, tt.wantRec)
			}
		})
	}
}

func TestTransportChecksAreCollected(t *testing.T) {
	conf := `http {
  upstream app { server 127.0.0.1:8080; }
  server { listen 80; location / { proxy_pass http://app; } }
}`
	m := byID(Collect(context.Background(), Options{
		NginxConf: []byte(conf),
		FS:        fakeOS("4096", "32768\t60999", goodLimits, "max 100000"),
	}))
	for _, id := range []string{"nginx-upstream-uds", "nginx-listen-backlog", "go-pgo"} {
		c, ok := m[id]
		if !ok {
			t.Fatalf("%s check missing from Collect", id)
		}
		if c.Status == "" {
			t.Errorf("%s has no status", id)
		}
	}
	if got := m["nginx-upstream-uds"].Status; got != StatusInfo {
		t.Errorf("loopback upstream = %q, want info", got)
	}
	if got := m["nginx-listen-backlog"].Status; got != StatusInfo {
		t.Errorf("listen without backlog at somaxconn=4096 = %q, want info", got)
	}
}

func TestSomaxconnDetailMentionsKernelDefault(t *testing.T) {
	m := byID(Collect(context.Background(), Options{
		FS: fakeOS("128", "32768\t60999", goodLimits, "max 100000"),
	}))
	c := m["os-somaxconn"]
	if c.Status != StatusWarn {
		t.Fatalf("somaxconn 128 = %q, want warn (threshold unchanged)", c.Status)
	}
	for _, want := range []string{"4096", "128"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail = %q, want it to mention the kernel default %q", c.Detail, want)
		}
	}
	if !strings.Contains(c.Recommendation, "8192") {
		t.Errorf("recommendation = %q, want the book's worked example", c.Recommendation)
	}
}

func TestParseNginxStmtsTracksBlocksAndLines(t *testing.T) {
	conf := `http {
  upstream app {
    server 127.0.0.1:8080;
  }
  server {
    listen 80;
  }
}`
	stmts := parseNginxStmts(conf)
	if len(stmts) != 2 {
		t.Fatalf("statements = %d, want 2: %+v", len(stmts), stmts)
	}
	if got := stmts[0].innermost().name; got != "upstream" {
		t.Errorf("innermost block = %q, want upstream", got)
	}
	if got := stmts[0].innermost().args; len(got) != 1 || got[0] != "app" {
		t.Errorf("upstream args = %v, want [app]", got)
	}
	if stmts[0].line != 3 {
		t.Errorf("line = %d, want 3", stmts[0].line)
	}
	if !stmts[1].enclosedBy("http") {
		t.Errorf("listen should be enclosed by http: %+v", stmts[1].stack)
	}
	if stmts[1].line != 6 {
		t.Errorf("line = %d, want 6", stmts[1].line)
	}
	if got := len(parseNginxStmts("}\nlisten 80;")); got != 1 {
		t.Errorf("unbalanced close brace must not lose later statements, got %d", got)
	}

	// A quoted value may span lines and carry ";" or "{" without ending the
	// directive; the line counter must keep up for the directives after it.
	multiline := parseNginxStmts("http {\n  log_format main 'a;b\n{c';\n  server { listen 80; }\n}")
	if len(multiline) != 2 {
		t.Fatalf("statements = %d, want 2: %+v", len(multiline), multiline)
	}
	if multiline[1].words[0] != "listen" || multiline[1].line != 4 {
		t.Errorf("statement after a multi-line quote = %v (line %d), want listen on line 4",
			multiline[1].words, multiline[1].line)
	}
}

func TestParseNginxStmtsBoundsPathologicalInput(t *testing.T) {
	var flat strings.Builder
	for i := 0; i < maxNginxStatements+10; i++ {
		flat.WriteString("listen 80;")
	}
	if got := len(parseNginxStmts(flat.String())); got != maxNginxStatements {
		t.Errorf("statements = %d, want them capped at %d", got, maxNginxStatements)
	}

	var deep strings.Builder
	for i := 0; i < maxNginxBlockDepth+4; i++ {
		deep.WriteString("server {")
	}
	deep.WriteString("listen 80;")
	stmts := parseNginxStmts(deep.String())
	if len(stmts) != 1 {
		t.Fatalf("statements = %d, want 1", len(stmts))
	}
	if got := len(stmts[0].stack); got != maxNginxBlockDepth {
		t.Errorf("block depth = %d, want it capped at %d", got, maxNginxBlockDepth)
	}
}
