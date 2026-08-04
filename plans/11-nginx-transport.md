# 11: nginx transport 検査(UNIX domain socket / listen backlog)+ MTU 表示 — v6

種別: 機能 / 対象リリース: v1.2.x / 依存: なし(静的検査。v1.1.0 のキャッシュ検査と同パターン)

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[MEDIUM] 「有効な listen endpoint」の判定入力を root nginx.conf
   *ファイル* に限定する**。v5 の「`ISUTOOLS_NGINX_CONF`(ディレクトリ)
   から読んだ内容のまま、include されていない inactive file を除外できる」
   という前提は**撤回する**。ディレクトリを列挙しても
   「その `.conf` が実際に include されたか」は原理的に判定できず、
   `conf.d/foo.conf.bak` や無効化した旧設定を有効設定として数える恐れがある。
   → **root conf ファイルを起点に `include`(glob 含む)を解決する
   `loadNginxConfTree(fsys, root, prefix)` を新設**し、
   `advisor.Options` に `NginxRoot *NginxRootConf` を追加する。
   root conf が無い(ディレクトリ指定のみ / 未設定)場合、
   `nginx-listen-backlog` は判定せず **`StatusSkip` + 明示 detail**
   にする(§設定入力・§nginx-listen-backlog)。
   既存の `Options.NginxConf []byte`(ディレクトリ連結)は
   **そのまま残し**、gzip / keepalive / worker_connections / sendfile /
   expires / proxy-cache / ECH / UDS の各 check は従来どおり動く
   (後方互換。§後方互換マトリクス)
2. **[MEDIUM] UDS の推奨パスを `/tmp/webapp.sock` から
   `/run/<app>/app.sock` へ変更する**。v4/v5 の `/tmp/webapp.sock` 推奨は
   **撤回する**。`/tmp` は world-writable(sticky)で、
   **app 起動前に第三者が同名パスを作れる**(bind が EADDRINUSE で失敗する
   pre-creation DoS、および stale 除去ロジックが他人のファイルを消す事故)
   ため、権限設計の出発点として不適切。加えて RHEL 系の `nginx.service` は
   既定で `PrivateTmp=true` であり、nginx と app の `/tmp` が別 mount
   namespace になって「同じパスなのに繋がらない」構成を生む。
   → 専用ディレクトリ `/run/<app>/`(owner `app:nginx-group`・mode `0750`)
   と socket `app.sock`(owner `app:nginx-group`・mode `0660`)、および
   **stale socket の安全な除去手順**を推奨文言と例に明記する
   (§UDS の推奨レイアウト)
3. **[MEDIUM の再点検] listen endpoint 解析節を 1 と整合させた**。
   前提を「somaxconn が読める」＋「root conf tree が確定している」の
   2 つに増やし、未解決 include(変数・prefix 外)や打ち切りがある場合は
   **検出漏れによる誤検知を避けて skip** する規則を追加した。
   endpoint の正規化規則(`0.0.0.0:80` / `[::]:80` / `unix:`)、
   port 省略形・`stream`/`mail` ブロックの除外、
   同一 endpoint への重複 `backlog=` の扱いを明文化(§nginx-listen-backlog)
4. **[MINOR] ヘッダ版数の陳腐化**。版数なしだったので v6 に更新
   (本書は第5回レビュー差し戻し対応版)
5. **[v6 監査反映] 稼働中 private-isu の実測を根拠に include 解決を修正**。
   v6 初稿の「絶対 include は先頭 `/` を落として FS 相対に正規化」という
   規則は**単独では不十分なので撤回**した(conf 内の絶対パス
   `/etc/nginx/conf.d/` と isutools のマウントパス `/nginx-conf/` が
   別名前空間で、コンテナ構成では必ず失敗する)。
   → §実測根拠 を新設し、§include パスの remap 規則(prefix 基準への
   段階的読み替え・最初の実在候補のみ採用・解決不能なら推測せず
   `Unresolved` + `nginx-listen-backlog` skip + health
   `nginx-conf-include-unresolved`)を必須要件として追加。
   あわせて実在する inactive file(`conf.d/php.conf.org`)を root ファイル
   起点要件の具体的根拠として明記し、新規 check 2 件が実配置で正しく
   振る舞う(`nginx-upstream-uds` は skip / `nginx-listen-backlog` は info)
   ことを実測として記録した。テスト fixture(remap 必要 / 解決不能 / glob)を
   §実装ステップ・§テスト計画 に追加。**見積もり 1.5 日は据え置き**
   (README の表と一致。再算定は不要)

## 背景

