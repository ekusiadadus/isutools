package advisor

import (
	"fmt"
	"regexp"
)

// Caching is opt-in: a missing proxy_cache is advisory (StatusInfo), never a
// defect. Once enabled, the hazards become real defects: thundering herd
// without proxy_cache_lock, and session leakage when Set-Cookie responses
// are forced into a shared cache.

var (
	nginxProxyPass       = regexp.MustCompile(`(?m)(?:^|[;{}])\s*proxy_pass\s`)
	nginxProxyCache      = regexp.MustCompile(`(?m)(?:^|[;{}])\s*proxy_cache\s+([^;\s]+)\s*;`)
	nginxProxyCacheLock  = regexp.MustCompile(`(?m)(?:^|[;{}])\s*proxy_cache_lock\s+on\s*;`)
	nginxIgnoreSetCookie = regexp.MustCompile(`(?im)(?:^|[;{}])\s*proxy_ignore_headers[^;]*\bSet-Cookie\b[^;]*;`)
)

// CacheTelemetry is an interval-aligned application cache snapshot
// (memcached "stats", redis/valkey INFO stats, or equivalent). HTTP
// middleware cannot observe cache internals, so values must be explicit.
type CacheTelemetry struct {
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
	// Evictions counts entries removed before their TTL expired
	// (capacity pressure), e.g. memcached evictions / redis evicted_keys.
	Evictions uint64 `json:"evictions"`
}

func checkResponseCache(opts Options) []Check {
	mk := func(id, title string) Check { return Check{ID: id, Title: title} }
	checks := []Check{
		mk("nginx-proxy-cache", "nginx: proxy_cache(レスポンスキャッシュ)"),
		mk("nginx-proxy-cache-lock", "nginx: proxy_cache_lock(thundering herd 対策)"),
		mk("nginx-proxy-cache-set-cookie", "nginx: Set-Cookie 応答のキャッシュ回避"),
	}
	if len(opts.NginxConf) == 0 {
		for i := range checks {
			checks[i].Status = StatusSkip
			checks[i].Detail = "ISUTOOLS_NGINX_CONF 未設定(conf を読めるようにすると検査できます)"
		}
		return checks
	}
	conf := stripComments(string(opts.NginxConf))
	if !nginxProxyPass.MatchString(conf) {
		for i := range checks {
			checks[i].Status = StatusSkip
			checks[i].Detail = "proxy_pass がなく対象外"
		}
		return checks
	}

	enabled := false
	for _, m := range nginxProxyCache.FindAllStringSubmatch(conf, -1) {
		if m[1] != "off" {
			enabled = true
			break
		}
	}
	if !enabled {
		checks[0].Status = StatusInfo
		checks[0].Detail = "proxy_cache 未設定(キャッシュは『必要なら使う』もの)"
		checks[0].Recommendation = "不整合を許容できるレスポンスがあれば proxy_cache_path + proxy_cache を検討(Set-Cookie 付き応答は nginx が既定で非キャッシュ)"
		for i := 1; i < len(checks); i++ {
			checks[i].Status = StatusSkip
			checks[i].Detail = "proxy_cache が無効"
		}
		return checks
	}
	checks[0].Status = StatusOK
	checks[0].Detail = "proxy_cache を検出"

	if nginxProxyCacheLock.MatchString(conf) {
		checks[1].Status = StatusOK
	} else {
		checks[1].Status = StatusWarn
		checks[1].Detail = "proxy_cache_lock なし(キャッシュ失効の瞬間、同一キーへの再生成が多重実行される)"
		checks[1].Recommendation = "proxy_cache_lock on; で再生成を1リクエストに絞る(thundering herd 回避)"
	}

	if nginxIgnoreSetCookie.MatchString(conf) {
		checks[2].Status = StatusWarn
		checks[2].Detail = "proxy_ignore_headers が Set-Cookie を無視(セッション入り応答が共有キャッシュ経由で他クライアントに配られうる)"
		checks[2].Recommendation = "Set-Cookie を無視しない。キャッシュ対象は Set-Cookie を返さない location に分離"
	} else {
		checks[2].Status = StatusOK
		checks[2].Detail = "Set-Cookie 付き応答は既定で非キャッシュ"
	}
	return checks
}

func cacheHealthCheck(telemetry *CacheTelemetry, telemetryError string) Check {
	c := Check{ID: "cache-app-telemetry", Title: "アプリ側キャッシュ: ヒット率 / expire前 eviction"}
	if telemetryError != "" {
		c.Status = StatusSkip
		c.Detail = "invalid cache telemetry: " + telemetryError
		c.Recommendation = "metrics JSONのcounterを非負整数で揃える"
		return c
	}
	if telemetry == nil {
		c.Status = StatusSkip
		c.Detail = "アプリ側キャッシュのtelemetryなし(HTTP middlewareからは観測不可)"
		c.Recommendation = "memcached stats / redis INFO stats の hits・misses・evictions を同一ベンチ区間で入力"
		return c
	}
	total := telemetry.Hits + telemetry.Misses
	if total == 0 && telemetry.Evictions == 0 {
		c.Status = StatusSkip
		c.Detail = "キャッシュアクセスのない区間"
		c.Recommendation = "キャッシュを使う workload の区間で telemetry を入力"
		return c
	}
	rate := 0.0
	if total > 0 {
		rate = float64(telemetry.Hits) * 100 / float64(total)
	}
	c.Detail = fmt.Sprintf("hits=%d / misses=%d(ヒット率 %.1f%%) / evictions=%d",
		telemetry.Hits, telemetry.Misses, rate, telemetry.Evictions)
	switch {
	case telemetry.Evictions > 0:
		c.Status = StatusWarn
		c.Recommendation = "expire前のevictionは容量不足のシグナル。メモリ上限と、キーの増やしすぎ(1対多構造の肥大)を確認"
	case rate < 50:
		c.Status = StatusWarn
		c.Recommendation = "ヒット率が低い。TTL・キー設計・そもそもキャッシュすべき対象かを見直す"
	default:
		c.Status = StatusOK
	}
	return c
}

// WithCacheTelemetry replaces the application cache check at snapshot time so
// hit/miss/eviction counters align with the measured interval.
func WithCacheTelemetry(checks []Check, telemetry *CacheTelemetry, err error) []Check {
	result := make([]Check, 0, len(checks)+1)
	for _, check := range checks {
		if check.ID != "cache-app-telemetry" {
			result = append(result, check)
		}
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	result = append(result, cacheHealthCheck(telemetry, errText))
	return sortChecks(result)
}
