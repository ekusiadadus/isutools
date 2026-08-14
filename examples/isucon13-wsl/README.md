# ISUCON13 WSL2へ計測だけを導入する

`matsuu/wsl-isucon`のISUCON13 Go初期実装へ、isutoolsのSQL・HTTP・Echo route、
nginx access log、run境界CPU profile、管理画面を追加する実例です。インデックス、SQL、
キャッシュ、PowerDNS、アプリの業務処理は変更しません。

実機検証で固定したrevisionは次のとおりです。

```text
wsl-isucon: 876bd43a6f6f1048b8d341b2ab62d7afff01efd2
isutools:   4111f0c3935040f6e0546f44e55ed332ae51b7ce
WSL:        Ubuntu 22.04.3 LTS / ISUCON13 Go初期実装
```

2026-08-14の実機では固定SHAをGo module proxyから解決したbinaryで公式ベンチ`pass=true`、
score `12,079`、SQL 56種、HTTP 41種、
DB pool/access log/run CPU profileのhealth `ok`まで確認しました。スコアは環境・runごとに変動し、
性能保証ではありません。run ID、artifact hash、再起動・OFF・port forwardの確認結果とUI画像は
[実機検証記録](../../docs/isucon13-wsl-verification-20260814.md)に分離しています。

公式WSL版は本番と異なり、DNS port `1053`、`*.u.isucon.local`、自己署名証明書を使います。
ベンチコマンドは`./bench run --dns-port 1053 --enable-ssl`です。

## ファイル

- `main.go.patch`: SQL、DB pool、HTTP、Echo v4 route templateの計測だけをGo実装へ追加
- `nginx-isutools.conf`: isutoolsが解釈する専用LTSV access log
- `isupipe-go.isutools.conf`: loopback管理port、永続化・CPU profile、access-log URI丸め設定
- `isupipe-go.manual-pprof.conf`: manual profile確認時だけ使う一時override
- `isupipe-go.off.conf`: 環境変数だけで全計測を止める確認用override
- `remote-bench.sh`: WSL内の`reset -> 公式bench -> collect -> save -> stage`
- `Makefile`: 制御PCの`make bench`、SCP、確認、tunnel
- `isutools.mk.example`: host、distro、port、Windows userのローカル設定例

## 1. 前提を確認する

Windows側の管理者PowerShellで公式手順を完了し、WSL内で次を確認します。

```bash
systemctl is-active nginx mysql pdns isupipe-go
curl -fkSs --resolve pipe.u.isucon.local:443:127.0.0.1 \
  https://pipe.u.isucon.local/ >/dev/null
```

4 serviceが`active`、curlが0ならアプリの基準状態です。`kmod-static-nodes.service`や
`ssh.service`など、競技外unitの失敗と混同しないでください。

Go初期実装はGo 1.21ですが、現行isutoolsはGo 1.24以上を必要とします。Go 1.21以降の
toolchain自動選択を使うため、`GOTOOLCHAIN=auto`でdependencyを解決します。自動取得できない
閉域環境ではGo 1.24以上を先に配置してください。

## 2. 既存ファイルをbackupする

WSL内で、時刻付きdirectoryへ変更対象だけを保存します。

```bash
stamp=$(date '+%Y%m%d-%H%M%S')
backup=/home/isucon/isutools-backup-$stamp
mkdir -p "$backup"
cp -a /home/isucon/webapp/go/main.go /home/isucon/webapp/go/go.mod \
  /home/isucon/webapp/go/go.sum /home/isucon/webapp/go/isupipe "$backup/"
sudo cp -a /etc/systemd/system/isupipe-go.service "$backup/"
```

## 3. Go実装へpatchを適用する

このexample directoryを`/home/isucon/isutools-example`へコピーした前提です。

```bash
cd /home/isucon
patch -p1 < /home/isucon/isutools-example/main.go.patch
cd /home/isucon/webapp/go
GOTOOLCHAIN=auto go get \
  github.com/ekusiadadus/isutools@4111f0c3935040f6e0546f44e55ed332ae51b7ce \
  github.com/ekusiadadus/isutools/adapters/echov4@4111f0c3935040f6e0546f44e55ed332ae51b7ce
GOTOOLCHAIN=auto go mod tidy
gofmt -w main.go
GOTOOLCHAIN=auto go test ./...
GOTOOLCHAIN=auto go build -o isupipe .
```

`RegisterDBTarget`と`WatchDBPool`はSQLとconnection poolを安定ID`app`で結合します。
`SQLDriverName("mysql")`は既存DSNとdriverの意味を変えずにSQLを集計します。
`echov4.Install(e)`はEchoが登録した`/api/user/:username`等のtemplateを渡し、raw usernameを
HTTP集計キーにしません。`echo.WrapMiddleware(isutools.HTTP)`がduration/status/bytesを記録します。

## 4. nginxとsystemdを設定する

保存先はWSLのext4上に置きます。`/tmp`はWSL停止後に消えるため保存先にしません。

```bash
sudo install -d -o isucon -g isucon -m 0750 /home/isucon/isutools-data
if ! sudo test -e /home/isucon/isutools-data/nginx-access.log; then
  sudo install -o www-data -g isucon -m 0640 /dev/null \
    /home/isucon/isutools-data/nginx-access.log
fi
sudo install -o root -g root -m 0644 \
  /home/isucon/isutools-example/nginx-isutools.conf \
  /etc/nginx/conf.d/isutools.conf
sudo install -d -o root -g root -m 0755 \
  /etc/systemd/system/isupipe-go.service.d
sudo install -o root -g root -m 0644 \
  /home/isucon/isutools-example/isupipe-go.isutools.conf \
  /etc/systemd/system/isupipe-go.service.d/isutools.conf
sudo nginx -t
sudo systemctl daemon-reload
sudo systemctl restart nginx isupipe-go
```