ISUCON 本 9-8「Linux カーネルパラメータ」・9-9「MTU」より。
isutools v1.1.0 は `os-somaxconn`(1024 未満 warn / 4096 推奨)と
`os-port-range`(幅 2 万未満 warn)を実装済みだが、次の 3 点が未対応:

1. **UNIX domain socket**(9-8): nginx→同一ホスト app の TCP 接続
   (`proxy_pass http://localhost:8080`)は UDS
   (`server unix:/run/isuconapp/app.sock;`)へ置き換えると、ephemeral port
   を消費せず TCP オーバーヘッドも避けられる(書籍は「利用できるパターン
   であれば利用することを推奨」)
2. **nginx `listen ... backlog=`**(9-8 の補完): `somaxconn` を上げても
   **nginx の listen backlog は既定 511** のままで accept キューは
   511 に制限される。カーネル側だけ上げて満足する片手落ちを検出する
3. **MTU**(9-9): Jumbo Frame(9000)の適用状態が見えない

## ゴール

- nginx conf の静的検査 2 件(UDS 機会・listen backlog)を advisor に追加
- root nginx.conf から `include` を解決し、**実際に有効な設定ファイル集合**
  を確定する loader を advisor 内に持つ(listen backlog 判定の前提)
- NIC 別 MTU を表示に追加(判定はしない)

## 非ゴール

- MTU の良し悪し判定(Jumbo Frame は経路全体の対応が前提で、
  環境依存が強い。表示のみ → 計画 05 の方針と同じ)
- アプリ側 listen の UDS 化そのものの検査(アプリ実装は観測できない)
- **ディレクトリ入力だけでの「有効な listen endpoint」判定**(v6)。
  include 関係が確定できないため実施しない。skip して理由を出す
- **`nginx -T` の実行**(v6)。isutools はアプリプロセス内のライブラリで
  あり外部プロセスを起動しない方針。実行中プロセスと同じ `-p` / `-c` を
  再現できず、root 権限も要る。root conf の明示指定
  (`ISUTOOLS_NGINX_ROOT_CONF`)を正規経路とする

## 設計

### 設定入力(v6 で明確化)

advisor が受け取る nginx 設定入力を **2 系統**に分ける。

| 入力 | 型 | 何を保証するか | 使う check |
|---|---|---|---|
| `Options.NginxConf`(既存) | `[]byte` | conf 断片の**連結**。ファイル境界・include 順・有効/無効は保証しない | gzip / keepalive / worker_connections / sendfile / expires / proxy-cache 系 / ECH 系 / `nginx-upstream-uds` |
| `Options.NginxRoot`(新規) | `*NginxRootConf` | root からの include 解決済みで、**実際に有効なファイル集合と順序**を保証 | `nginx-listen-backlog` |

```go
// NginxRootConf は root nginx.conf **ファイル**の位置を指す。
// ディレクトリ指定しか無い場合は Options.NginxRoot を nil のままにする
// (どの .conf が include されたか確定できないため)。
type NginxRootConf struct {
    // Path は FS からの相対パス(先頭 "/" なし)。例: "etc/nginx/nginx.conf"
    Path string
    // FS は include/glob を解決する FS。nil なら Options.FS を使う
    FS fs.FS
    // Prefix は相対 include の解決基点(nginx の -p 相当)。
    // 空なら path.Dir(Path)。FS からの相対パス
    Prefix string
}

type Options struct {
    // ...既存フィールド...
    NginxConf []byte          // 既存: 連結された conf(後方互換のため維持)
    NginxRoot *NginxRootConf  // 新規: root conf ファイル。nil なら listen 判定を skip
}
```

`isutools.go` 側の解決順(既存 `collectAdviceWithEnv` を拡張):

1. `ISUTOOLS_NGINX_ROOT_CONF` が設定されていればそれを root とする(最優先)
2. `ISUTOOLS_PROXY_CONF` が **ファイル**を指し、kind が `nginx` ならそれを root
3. `ISUTOOLS_NGINX_CONF`(legacy)が **ファイル**を指すならそれを root
4. どれも**ディレクトリ**または未設定 → `NginxRoot = nil`
5. `ISUTOOLS_NGINX_PREFIX`(任意)で `Prefix` を上書きできる。既定は root の親

`readProxyConf`(isutools.go:449)によるディレクトリ連結は**現状のまま**
`Options.NginxConf` を埋め続ける。すなわち root conf を追加しても
既存の nginx check は挙動が変わらない。

#### 後方互換マトリクス

