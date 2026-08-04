package advisor

import (
	"fmt"
	"regexp"
	"strings"
)

// Only nginx exposes ECH as inspectable configuration (ssl_ech_file); Caddy
// manages ECH keys and DNS records itself, and the DNS HTTPS record (ech=)
// cannot be verified from inside this process. ECH never blocks serving, so
// every finding here is advisory (StatusInfo), not a defect.

var nginxECHFile = regexp.MustCompile(`(?m)(?:^|[;{}])\s*ssl_ech_file\s+[^;]+;`)

func checkECH(opts Options) []Check {
	mk := func(id, title string) Check { return Check{ID: id, Title: title} }
	checks := []Check{
		mk("ech-config", "ECH: ssl_ech_file(Encrypted ClientHello)"),
		mk("ech-key-rotation", "ECH: 鍵ローテーション(新旧鍵の並行受け入れ)"),
		mk("ech-logging", "ECH: $ssl_ech_status のログ確認"),
	}
	conf := stripComments(string(opts.Protocol.ProxyConfig))
	if strings.TrimSpace(conf) == "" {
		for i := range checks {
			checks[i].Status = StatusSkip
			checks[i].Detail = "proxy 設定を特定できない(ISUTOOLS_PROXY_KIND/ISUTOOLS_PROXY_CONF を設定)"
		}
		return checks
	}
	kind := strings.ToLower(strings.TrimSpace(opts.Protocol.ProxyKind))
	if kind == "" {
		kind = detectProxyKind(conf)
	}
	if kind != "nginx" {
		for i := range checks {
			checks[i].Status = StatusSkip
			checks[i].Detail = "ECH 検査は nginx 設定のみ対応(Caddy は鍵と DNS を自動管理)"
		}
		return checks
	}

	files := len(nginxECHFile.FindAllString(conf, -1))
	if files == 0 {
		checks[0].Status = StatusInfo
		checks[0].Detail = "ssl_ech_file なし(SNI が平文で送られる)"
		checks[0].Recommendation = "nginx 1.29.4+ と ECH 対応 OpenSSL で ssl_ech_file を設定し、DNS HTTPS レコードに ech= を登録"
		for i := 1; i < len(checks); i++ {
			checks[i].Status = StatusSkip
			checks[i].Detail = "ECH 未設定"
		}
		return checks
	}
	checks[0].Status = StatusOK
	checks[0].Detail = fmt.Sprintf("ssl_ech_file を %d 件検出", files)

	if files == 1 {
		checks[1].Status = StatusInfo
		checks[1].Detail = "ssl_ech_file が1つ(旧鍵の受け入れ期間なし)"
		checks[1].Recommendation = "鍵切替時は新鍵を先頭、旧鍵を2番目以降に指定(先頭のみ retry_configs 対象、以降は復号専用。古い DNS キャッシュのクライアントを救済)"
	} else {
		checks[1].Status = StatusOK
		checks[1].Detail = fmt.Sprintf("鍵ファイル %d 件(先頭= retry_configs 対象、以降=復号専用の旧鍵)", files)
	}

	if strings.Contains(conf, "$ssl_ech_status") {
		checks[2].Status = StatusOK
		checks[2].Detail = "log_format に $ssl_ech_status を検出"
	} else {
		checks[2].Status = StatusInfo
		checks[2].Detail = "access log に ECH の成否が出ない"
		checks[2].Recommendation = "log_format に $ssl_ech_status(必要なら $ssl_ech_outer_server_name も)を追加し、実接続で ech:SUCCESS を確認"
	}
	return checks
}
