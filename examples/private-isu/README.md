# private-isuへ手動で最小導入する

公式のprivate-isuへ、isutoolsのSQL計測、HTTP計測、管理画面だけを追加します。
インデックスやキャッシュなどの高速化は行いません。まず「計測できる状態」だけを作る手順です。

この手順ではインストールスクリプトやベンチラッパーを使いません。変更内容と実行するコマンドを
1つずつ確認しながら進めます。

検証対象は次の固定revisionです。

```text
private-isu: 0dc3be8b5b32d8519e0e841721da3ddf2c6a1542
isutools:    v1.4.0
```

## 1. 必要なものを確認する

Docker、Go 1.24以上、`make`、`curl`を使います。

```bash
docker version
go version
make --version
curl --version
```

理由: 先に不足しているコマンドを見つけておくと、アプリの変更が原因なのか、実行環境が原因なのかを
切り分けやすくなります。

## 2. 公式実装を固定revisionで取得する

```bash
git clone https://github.com/catatsuy/private-isu.git
cd private-isu
git checkout 0dc3be8b5b32d8519e0e841721da3ddf2c6a1542
make init
```

理由: 上流のコードが更新されても、ここに書かれた行と手順がずれない状態から始めるためです。
`make init`はDBの初期データとベンチ用画像を配置します。

## 3. Go実装へisutoolsをimportする

`webapp/golang/app.go`のimportへ1行追加します。

```go
import (
	// 既存のimportはそのまま

	"github.com/ekusiadadus/isutools"
)
```

実際には、既存の外部package群へ次のように置きます。

```go
"github.com/bradfitz/gomemcache/memcache"
gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
"github.com/ekusiadadus/isutools"
"github.com/go-chi/chi/v5"
```

理由: 次の2ステップでSQLドライバーとHTTPハンドラーをisutoolsへ渡すためです。

## 4. SQLを計測対象にする

同じ`webapp/golang/app.go`で、次の行を探します。

```go
db, err = sqlx.Open("mysql", dsn)
```

次のように変更します。

```go
db, err = sqlx.Open(isutools.SQLDriverName("mysql"), dsn)
```

理由: 実際のMySQLドライバーの前に計測用ドライバーを挟み、SQLの回数と時間を収集するためです。
接続先やSQLの意味は変わりません。

## 5. HTTPを計測対象にする

同じファイル末尾の次の行を探します。

```go
log.Fatal(http.ListenAndServe(":8080", r))
```

次のように変更します。

```go
log.Fatal(http.ListenAndServe(":8080", isutools.HTTP(r)))
```

理由: 既存routerの外側へ計測middlewareを置き、HTTP pathごとの回数と時間を収集するためです。
アプリが待ち受けるportは`8080`のままです。isutoolsの管理画面は別の`19191`で起動します。

## 6. dependencyを追加する

```bash
cd webapp/golang
go get github.com/ekusiadadus/isutools@v1.4.0
go mod tidy
gofmt -w app.go
go test ./...
cd ../..
```

理由: versionを固定すると、後日同じ手順を実行しても別versionへ勝手に変わりません。
`gofmt`で手編集したimportを整え、起動前に`go test`を通して編集の誤りも検出します。

## 7. Composeとnginxの設定を置く

このisutools repositoryの場所を指定し、2つの設定ファイルをコピーします。

```bash
export ISUTOOLS_DIR=/absolute/path/to/isutools

cp "$ISUTOOLS_DIR/examples/private-isu/compose.isutools.yml" webapp/
cp "$ISUTOOLS_DIR/examples/private-isu/nginx-isutools.conf" \
  webapp/etc/nginx/conf.d/isutools.conf
```

それぞれの理由は次のとおりです。

- `compose.isutools.yml`: Go実装を選び、管理port`19191`をloopbackだけへ公開します。
- `nginx-isutools.conf`: nginxのrequest timeとupstream response timeを専用ログへ出します。
- 専用volume: nginxが書いたログをisutoolsのapp containerからread-onlyで読みます。
- data volume: Docker DesktopのLinux filesystemへ計測結果を安全に保存します。

