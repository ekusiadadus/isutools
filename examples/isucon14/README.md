# ISUCON14へ手動で最小導入する

公式ISUCON14のGo初期実装へ、isutoolsのSQL計測、HTTP計測、管理画面だけを追加します。
インデックス、キャッシュ、通知、マッチングの変更は行いません。

この手順ではインストールスクリプトやベンチラッパーを使いません。変更内容と実行するコマンドを
1つずつ確認しながら進めます。

検証対象は次の固定revisionです。

```text
ISUCON14: 53f8b627e040c30ebec600457c6c97da008b84b0
isutools: v1.4.0
```

## 1. 必要なものを確認する

Docker、Go 1.24以上、Task、`curl`を使います。

```bash
docker version
go version
task --version
curl --version
```

理由: ISUCON14の初期`go.mod`はGo 1.23ですが、isutools v1.4.0はGo 1.24以上を必要とします。
先にGo versionを確認すると、dependency追加時の失敗をコードの問題と取り違えずに済みます。

## 2. 公式実装を固定revisionで取得する

```bash
git clone https://github.com/isucon/isucon14.git
cd isucon14
git checkout 53f8b627e040c30ebec600457c6c97da008b84b0
```

理由: 上流のコードが更新されても、ここに書かれた行と手順がずれない状態から始めるためです。

## 3. Go実装へisutoolsをimportする

`webapp/go/main.go`のimportへ1行追加します。

```go
"github.com/ekusiadadus/isutools"
"github.com/go-chi/chi/v5"
"github.com/go-chi/chi/v5/middleware"
```

理由: 次の2ステップでSQLドライバーとHTTPハンドラーをisutoolsへ渡すためです。

## 4. SQLを計測対象にする

同じ`webapp/go/main.go`で次の行を探します。

```go
_db, err := sqlx.Connect("mysql", dbConfig.FormatDSN())
```

次のように変更します。

```go
_db, err := sqlx.Connect(isutools.SQLDriverName("mysql"), dbConfig.FormatDSN())
```

理由: 実際のMySQLドライバーの前に計測用ドライバーを挟み、SQLの回数と時間を収集するためです。
接続先やSQLの意味は変わりません。

## 5. HTTPを計測対象にする

同じファイルの`main`関数にある次の行を探します。

```go
http.ListenAndServe(":8080", mux)
```

次のように変更します。

```go
http.ListenAndServe(":8080", isutools.HTTP(mux))
```

理由: 既存routerの外側へ計測middlewareを置き、HTTP pathごとの回数と時間を収集するためです。
ISURIDE本体は`8080`のまま、isutools管理画面はloopbackの`19191`で起動します。

## 6. dependencyを追加する

```bash
cd webapp/go
go get github.com/ekusiadadus/isutools@v1.4.0
go mod tidy
gofmt -w main.go
go test -vet=off ./...
go build ./...
cd ../..
```

理由: versionを固定すると、後日同じ手順を実行しても別versionへ勝手に変わりません。
`gofmt`で手編集したimportを整え、起動前にtestとbuildを通して編集の誤りも検出します。
初期実装には現行Goの`go vet`が検出する既存の`log/slog`呼び出しがあるため、ここでは
`-vet=off`でtest本体を確認し、続く`go build`でコンパイルを別に確認します。

## 7. DBと決済mockを起動する

```bash
task up
```

理由: 公式のlocal構成ではMySQL、決済mock、matcherをDockerで動かし、Goアプリだけをhostで動かします。

## 8. Goアプリとisutoolsを起動する

計測結果用directoryを作ります。

```bash
mkdir -p .isutools-data
```

Goアプリを起動します。このコマンドはベンチ終了まで動かしたままにします。

```bash
ISUTOOLS_DATA_DIR="$(pwd)/.isutools-data" \
ISUTOOLS_ROLE=app \
task go:run
```

理由: `ISUTOOLS_DATA_DIR`を絶対パスにすると、Taskがworking directoryを`webapp/go`へ変えても
保存先がずれません。管理portを指定しないため、安全な既定値`127.0.0.1:19191`が使われます。

## 9. アプリと管理画面を確認する

別ターミナルを開き、ISUCON14 repository rootで実行します。

```bash
curl -fsS http://127.0.0.1:8080/api/initialize -X POST >/dev/null
curl -fsS http://127.0.0.1:19191/json >/dev/null
```

両方成功したら、ブラウザで<http://127.0.0.1:19191/>を開きます。

理由: `8080`の成功はISURIDE本体、`19191`の成功はisutools管理画面の起動をそれぞれ確認します。

## 10. 手動で1回ベンチを実行して保存する

最初に計測区間を開始します。

```bash
curl -fsS -X POST http://127.0.0.1:19191/reset
```

次に公式ベンチを実行します。

```bash
cd bench
task run-local
cd ..
```

最後の結果ログから`pass`と`スコア`を目で確認します。例えば結果が`pass=true`、
`スコア=9511`なら、次のように保存します。

```bash
curl -fsS -X POST \
  'http://127.0.0.1:19191/save?score=9511&pass=true'
```

理由: `reset -> benchmark -> save`の順にすると、ベンチ区間だけのSQL、HTTP、host情報とscoreを
同じrunへ保存できます。数値を人が確認して入力するため、ログ形式に依存する自動解析は行いません。

ベンチが起動しなかった場合は、誤ったrunを保存せず終了させます。

```bash
curl -fsS -X POST http://127.0.0.1:19191/abort
```

## 11. ISUCON14実機のsystemd構成へ入れる

実機で既にチューニングしている場合も、`/home/isucon/webapp/go/main.go`へステップ3から5の
3か所だけを手で反映します。その後、dependency追加とbuildを行います。

```bash
cd /home/isucon/webapp/go
go get github.com/ekusiadadus/isutools@v1.4.0
go mod tidy
gofmt -w main.go
go test -vet=off ./...
go build -o isuride .
```

計測結果用directoryをservice userで書けるように作ります。

```bash
sudo install -d -o isucon -g isucon -m 0750 /home/isucon/isutools-data
```

systemd serviceが読む`/home/isucon/env.sh`へ次の2行を追加します。

```bash
ISUTOOLS_DATA_DIR=/home/isucon/isutools-data
ISUTOOLS_ROLE=app
```

再起動して確認します。unit名は実機の設定に合わせてください。

```bash
sudo systemctl restart isuride-go.service
sudo systemctl status isuride-go.service --no-pager
curl -fsS http://127.0.0.1:19191/json >/dev/null
```

理由: sourceの変更だけでは稼働中binaryへ反映されません。build、service再起動、管理port確認までを
1セットで行う必要があります。

## 12. 別PCから管理画面を見る

isutoolsを動かしているhostへSSH転送します。

```bash
ssh -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
  -L 19191:127.0.0.1:19191 isucon@example-host
```

SSH接続を維持したまま、手元のブラウザで<http://127.0.0.1:19191/>を開きます。

理由: 管理画面を外部公開せず、暗号化されたSSH接続だけで閲覧するためです。

Windows SSH hostからWSL2内のisutoolsへ接続する構成では、SSH daemonとGoアプリが別network
namespaceにいる場合があります。その場合は単純な`-L`だけで届かないため、
[導入ガイドのguest relay手順](../../docs/INTEGRATION.md#8-管理ポートと権限)を使ってください。

## 13. 終了する

Goアプリを`Ctrl-C`で停止してから、Docker側を終了します。

```bash
task down
```

`.isutools-data`には計測結果が残ります。不要であることを確認できるまでは削除しないでください。
