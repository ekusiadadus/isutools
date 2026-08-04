package advisor

import (
	"context"
	"strings"
	"testing"
)

func TestResponseCacheChecksSkipWithoutConf(t *testing.T) {
	m := byID(Collect(context.Background(), Options{}))
	for _, id := range []string{"nginx-proxy-cache", "nginx-proxy-cache-lock", "nginx-proxy-cache-set-cookie"} {
		if m[id].Status != StatusSkip {
			t.Errorf("%s = %q, want skip without nginx conf", id, m[id].Status)
		}
	}
}

func TestResponseCacheChecksSkipWithoutProxyPass(t *testing.T) {
	conf := "http { server { location /image/ { expires 1d; } } }"
	m := byID(Collect(context.Background(), Options{NginxConf: []byte(conf)}))
	if got := m["nginx-proxy-cache"].Status; got != StatusSkip {
		t.Errorf("static-only conf must skip proxy_cache check, got %q", got)
	}
}

func TestResponseCacheAdvisoryWhenProxyPassWithoutCache(t *testing.T) {
	conf := `
http {
  upstream app { server app:8080; }
  server { location / { proxy_pass http://app; } }
}`
	m := byID(Collect(context.Background(), Options{NginxConf: []byte(conf)}))
	c := m["nginx-proxy-cache"]
	if c.Status != StatusInfo {
		t.Errorf("proxy_pass without proxy_cache = %q, want info (caching is opt-in)", c.Status)
	}
	if m["nginx-proxy-cache-lock"].Status != StatusSkip {
		t.Errorf("lock check must skip while proxy_cache is disabled, got %q", m["nginx-proxy-cache-lock"].Status)
	}
	if m["nginx-proxy-cache-set-cookie"].Status != StatusSkip {
		t.Errorf("set-cookie check must skip while proxy_cache is disabled, got %q", m["nginx-proxy-cache-set-cookie"].Status)
	}
}

func TestResponseCacheProxyCacheOffIsDisabled(t *testing.T) {
	conf := "http { server { location / { proxy_pass http://app; proxy_cache off; } } }"
	m := byID(Collect(context.Background(), Options{NginxConf: []byte(conf)}))
	if got := m["nginx-proxy-cache"].Status; got != StatusInfo {
		t.Errorf("proxy_cache off must count as disabled, got %q", got)
	}
}

func TestResponseCacheThunderingHerdLock(t *testing.T) {
	noLock := `
http {
  proxy_cache_path /var/cache/nginx keys_zone=app:10m;
  server { location / { proxy_pass http://app; proxy_cache app; } }
}`
	m := byID(Collect(context.Background(), Options{NginxConf: []byte(noLock)}))
	if m["nginx-proxy-cache"].Status != StatusOK {
		t.Errorf("proxy_cache enabled = %q, want ok", m["nginx-proxy-cache"].Status)
	}
	c := m["nginx-proxy-cache-lock"]
	if c.Status != StatusWarn {
		t.Errorf("proxy_cache without proxy_cache_lock = %q, want warn", c.Status)
	}
	if !strings.Contains(c.Recommendation, "proxy_cache_lock") {
		t.Errorf("recommendation = %q", c.Recommendation)
	}

	withLock := `
http {
  proxy_cache_path /var/cache/nginx keys_zone=app:10m;
  server { location / { proxy_pass http://app; proxy_cache app; proxy_cache_lock on; } }
}`
	m = byID(Collect(context.Background(), Options{NginxConf: []byte(withLock)}))
	if got := m["nginx-proxy-cache-lock"].Status; got != StatusOK {
		t.Errorf("proxy_cache_lock on = %q, want ok", got)
	}
}

func TestResponseCacheSetCookieHazard(t *testing.T) {
	hazard := `
http {
  proxy_cache_path /var/cache/nginx keys_zone=app:10m;
  server {
    location / {
      proxy_pass http://app;
      proxy_cache app;
      proxy_cache_lock on;
      proxy_ignore_headers Cache-Control Set-Cookie;
    }
  }
}`
	m := byID(Collect(context.Background(), Options{NginxConf: []byte(hazard)}))
	c := m["nginx-proxy-cache-set-cookie"]
	if c.Status != StatusWarn {
		t.Errorf("proxy_ignore_headers Set-Cookie with proxy_cache = %q, want warn (session leak)", c.Status)
	}

	safe := `
http {
  proxy_cache_path /var/cache/nginx keys_zone=app:10m;
  server { location / { proxy_pass http://app; proxy_cache app; proxy_cache_lock on; } }
}`
	m = byID(Collect(context.Background(), Options{NginxConf: []byte(safe)}))
	if got := m["nginx-proxy-cache-set-cookie"].Status; got != StatusOK {
		t.Errorf("default Set-Cookie bypass = %q, want ok", got)
	}
}

func TestCacheTelemetryCheck(t *testing.T) {
	if got := cacheHealthCheck(nil, "").Status; got != StatusSkip {
		t.Errorf("nil telemetry = %q, want skip", got)
	}
	if got := cacheHealthCheck(nil, "boom").Status; got != StatusSkip {
		t.Errorf("telemetry error = %q, want skip", got)
	}
	if got := cacheHealthCheck(&CacheTelemetry{}, "").Status; got != StatusSkip {
		t.Errorf("zero telemetry = %q, want skip", got)
	}

	c := cacheHealthCheck(&CacheTelemetry{Hits: 900, Misses: 100, Evictions: 5}, "")
	if c.Status != StatusWarn {
		t.Errorf("pre-expiry evictions = %q, want warn (capacity pressure)", c.Status)
	}
	if !strings.Contains(c.Recommendation, "容量") {
		t.Errorf("recommendation = %q", c.Recommendation)
	}

	c = cacheHealthCheck(&CacheTelemetry{Hits: 10, Misses: 90}, "")
	if c.Status != StatusWarn {
		t.Errorf("10%% hit rate = %q, want warn", c.Status)
	}

	c = cacheHealthCheck(&CacheTelemetry{Hits: 900, Misses: 100}, "")
	if c.Status != StatusOK {
		t.Errorf("90%% hit rate without evictions = %q, want ok", c.Status)
	}
	if !strings.Contains(c.Detail, "90.0%") {
		t.Errorf("detail = %q, want hit rate", c.Detail)
	}
}

func TestWithCacheTelemetryReplacesStaticSkip(t *testing.T) {
	checks := Collect(context.Background(), Options{})
	replaced := WithCacheTelemetry(checks, &CacheTelemetry{Hits: 10, Misses: 90}, nil)
	if len(replaced) != len(checks) {
		t.Fatalf("check count changed: %d -> %d", len(checks), len(replaced))
	}
	if got := byID(replaced)["cache-app-telemetry"].Status; got != StatusWarn {
		t.Errorf("replaced telemetry check = %q, want warn", got)
	}
}