管理portをcontainer内では`:19191`で待ち受けるため、
`ISUTOOLS_ALLOW_UNAUTHENTICATED=1`を明示しています。ホスト側は
`127.0.0.1:19191`だけへbindするため、LANやInternetには公開しません。

## 8. アプリを起動する

最初にベンチマーカーのimageを作ります。

```bash
docker build -t private-isu-benchmarker ./benchmarker
```

次に、公式Composeへisutools用の上書き設定を重ねて起動します。

```bash
cd webapp
docker compose -f compose.yml -f compose.isutools.yml up -d --build
docker compose -f compose.yml -f compose.isutools.yml ps
cd ..
```

理由: 公式`compose.yml`を直接書き換えず、Go実装の選択と計測設定だけを別ファイルに分けるためです。

初回だけは1.2 GB前後のDB dumpを投入するため、containerが`Up`になってからMySQLが使えるまで
時間がかかります。次のコマンドを実行し、`mysqld is alive`になるまで待ってください。

```bash
cd webapp
docker compose -f compose.yml -f compose.isutools.yml exec mysql \
  mysqladmin ping -h 127.0.0.1 -uroot -proot
cd ..
```

まだ接続できない場合は数十秒待って同じコマンドを再実行します。

理由: Composeの`Up`はprocessの起動を示すだけで、1.2 GBの初期データ投入完了までは保証しません。
DB準備前にベンチを始めると、isutoolsの導入とは無関係な接続エラーになります。

## 9. アプリと管理画面を確認する

```bash
curl -fsS http://127.0.0.1/ >/dev/null
curl -fsS http://127.0.0.1:19191/json >/dev/null
```

両方成功したら、ブラウザで<http://127.0.0.1:19191/>を開きます。

理由: `80`の成功はprivate-isu本体、`19191`の成功はisutools管理画面の起動をそれぞれ確認します。

## 10. 手動で1回ベンチを実行して保存する

まず計測区間を開始します。

```bash
curl -fsS -X POST http://127.0.0.1:19191/reset
```

次に公式ベンチを実行します。

```bash
docker run --rm \
  --network private-isu_my_network \
  private-isu-benchmarker \
  /bin/benchmarker -t http://nginx -u /opt/userdata
```

最後のJSONを目で確認します。例えば次の結果なら、scoreは`1710`、passは`true`です。

```json
{"pass":true,"score":1710,"success":1434,"fail":0,"messages":[]}
```

nginxのbufferを取り込み、確認した値を手動で保存します。

```bash
curl -fsS -X POST http://127.0.0.1:19191/collect
curl -fsS -X POST \
  'http://127.0.0.1:19191/save?score=1710&pass=true'
```

理由: `reset -> benchmark -> collect -> save`の順にすると、ベンチ区間だけのSQL、HTTP、
nginx、host情報とscoreを同じrunへ保存できます。数値を人が確認して入力するため、ログ形式に依存する
自動解析は行いません。

ベンチが途中で起動できなかった場合は、誤ったrunを保存せず終了させます。

```bash
curl -fsS -X POST http://127.0.0.1:19191/abort
```

## 11. 別PCから管理画面を見る

isutoolsを動かしているhostへSSH転送します。

```bash
ssh -o ServerAliveInterval=30 -o ServerAliveCountMax=3 \
  -L 19191:127.0.0.1:19191 user@example-host
```

SSH接続を維持したまま、手元のブラウザで<http://127.0.0.1:19191/>を開きます。

理由: 管理画面を外部公開せず、暗号化されたSSH接続だけで閲覧するためです。

## 12. 終了する

```bash
cd webapp
docker compose -f compose.yml -f compose.isutools.yml down
```

この最小構成の計測結果はDocker named volume `private-isu_isutools_data`に残ります。
`docker compose down`だけでは削除されません。長期保存先やbackup方法を指定する場合は、
[導入ガイド](../../docs/INTEGRATION.md)の永続data directory設定へ進んでください。