| `ISUTOOLS_*_CONF` の指す先 | `NginxConf` | `NginxRoot` | 既存 nginx check | `nginx-listen-backlog` |
|---|---|---|---|---|
| ファイル(nginx.conf) | そのファイル内容 | 設定される | 従来どおり | **判定する** |
| ディレクトリ(conf.d 等) | `*.conf` 連結(従来) | nil | 従来どおり | **skip**(理由を明示) |
| ディレクトリ + `ISUTOOLS_NGINX_ROOT_CONF` | `*.conf` 連結(従来) | root ファイル | 従来どおり | **判定する** |
| 未設定 | 空 | nil | 従来どおり skip | skip |

既存 check を tree ベースへ移行しないのは意図的(v6)。それらは
「ある設定が書かれているか」の存在検査であり、endpoint の**有効性**を
断定しないため、入力を厳格化しても得が無く後方互換だけを壊す。
移行するとしても本計画の範囲外とする。

### 実測根拠(v6 監査反映): 稼働中 private-isu の nginx 設定レイアウト

本節は机上の想定ではなく、**稼働中の private-isu デプロイで実際に確認した値**
である。以降の設計要件(include の remap、root ファイル起点)はこの実測に基づく。

| 事実 | 実測値 |
|---|---|
| `ISUTOOLS_NGINX_CONF` の指す先 | `/nginx-conf` — **ディレクトリ** |
| その中身 | `nginx.conf`(root)/ `conf.d/default.conf` / `conf.d/php.conf.org` |
| root `nginx.conf` の include 記述 | `include /etc/nginx/conf.d/*.conf;` — **nginx コンテナ名前空間の絶対パス** |
| isutools(app コンテナ)から見えるパス | `/nginx-conf/...` — **別のマウントパス** |
| upstream | `upstream app_backend { server app:8080; }` — コンテナのサービス名(loopback ではない) |
| listen | `listen 80;`(`backlog=` なし)。同ホストの somaxconn は 4096 |

この実測から確定する 3 点:

1. **include パスは素朴には解決できない**(→ §include パスの remap 規則)。
   conf に書かれた絶対パス(`/etc/nginx/conf.d/`)と isutools が読む
   マウントパス(`/nginx-conf/`)が**別名前空間**なので、
   「先頭 `/` を落として FS 相対に読み替える」だけの実装は
   この(コンテナでは非常に一般的な)配置で**必ず失敗する**
2. **inactive file は実在する**(→ root ファイル起点要件の具体的根拠)。
   `conf.d/php.conf.org` は ISUCON 定番の「リネームして無効化」パターン。
   現行実装 `readProxyConf`(isutools.go:449。`WalkDir` + `.conf` 接尾辞の
   ものを全部連結)がこれを取り込まないのは、**接尾辞がたまたま `.conf`
   でないという偶然による**。include されていないが `.conf` 接尾辞のまま
   無効化されたファイル(例: どの include にも掛からない `conf.d/old.conf`)
   は**誤って有効設定として数えてしまう**。ディレクトリ列挙では原理的に
   防げないため、`nginx-listen-backlog` は root ファイル起点を必須とする
3. **新規 check 2 件はこの実配置で意図どおりに振る舞う**(実測による検証):

| check | 実配置での入力 | 期待挙動 | 判定 |
|---|---|---|---|
| `nginx-upstream-uds` | `server app:8080;`(サービス名・非 loopback) | **skip**(「対象外: upstream が同一ホストではない」) | **偽陽性なし** — §`nginx-upstream-uds` の loopback 限定規則が実配置で正しく効く |
| `nginx-listen-backlog` | `listen 80;`(`backlog=` なし)+ somaxconn=4096 | **info**(既定 511 が somaxconn を活かせていない) | §判定「`backlog=` なし かつ somaxconn ≥ 4096 → info」が実配置で正しく発火 |

ただし後者は `NginxRoot` が確定している場合の挙動である。この実測環境は
`ISUTOOLS_NGINX_CONF` が**ディレクトリ**なので、そのままでは
§後方互換マトリクスどおり `NginxRoot == nil` → **skip** になる。
info を得るには `ISUTOOLS_NGINX_ROOT_CONF=/nginx-conf/nginx.conf` の
明示が必要で、これは設計どおりの挙動(断定できない入力では判定しない)。

### `loadNginxConfTree`(新規・advisor 内部)

```go
type nginxConfFile struct {
    Path string // FS 相対パス
    Body []byte // 生バイト列(stripComments 前)
}

type nginxConfTree struct {
    Root       string          // root conf の FS 相対パス
    Files      []nginxConfFile // include 順(root が先頭、深さ優先)
    Unresolved []string        // 展開できなかった include の raw 引数
    Truncated  bool            // 上限に到達して打ち切った
}

// loadNginxConfTree は root から include(glob 含む)を再帰展開し、
// **実際に有効な**設定ファイル集合を include 順で返す。
func loadNginxConfTree(fsys fs.FS, root, prefix string) (*nginxConfTree, error)
```