この例は既存の`19191`との衝突を避けて`127.0.0.1:19196`を使います。外部bindや
`ISUTOOLS_ALLOW_UNAUTHENTICATED=1`は不要です。`ISUTOOLS_GIT_DIRTY=1`は、公式checkoutへ
計測patchを加えた事実をartifactに残すため意図的に設定しています。

`ISUTOOLS_ACCESS_LOG_PATH_RULES`は、nginx LTSVの`/api/user/<username>`と
`/api/livestream/<id>`をEchoと同じ定数templateへ丸めます。ruleは上からfull-matchで評価されるため、
livecomment reportなど長いpathを先に置いています。未一致pathは`keep`でそのまま残します。
設定が読み込まれたことは次で確認できます（値そのものは長いので、credentialと同様に診断logへ
出力しないでください）。

```bash
systemctl show isupipe-go -p Environment --value | grep -q 'ISUTOOLS_ACCESS_LOG_PATH_RULES='
```

## 5. readinessを確認する

```bash
systemctl is-active nginx mysql pdns isupipe-go
curl -fsS http://127.0.0.1:19196/json >/dev/null
curl -fkSs --resolve pipe.u.isucon.local:443:127.0.0.1 \
  https://pipe.u.isucon.local/ >/dev/null
mysqladmin ping -h 127.0.0.1 -uisucon -pisucon --silent
```

管理JSONだけの成功をベンチ成功とは扱いません。次の公式ベンチまで通して初めて実動確認です。

## 6. 制御PCからmake benchする

このdirectoryでローカル設定を作ります。`isutools.mk`はhost固有なのでcommitしません。

```bash
cp isutools.mk.example isutools.mk
$EDITOR isutools.mk
make check
make bench
```

`make bench`は次を順番に行います。

1. `POST /reset`し、`X-Isutools-Run-Id`を固定
2. 公式`./bench run --dns-port 1053 --enable-ssl`
3. `/tmp/result.json`をPython標準ライブラリで型検査
4. 公式結果をext4の`/home/isucon/isutools-data`へ即時copy
5. nginx logを`POST /collect`で取り込み、score/passを`POST /save`へ渡す
6. JSON/HTML/pprofをWindows共有directoryへstage
7. `scp`で`~/isutools-isucon13-results`へ取得し、最新snapshotを要約

ベンチ起動、結果JSON、collect、saveのどれかが失敗した場合は`POST /abort`し、誤ったscoreを
保存しません。`pass=false`は有効な公式結果なので、そのまま保存します。

結果は3層に分かれます。

- 公式原本: `/home/isucon/isutools-data/official-benchmark-*.json`
- isutools成果物: `/home/isucon/isutools-data/<timestamp>-<sequence>_gen*.json|html`
- 制御PCのcopy: `~/isutools-isucon13-results/`

JSONは再集計・機械検査用、HTMLは依存assetを含む自己完結report、`.pprof`と
`.meta.json`はrun境界CPU profileとprovenanceです。保存名の`gen3`は3世代目という意味ではなく、
同一process内のreset世代番号です。reset以外の世代更新もあるため、run一覧で2ずつ増える場合が
あります。比較の同一性には`gen`単独ではなく`run_id`、revision、score/pass、時刻を使います。

## 7. 管理画面とpprof

```bash
make tunnel
# 別terminal
make dashboard
```

ブラウザは`http://127.0.0.1:19196/`を開きます。adminはWSL内loopbackのままで、SSH転送だけを
使います。このMakefileはISUCON13 distroの現在IPを取得し、そのIPの`29196`だけへ
`systemd-socket-proxyd`を一時bindします。relayとSSH forwardは同じlifecycleなので、
`Ctrl-C`で両方終了します。管理port`19196`自体は外部公開しません。

このexampleのrun modeではidle時もmanual endpointはHTTP 409です。通常は`make bench`が保存した
`cpu_<capture-id>.pprof`を使います。manual endpoint自体を確認するときだけ、一時overrideで
run modeをoffにします。

```bash
sudo install -o root -g root -m 0644 \
  /home/isucon/isutools-example/isupipe-go.manual-pprof.conf \
  /etc/systemd/system/isupipe-go.service.d/zz-manual-pprof.conf
sudo systemctl daemon-reload
sudo systemctl restart isupipe-go

# 制御PCで実行
make pprof PPROF_SECONDS=30

# WSL内でrun modeへ戻す
sudo rm /etc/systemd/system/isupipe-go.service.d/zz-manual-pprof.conf
sudo systemctl daemon-reload
sudo systemctl restart isupipe-go
```

保存runにはopen/closeのCPU profileとmanifestが残ります。

## 8. OFFとrollback

計測を一時的に完全停止する場合は`isupipe-go.off.conf`をsystemd drop-inへ配置し、daemon
reloadとrestartを行います。`SQLDriverName`は元driver名、HTTP/Echo middlewareはno-opになり、
管理portも起動しません。

完全に戻す場合は、手順2のbackupから`main.go`、`go.mod`、`go.sum`、`isupipe`を戻し、追加した
2設定を削除して再起動します。結果directoryは証跡なので、不要と確認するまで削除しません。

```bash
sudo rm /etc/nginx/conf.d/isutools.conf
sudo rm /etc/systemd/system/isupipe-go.service.d/isutools.conf
sudo systemctl daemon-reload
sudo nginx -t
sudo systemctl restart nginx isupipe-go
```
