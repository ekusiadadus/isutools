# 11: nginx transport 検査(UNIX domain socket / listen backlog)+ MTU 表示

種別: 機能 / 対象リリース: v1.2.x / 依存: なし(静的検査。v1.1.0 のキャッシュ検査と同パターン)

## 背景

ISUCON 本 9-8「Linux カーネルパラメータ」・9-9「MTU」より。
isutools v1.1.0 は `os-somaxconn`(1024 未満 warn / 4096 推奨)と
`os-port-range`(幅 2 万未満 warn)を実装済みだが、次の 3 点が未対応:

1. **UNIX domain socket**(9-8): nginx→同一ホスト app の TCP 接続
   (`proxy_pass http://localhost:8080`)は UDS
   (`server unix:/tmp/webapp.sock`)へ置き換えると、ephemeral port を
   消費せず TCP オーバーヘッドも避けられる(書籍は「利用できるパターン
   であれば利用することを推奨」)
2. **nginx `listen ... backlog=`**(9-8 の補完): `somaxconn` を上げても
   **nginx の listen backlog は既定 511** のままで accept キューは
   511 に制限される。カーネル側だけ上げて満足する片手落ちを検出する
3. **MTU**(9-9): Jumbo Frame(9000)の適用状態が見えない

## ゴール

- nginx conf の静的検査 2 件(UDS 機会・listen backlog)を advisor に追加
- NIC 別 MTU を表示に追加(判定はしない)

## 非ゴール

- MTU の良し悪し判定(Jumbo Frame は経路全体の対応が前提で、
  環境依存が強い。表示のみ → 計画 05 の方針と同じ)
- アプリ側 listen の UDS 化そのものの検査(アプリ実装は観測できない)

## 設計

### `nginx-upstream-uds`(新規 check)

`opts.NginxConf`(既存入力)の静的解析:

- 対象は **`http://` の loopback TCP 宛先のみ**(`127.0.0.1:*` /
  `localhost:*`)→ **info**:
  「同一ホスト内通信は UNIX domain socket に置き換え可能
  (app 側 listen を `/tmp/webapp.sock` へ、nginx は
  `server unix:/tmp/webapp.sock;`)。ephemeral port 消費と
  TCP 処理を回避(ISUCON 本 9-8)」
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
- 重大度は info 固定(defect ではなく機会の提示。キャッシュ検査の
  `nginx-proxy-cache` と同じ整理)

### `nginx-listen-backlog`(新規 check)

- 前提: FS から somaxconn が読めていること(読めなければ skip)
- **解析単位は「有効な listen endpoint(address:port)」**(v4 修正:
  backlog は server ブロックではなく listen socket の属性。nginx 仕様では
  socket parameter は同一 address:port につき 1 箇所の listen にのみ
  指定できる)。conf 全体から listen directive を address:port で
  グルーピングし、endpoint ごとに backlog 指定の有無・値を判定する
- endpoint に `backlog=` がなく、somaxconn ≥ 4096 → **info**:
  「nginx の listen backlog は既定 511。somaxconn=%d を活かすには
  `listen 80 backlog=8192;` 等の明示が必要(同一 address:port では
  1 箇所の listen にのみ指定可)」
- `backlog=N` があり N < somaxconn/2 → info(値の乖離を提示)
- endpoint 単位で妥当な `backlog=` あり → ok
- 動的・変数を含む listen や解析不能な形式は当該 endpoint を skip
  (保守的に。誤検知で起動エラーを誘発する提案をしない)

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

1. UDS 検査のテスト先行: loopback TCP(127.0.0.1 / localhost /
   ポート付き)→ info、`unix:` → ok、サービス名宛 → skip、
   upstream ブロック経由の loopback → info
2. backlog 検査のテスト: backlog なし + somaxconn 4096 → info /
   backlog=8192 → ok / FS なし → skip / backlog=256 + somaxconn 4096 → info
3. `checkNginx` 系への配線(既存の Collect 順序・sort に従う)
4. os-somaxconn 文言更新(既存テストの期待値調整)
5. 計画 05 実装時に MTU 列を追加(05 のテスト計画に含める)
6. docs: INTEGRATION.md「§ nginx transport」(UDS の unicorn/Go の
   listen 変更例、backlog の重複指定制約)、README チェック一覧

## テスト計画

- unit: `proxy_pass https://localhost:8443;`(TLS 付き loopback)は
  **info を出さない**(https は対象外 — v4)
- unit: コメントアウトされた listen/proxy_pass を無視(stripComments 済み)
- unit: 複数 server ブロックの混在(unix と loopback 併存 → info を出す)

## リスク

| リスク | 対策 |
|---|---|
| localhost 宛でも別 netns(コンテナ)で UDS 不可の構成 | info 止まり + 文言を「同一ホスト内なら」と条件付きに |
| backlog 重複指定による起動エラーの誘発 | recommendation に制約を明記(自動修正はしない) |
| resolver 経由の動的 upstream | 静的に判定できないものは skip(fail-open) |

## 見積もり

1 日(検査 2 件 + 文言更新 + docs)。MTU は 05 の見積もりに +0.25 日。