展開規則(いずれも保守的・fail-open):

- `include` 引数に `$` を含む(変数展開)→ 展開せず `Unresolved` へ
- 相対パスは `prefix` 基準で解決する
- **絶対パスを「先頭 `/` を落として FS 相対に正規化」するだけの規則は
  撤回する**(v6 監査反映)。実測(§実測根拠)のとおり、conf 内の絶対パスは
  nginx 側名前空間のものであり isutools のマウントパスと一致しないため、
  この規則単独では解決できない。→ §include パスの remap 規則に従う
- 解決候補は `path.Clean` 後に **`prefix` 配下でなければ `Unresolved`**
  (任意 path を辿らせない。この不変条件は remap 後の候補にも同じく適用する)
- glob は `fs.Glob` で展開し、**辞書順**に並べる(nginx の `glob()` と同じ)。
  マッチ 0 件の glob は nginx 同様エラーにせず無視する
- 循環検出は **展開中の include スタック**で行う(スタック上に既にある
  パスを再び include → 循環とみなし当該 include を `Unresolved` へ、
  `Truncated = true`)。**別ブランチからの同一ファイル重複 include は
  正当**(共通 snippet を複数 server が include する構成)なので許可し、
  `maxIncludeFiles` の消費としてのみ数える
- 上限: `maxIncludeDepth = 16` / `maxIncludeFiles = 256` /
  `maxTreeBytes = 4 << 20`。超過は `Truncated = true`
- root が開けない → `error`(呼び出し側は skip 扱い)

#### include パスの remap 規則(v6 監査反映・必須要件)

include 先の**ディレクトリ成分を、指定された root conf ファイルの
ディレクトリ(`prefix`)基準へ読み替える**。候補は決定的な順序で試し、
**最初に実在した候補だけを採用**する(曖昧な多重採用をしない)。

`raw` を include の引数、`p = path.Clean(strings.TrimPrefix(raw, "/"))` として:

1. **候補 0(従来どおり)**: `p` をそのまま FS 相対として扱う。
   FS が実ホストの `/` を指す通常構成ではこれが正しい
2. **候補 1..n(remap)**: `p` の**先頭ディレクトリ成分を 1 つずつ落とした
   tail** を `path.Join(prefix, tail)` に読み替える。段数上限
   `maxRemapDepth = 8`
3. 各候補は採用前に `prefix` 配下チェックを通す(`..` で脱出する候補は破棄)
4. 先頭から順に FS 上の実在を確認する。glob はマッチ 1 件以上で実在とみなす。
   **最初に実在した候補で確定し、以降の段は試さない**
5. どの候補も実在しない → **推測せず** `Unresolved` に `raw` を積む

実測レイアウト(§実測根拠)での解決例
— FS = ホスト `/`、root = `nginx-conf/nginx.conf`、`prefix` = `nginx-conf`、
`raw` = `/etc/nginx/conf.d/*.conf`:

| 段 | 候補 | 結果 |
|---|---|---|
| 0 | `etc/nginx/conf.d/*.conf` | `prefix` 配下でない → 破棄 |
| 1 | `nginx-conf/etc/nginx/conf.d/*.conf` | 実在しない |
| 2 | `nginx-conf/nginx/conf.d/*.conf` | 実在しない |
| 3 | `nginx-conf/conf.d/*.conf` | **マッチ**(`conf.d/default.conf`)→ 採用・確定 |

`conf.d/php.conf.org` は glob `*.conf` に一致しないため入らない。
`.conf` 接尾辞のまま無効化されたファイルも、この include に掛からなければ
入らない(ディレクトリ連結との決定的な差)。

**解決できなかった場合の扱い(推測は禁止)**:

- 当該 include を `Unresolved` に積む
- **その include が覆う範囲の「有効な listen endpoint」判定を行わない**。
  `nginx-listen-backlog` は §前提を欠く場合の出力の表に従い `StatusSkip`
- health キー **`nginx-conf-include-unresolved`** に理由を記録する
  (detail: 未解決の raw 引数と試した候補を各最大 3 件。
  NIC ごとに health を増やさない 05 と同じく **キーは 1 つに集約**する)

### `nginx-upstream-uds`(新規 check)

`opts.NginxConf`(既存入力)の静的解析。**この check は root conf を要求
しない**(機会の提示であり、有効性の断定ではないため)。

