package advisor

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

func fakeOS(somaxconn, portRange, limits, cpuMax string) fstest.MapFS {
	return fstest.MapFS{
		"proc/sys/net/core/somaxconn":           {Data: []byte(somaxconn + "\n")},
		"proc/sys/net/ipv4/ip_local_port_range": {Data: []byte(portRange + "\n")},
		"proc/self/limits":                      {Data: []byte(limits)},
		"sys/fs/cgroup/cpu.max":                 {Data: []byte(cpuMax + "\n")},
	}
}

const goodLimits = `Limit                     Soft Limit           Hard Limit           Units
Max open files            1048576              1048576              files
`

const lowLimits = `Limit                     Soft Limit           Hard Limit           Units
Max open files            1024                 4096                 files
`

func byID(checks []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range checks {
		m[c.ID] = c
	}
	return m
}

func TestDSNInterpolateParams(t *testing.T) {
	checks := Collect(context.Background(), Options{
		DriverName: "mysql",
		DSN:        "root:root@tcp(mysql:3306)/isuconp?charset=utf8mb4&parseTime=true",
		FS:         fakeOS("4096", "32768\t60999", goodLimits, "max 100000"),
	})
	c, ok := byID(checks)["dsn-interpolate-params"]
	if !ok {
		t.Fatal("dsn-interpolate-params check missing")
	}
	if c.Status != StatusMissing {
		t.Errorf("status = %q, want missing when interpolateParams is absent", c.Status)
	}

	checks = Collect(context.Background(), Options{
		DriverName: "mysql",
		DSN:        "root:root@tcp(mysql:3306)/isuconp?interpolateParams=true",
		FS:         fakeOS("4096", "32768\t60999", goodLimits, "max 100000"),
	})
	if got := byID(checks)["dsn-interpolate-params"].Status; got != StatusOK {
		t.Errorf("status = %q, want ok when set", got)
	}
}

func TestNginxConfChecks(t *testing.T) {
	conf := `
worker_processes auto;
events { worker_connections 4096; }
http {
  sendfile on;
  upstream app { server app:8080; keepalive 64; }
  server {
    location /image/ { expires 1d; }
  }
}`
	checks := Collect(context.Background(), Options{
		NginxConf: []byte(conf),
		FS:        fakeOS("4096", "32768\t60999", goodLimits, "max 100000"),
	})
	m := byID(checks)
	for _, id := range []string{"nginx-gzip"} {
		if m[id].Status != StatusMissing {
			t.Errorf("%s = %q, want missing", id, m[id].Status)
		}
	}
	for _, id := range []string{"nginx-keepalive", "nginx-worker-connections", "nginx-sendfile", "nginx-expires"} {
		if m[id].Status != StatusOK {
			t.Errorf("%s = %q, want ok", id, m[id].Status)
		}
	}
}

func TestNginxConfAbsent(t *testing.T) {
	checks := Collect(context.Background(), Options{
		FS: fakeOS("4096", "32768\t60999", goodLimits, "max 100000"),
	})
	if got := byID(checks)["nginx-gzip"].Status; got != StatusSkip {
		t.Errorf("without conf, nginx checks must be skip; got %q", got)
	}
}

func TestNginxKeepaliveTimeoutIsNotUpstreamKeepalive(t *testing.T) {
	checks := Collect(context.Background(), Options{
		NginxConf: []byte("http { keepalive_timeout 65; upstream app { server app:8080; } }"),
	})
	if got := byID(checks)["nginx-keepalive"].Status; got != StatusMissing {
		t.Errorf("keepalive_timeout must not satisfy upstream keepalive: %q", got)
	}
}

func TestOSChecks(t *testing.T) {
	checks := Collect(context.Background(), Options{
		FS: fakeOS("128", "49152\t50000", lowLimits, "max 100000"),
	})
	m := byID(checks)
	if m["os-somaxconn"].Status != StatusWarn {
		t.Errorf("somaxconn 128 must warn, got %q", m["os-somaxconn"].Status)
	}
	if m["os-port-range"].Status != StatusWarn {
		t.Errorf("narrow port range must warn, got %q", m["os-port-range"].Status)
	}
	if m["os-nofile"].Status != StatusWarn {
		t.Errorf("nofile 1024 must warn, got %q", m["os-nofile"].Status)
	}
}

func TestGOMAXPROCSVsQuota(t *testing.T) {
	checks := Collect(context.Background(), Options{
		GOMAXPROCS: 24,
		FS:         fakeOS("4096", "32768\t60999", goodLimits, "100000 100000"),
	})
	c := byID(checks)["go-gomaxprocs"]
	if c.Status != StatusWarn {
		t.Errorf("GOMAXPROCS 24 with 1-CPU quota must warn, got %q", c.Status)
	}
	if !strings.Contains(c.Recommendation, "GOMAXPROCS") {
		t.Errorf("recommendation = %q", c.Recommendation)
	}

	checks = Collect(context.Background(), Options{
		GOMAXPROCS: 2,
		FS:         fakeOS("4096", "32768\t60999", goodLimits, "100000 100000"),
	})
	if got := byID(checks)["go-gomaxprocs"].Status; got != StatusOK {
		t.Errorf("GOMAXPROCS 2 with 1-CPU quota should be ok, got %q", got)
	}
}

func TestChecksAreSortedBySeverity(t *testing.T) {
	checks := Collect(context.Background(), Options{
		DriverName: "mysql",
		DSN:        "root@tcp(db)/x",
		FS:         fakeOS("128", "32768\t60999", goodLimits, "max 100000"),
	})
	rank := map[Status]int{StatusMissing: 0, StatusWarn: 1, StatusInfo: 2, StatusOK: 3, StatusSkip: 4}
	for i := 1; i < len(checks); i++ {
		if rank[checks[i-1].Status] > rank[checks[i].Status] {
			t.Fatalf("checks not sorted by severity: %v then %v", checks[i-1].Status, checks[i].Status)
		}
	}
}