- 対象は **`http://` の loopback TCP 宛先のみ**(`127.0.0.1:*` /
  `localhost:*`)→ **info**:
  「同一ホスト内通信は UNIX domain socket に置き換え可能。
  ephemeral port 消費と TCP 処理を回避(ISUCON 本 9-8)」
- **`https://` 宛先は対象外**(v4 修正: UDS 化すると TLS を失う。
  loopback への TLS は意図的な構成であり提案しない)
- 既に `unix:` 宛先 → ok
- 宛先が loopback 以外(別ホスト・コンテナのサービス名)→ skip
  (「対象外: upstream が同一ホストではない」)。
  **コンテナ分離構成(例: private-isu の compose)では発火しない**のが
  正しい挙動
- 推奨文言に前提条件を明記する(v4): (a) app が UDS listen に変更
  できること、(b) nginx と app が socket ファイルの filesystem を
  共有できること(localhost 宛でも別 mount namespace では不可)、
  (c) 同一ホスト間で TLS が不要であること
- **入力がディレクトリ連結だった場合**(`NginxRoot == nil`)は info のまま
  だが、detail 末尾に
  「解析対象: ディレクトリ連結(include されていない .conf を含む可能性)」
  を付す(v6。断定しない旨を出力に残す)
- 重大度は info 固定(defect ではなく機会の提示。キャッシュ検査の
  `nginx-proxy-cache` と同じ整理)

#### UDS の推奨レイアウト(v6)

app 名 `isuconapp`、app 実行ユーザー `isucon`、nginx worker のグループを
`www-data` とした場合の推奨値:

| 対象 | 値 | 理由 |
|---|---|---|
| ディレクトリ | `/run/isuconapp/` | tmpfs・再起動で消える・world-writable ではない |
| ディレクトリ owner:group | `isucon:www-data` | app が socket を作れる / nginx が辿れる |
| ディレクトリ mode | `0750`(setgid を使うなら `2750`) | connect(2) には親ディレクトリの `x` が必要。other からは辿れない |
| socket パス | `/run/isuconapp/app.sock` | |
| socket owner:group | `isucon:www-data` | |
| socket mode | `0660` | Linux では connect(2) に socket への **write 権限**が必要。`0666` にしない |

systemd(stale socket の掃除込み):

```ini
[Service]
User=isucon
Group=www-data            # socket の group を nginx worker と揃える
RuntimeDirectory=isuconapp     # /run/isuconapp を起動時に作成
RuntimeDirectoryMode=0750      # 停止時に自動削除 → stale socket が残らない
ExecStart=/home/isucon/app
```

app 側(systemd を使わない起動でも安全に stale を除去する):

```go
const sockPath = "/run/isuconapp/app.sock"

// 残骸の除去は「socket であること」を確認してから行う。
// 通常ファイル/ディレクトリを黙って消さない(誤設定時の破壊を防ぐ)。
if fi, err := os.Lstat(sockPath); err == nil {
    if fi.Mode()&os.ModeSocket == 0 {
        return fmt.Errorf("%s is not a socket; refusing to remove", sockPath)
    }
    if err := os.Remove(sockPath); err != nil {
        return err
    }
}
ln, err := net.Listen("unix", sockPath) // 生成 mode は umask の影響を受ける
if err != nil {
    return err
}
if err := os.Chmod(sockPath, 0o660); err != nil { // nginx の group から connect 可に
    return err
}
```

`net.Listen` と `os.Chmod` の間には socket が umask 由来の mode で存在する
一瞬があるが、**親ディレクトリが 0750 で other から辿れない**ため到達され
得ない。この点でも `/tmp` 直置きより安全側になる。socket の group は
生成プロセスの egid になるので、`Group=` を nginx 側に揃えるか
ディレクトリを setgid(`2750`)にする。

nginx 側:

```nginx
upstream app {
    server unix:/run/isuconapp/app.sock;
}
```

**`/tmp` を使わない理由**(推奨文言にも 1 行で入れる):

1. `/tmp` は world-writable(sticky)。app 起動前に第三者が同名パスを
   作れるため、bind が EADDRINUSE で失敗する pre-creation DoS が成立し、
   stale 除去ロジックが他人のファイルを消す事故も起こる
2. RHEL 系の `nginx.service` は既定で `PrivateTmp=true`。nginx と app の
   `/tmp` が別 mount namespace になり、同じパスでも到達できない
3. `systemd-tmpfiles` の掃除対象であり、長時間稼働中に socket が消え得る

推奨文言(check の `Recommendation`):

> 同一ホストなら UDS へ。app を `/run/<app>/app.sock` で listen させ
> (専用ディレクトリ 0750・socket 0660・owner を app、group を nginx worker
> と共有)、nginx は `server unix:/run/<app>/app.sock;`。`/tmp` は
> world-writable かつ `PrivateTmp` で分離され得るため使わない。再起動時は
> socket であることを確認してから stale を unlink(systemd なら
> `RuntimeDirectory=` が停止時に自動削除)。

### `nginx-listen-backlog`(新規 check)

**前提は 2 つ**(v6。どちらか欠けたら判定しない):

1. FS から `proc/sys/net/core/somaxconn` が読めること
2. `opts.NginxRoot != nil` で `loadNginxConfTree` が成功していること

前提を欠く場合の出力(いずれも `StatusSkip`。理由を必ず detail に書く):

| 状況 | Detail |
|---|---|
| somaxconn が読めない | 「somaxconn を読めないため backlog との比較ができません」 |
| `NginxRoot == nil`(設定入力がディレクトリ or 未設定) | 「root nginx.conf が未指定(設定入力がディレクトリ)。どの `.conf` が実際に include されているか確定できないため、**有効な listen endpoint を判定しません**」+ Recommendation「`ISUTOOLS_NGINX_ROOT_CONF=/etc/nginx/nginx.conf` を設定してください(`ISUTOOLS_PROXY_CONF` / `ISUTOOLS_NGINX_CONF` のディレクトリ指定は他の nginx check 用にそのまま使えます)」 |
| tree の `Unresolved` が 1 件以上(remap 全段が失敗した include を含む) | 「未解決の include があり listen の見落としが起こり得ます: `<最大3件>`」+ Recommendation「`ISUTOOLS_NGINX_PREFIX` で prefix を合わせてください」。あわせて health `nginx-conf-include-unresolved` を記録する(§include パスの remap 規則)。**未解決部分を推測で埋めて判定することはしない** |
| tree の `Truncated == true` | 「include が上限(深さ16 / 256ファイル / 4MiB)に達したため判定を打ち切りました」 |
| 判定可能な endpoint が 0 件 | 「解析できる listen が見つかりませんでした」 |

**解析単位は「有効な listen endpoint(address:port)」**(v4 修正・v6 で
入力を厳格化): backlog は server ブロックではなく listen socket の属性で、
nginx 仕様では socket parameter は同一 address:port につき 1 箇所の
listen にのみ指定できる。

```go
type listenEndpoint struct {
    Key     string   // 正規化キー。"0.0.0.0:80" / "[::]:80" / "unix:/run/nginx/http.sock"
    Backlog int      // 0 = 未指定
    At      string   // backlog= を指定した "file:line"
    Sites   []string // 同一 Key を持つ listen の "file:line" 一覧
}

// effectiveListenEndpoints は tree の有効ファイルだけを走査し、
// 判定可能な endpoint と、判定を見送った listen の理由を返す。
func effectiveListenEndpoints(tree *nginxConfTree) (eps []listenEndpoint, skipped []string)
```

走査は `{` / `}` の深さと直近ブロック名(`http` / `server` / `stream` /
`mail` / `upstream`)を追跡する保守的スキャナで行う。

正規化規則:

| 記述 | Key | 備考 |
|---|---|---|
| `listen 80;` / `listen *:80;` | `0.0.0.0:80` | address 省略は IPv4 wildcard |
| `listen [::]:80;` | `[::]:80` | `ipv6only=on` 既定のため IPv4 とは別 socket |
| `listen 127.0.0.1:8080;` | `127.0.0.1:8080` | |
| `listen 443 ssl;` | `0.0.0.0:443` | `ssl` / `http2` / `default_server` / `reuseport` 等のフラグは Key に含めない |
| 同一 Key の listen が複数 server にある | 1 つの `listen socket` に集約 | `Sites` に全出現の `file:line` を記録し、`backlog=` は最大 1 箇所の想定 |
| `listen unix:/path;` | `unix:/path` | UDS の accept キューも somaxconn に制限されるため対象 |
| `listen 127.0.0.1;`(port 省略) | — | **skipped**(既定 port の解釈を断定しない) |
| `listen $port;`(変数) | — | **skipped** |
| `stream` / `mail` ブロック内の listen | — | **skipped**(v1 対象外) |

判定(endpoint ごと):

- `backlog=` なし かつ somaxconn ≥ 4096 → **info**:
  「nginx の listen backlog は既定 511。somaxconn=%d を活かすには
  `listen 80 backlog=8192;` 等の明示が必要(同一 address:port では
  1 箇所の listen にのみ指定可)」
- `backlog=` なし かつ somaxconn < 4096 → **ok**(v6 で明示):
  既定 511 と somaxconn の差が小さく、先に上げるべきは somaxconn 側。
  そちらは `os-somaxconn` の担当なので二重に報告しない
- `backlog=N` があり N < somaxconn/2 → **info**(値の乖離を提示)
- `backlog=N` があり N ≥ somaxconn/2 → **ok**
- 同一 Key に `backlog=` が **2 箇所以上** → 当該 endpoint を `skipped` へ:
  nginx は起動時に "duplicate listen options" で失敗する構成であり、
  読んだ conf が実行中のものと異なる疑いがある旨を detail に書く

check は `nginx-listen-backlog` 1 件に集約し、Detail に endpoint 単位の
要約を並べる(例: `0.0.0.0:80 backlog 未指定 / [::]:80 backlog=8192`)。
Status は endpoint 判定の中で最も重いもの(info > ok)。個別 endpoint の
`skipped` は check 全体を skip にはせず、Detail 末尾に
「判定対象外: %d 件」を付す(判定できた endpoint が 0 件のときだけ
check 全体が `StatusSkip`)。

### `go-pgo`(新規 check・小)

ISUCON 本の「Ruby の YJIT を有効にする」の Go 版に相当する
ランタイム最適化として、PGO ビルドの有無を提示する
(ISUCON13 優勝チームも PGO を使用):

- `debug.ReadBuildInfo()` の Settings に `-pgo` があるか確認
  (buildinfo は revision 取得で既に利用しており追加コストなし)
- なし → **info**: 「Go 1.21+ の PGO(default.pgo を置いて再ビルド)で
  数%の改善余地。ベンチ中の CPU プロファイル(/files/ の
  *_cpu.pprof)をそのまま default.pgo に使える」
- あり → ok(適用済みを明示)
- buildinfo が読めない場合は skip

### MTU 表示(計画 05 への追記)

計画 05 の `Interface` に `MTU int64`(`/sys/class/net/<if>/mtu`、
読めなければ省略)を追加し、Network 表に列を足す。判定なし。
speed と同じ sysfs 注入経路のため実装は 05 に含める(本計画からは
参照のみ)。

### 既存 `os-somaxconn` の文言更新(小)

- detail に「Linux 5.4 以降の既定は 4096(それ以前は 128)」を追記し、
  recommendation に書籍の実例値 8192 を併記する(閾値ロジックは不変:
  warn < 1024 を維持)

## 実装ステップ(TDD)

1. `loadNginxConfTree` のテスト先行(fstest.MapFS): include 順・glob 辞書順・
   inactive file 除外・循環打ち切り・変数 include → `Unresolved`・
   prefix 外 include → `Unresolved`・上限超過 → `Truncated`。
   **fixture は §実測根拠 のレイアウトをそのまま写す**
   (`nginx-conf/nginx.conf` + `conf.d/default.conf` + `conf.d/php.conf.org`、
   root の include は `/etc/nginx/conf.d/*.conf`)。
   最低 3 本: (a) remap が必要な絶対 include、(b) どの段でも解決できない
   絶対 include、(c) glob include
2. `advisor.Options.NginxRoot` / `NginxRootConf` の追加と、
   `NginxConf` 単独入力での既存 nginx check 回帰テスト(後方互換)
3. UDS 検査のテスト: loopback TCP(127.0.0.1 / localhost / ポート付き)
   → info、`unix:` → ok、サービス名宛 → skip、
   upstream ブロック経由の loopback → info、推奨文言の退行防止アサーション
4. backlog 検査のテスト: backlog なし + somaxconn 4096 → info /
   backlog なし + somaxconn 1024 → ok(`os-somaxconn` と二重に出さない)/
   backlog=8192 → ok / somaxconn 読めず → skip / `NginxRoot` なし → skip /
   `Unresolved` あり → skip / backlog=256 + somaxconn 4096 → info
5. `isutools.go` の root conf 解決(`ISUTOOLS_NGINX_ROOT_CONF` >
   ファイル指定の `ISUTOOLS_PROXY_CONF`(kind=nginx)> ファイル指定の
   `ISUTOOLS_NGINX_CONF`)と `ISUTOOLS_NGINX_PREFIX`。
   既存の `TestCollectAdviceLegacyNginxConfigStillWorks` を維持
6. `checkNginx` 系への配線(既存の Collect 順序・sort に従う)
7. os-somaxconn 文言更新(既存テストの期待値調整)
8. 計画 05 実装時に MTU 列を追加(05 のテスト計画に含める)
9. docs: INTEGRATION.md「§ nginx transport」(UDS の権限レイアウトと
   systemd 例、backlog の重複指定制約、`ISUTOOLS_NGINX_ROOT_CONF` /
   `ISUTOOLS_NGINX_PREFIX` の説明)、README 環境変数表とチェック一覧

## テスト計画

- unit: `proxy_pass https://localhost:8443;`(TLS 付き loopback)は
  **info を出さない**(https は対象外 — v4)
- unit: コメントアウトされた listen/proxy_pass を無視(stripComments 済み)
- unit: 複数 server ブロックの混在(unix と loopback 併存 → info を出す)
- unit(v6): root から include されない `conf.d/old.conf.bak` に
  `listen 80 backlog=8192;` があっても **判定に使われない**
  (somaxconn=4096 の fixture で `0.0.0.0:80` は「backlog 未指定」→ info)
- unit(v6): `include conf.d/*.conf;` が辞書順で展開される
- unit(v6 監査反映): **remap が必要な絶対 include** — root
  `nginx-conf/nginx.conf` の `include /etc/nginx/conf.d/*.conf;` が
  `nginx-conf/conf.d/default.conf` に解決され、`Unresolved` が 0 件になる。
  同ディレクトリの `conf.d/php.conf.org` は展開結果に**含まれない**
- unit(v6 監査反映): **remap 全段が失敗する絶対 include** — 例
  `include /opt/other/extra.conf;` で `prefix` 配下に該当が無い場合、
  `Unresolved` に raw 引数が入り、`nginx-listen-backlog` は `StatusSkip`。
  健全性理由 `nginx-conf-include-unresolved` が記録され、
  **推測で解決した endpoint が判定に混ざらない**
- unit(v6 監査反映): **glob include** — マッチ 0 件の glob は
  エラーにせず無視され、`Unresolved` にも入らない
- unit(v6): `NginxRoot == nil`(ディレクトリ連結のみ)→
  `nginx-listen-backlog` は skip、Detail に「root nginx.conf が未指定」を含む。
  同じ入力で `nginx-gzip` / `nginx-keepalive` は従来どおり判定される
- unit(v6): `[::]:80` と `0.0.0.0:80` は別 endpoint として扱われる
- unit(v6): `listen 80 default_server;` と `listen 80;` が別 server に
  あるとき **1 endpoint(`0.0.0.0:80`)**に集約され、片方に
  `backlog=8192` があれば ok(v5 の「同一 endpoint の重複 listen」項目)
- unit(v6): 同一 `0.0.0.0:80` に `backlog=` が 2 箇所 → 当該 endpoint は
  `skipped` に入り、Detail に「判定対象外: 1 件」が出る
- unit(v6): `stream { server { listen 3306; } }` は判定対象外
- unit(v6): 判定できる endpoint が 0 件(`stream` の listen のみ)→
  check 全体が `StatusSkip`
- unit(v6・退行防止): UDS check の Recommendation に `/tmp` が含まれず、
  `/run/` を含むこと(文字列アサーション)

## リスク

| リスク | 対策 |
|---|---|
| localhost 宛でも別 netns(コンテナ)で UDS 不可の構成 | info 止まり + 文言を「同一ホスト内なら」と条件付きに |
| backlog 重複指定による起動エラーの誘発 | recommendation に制約を明記(自動修正はしない)。重複検出時は判定せず skip |
| resolver 経由の動的 upstream | 静的に判定できないものは skip(fail-open) |
| root conf のパスと実行中 nginx の `-p` prefix が異なる(コンテナで**常態**。§実測根拠) | 既定 prefix は root conf の親。絶対 include は §include パスの remap 規則で prefix 基準へ読み替える。`ISUTOOLS_NGINX_PREFIX` で上書き可。全段失敗なら未解決 include として skip + health |
| remap が別ディレクトリの同名ファイルを誤って掴む | 候補は決定的順序で **最初の実在 1 件のみ**採用し、段数上限 8・`prefix` 配下チェックで探索範囲を限定。誤解決の疑いが残る構成は prefix の明示指定を Recommendation に出す |
| symlink による prefix 脱出 | prefix チェックは path 文字列ベースで symlink は防げない。read-only アクセス + 件数/深さ/バイト上限で影響を限定 |
| `/run` が tmpfs でない・存在しないホスト | 推奨は「専用ディレクトリ」であり `/run` は既定例。`/var/run` symlink や別パスでも権限要件(0750 / 0660 / owner)は同じと明記 |

## 見積もり

1.5 日(include 解決 loader + 検査 2 件 + 文言更新 + docs)。
MTU は 05 の見積もりに +0.25 日。
