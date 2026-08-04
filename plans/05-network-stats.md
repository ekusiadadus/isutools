# 05: ネットワーク観測(表示のみ)— v6

種別: 機能 / 対象リリース: v1.2.x / 依存: **02 v6**(`BaselineCollector` 契約)、03(procfs/sysfs 分離注入を共有)/ 新規パッケージ: `netstats`

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[MEDIUM] MTU がデータモデルにだけ入り、実装ステップとテスト計画が
   link speed しかカバーしていなかった**。v5 の実装ステップ 4
   「speed 読み(-1・欠損・非数)」だけで MTU も賄えるという暗黙の前提は
   **撤回する**。speed と MTU は同じ `/sys/class/net/<if>/` 配下だが、
   **正常値の範囲も欠損時の意味も異なる**(speed は仮想 NIC で -1 が
   正常、MTU に -1 は存在しない)ため、共通リーダ 1 本では
   境界が固定されない。
   → sysfs 属性リーダを
   **`readLinkAttrs(sysFS fs.FS, ifname string) (linkAttrs, error)`**
   として実装ステップ 3 に独立させ、speed と MTU の**受理範囲を別々に
   定義**した(§sysfs 属性の受理規則)。テスト計画に MTU の
   **欠損 / 非数 / 範囲外 / Jumbo(9000)/ 境界(68・65536)** の
   各ケースと、**MTU 値が advisor 出力を一切変えない**ことの
   退行防止テストを追加した(§テスト計画)。MTU は 11 §非ゴールどおり
   **表示のみ・判定なし**を維持する
2. **[整合] 02 v6 の `BaselineCollector` 契約へ追従した**。v5 §設計メモの
   「03 hoststats と同一 collector 系(…**reset-to-snapshot デルタ**)」
   という記述は**撤回する**。02 v6 で baseline collector は
   `Reset()` / `Snapshot()` ではなく
   **採取値を内包した不変 handle(`runctl.BaselineHandle`)+
   `Collect(base, final)`** に変わったため、v5 の書き方のままでは
   実装できない。
   → §collector 契約(02 v6 準拠)を新設し、`New(procFS, sysFS)` /
   `CaptureBaseline` / `CaptureFinal` / `Collect(base, final)` /
   `Release` の署名、handle が内包する不変 `sample` の中身、
   契約項目の対応表、conformance test を明記した。
   **この変更の副作用として、sysfs(speed / MTU)の読み取りは
   capture 時に行う**必要がある(`Collect` は I/O 禁止)。
   MTU をどのフェーズで読むかが確定するため、上記 1 の前提でもある。
   あわせて v5 の「実装上は **hoststats のサブモジュール**とし、
   flag だけ独立にする」も**撤回する**。02 §登録は `hoststats`(03)と
   `network`(05)を**別々の baseline collector** として並べており、
   `ISUTOOLS_NETSTATS=off` を「`RegisterBaseline` を呼ばない」で
   実現する以上、独立した型と `Name()` が要る。
   → 新規パッケージ **`netstats`** に分離した。
   共有するのは **FS 注入設計(procfs/sysfs 分離)だけ**で、
   lifecycle は各々が 02 v6 契約を実装する(§設計メモ)
3. **[MINOR] ヘッダ版数の陳腐化**。「再設計版」表記のみで版数が
   無かったので **v6** に更新した
4. **[MINOR] 見積もりが README と不一致**。v5 は「1.5 日」で、
   11 から委譲された MTU 列(+0.25 日)を含んでいなかった。
   MTU と `BaselineCollector` 化を計上して **2.0 日**に更新した
   (§見積もり)
5. **[v6 監査反映] クロスファイル整合の調整**:
   - 見積もりの基準を **1.5 日 → 2.0 日(+0.5)** で
     plans/README.md §リリース対応の増分表と一本化した。v6 初稿の
     「README は 05 = 1.75 日のまま / README 側の再算定が必要」という注記は
     **MTU を v5 に先取り計上した二重計上に基づく誤りなので撤回**し、削除した
     (§見積もり)
   - `netstats` が**独立パッケージ**であり、hoststats とは
     **FS 注入設計(procfs / sysfs 分離)だけ**を共有し `Options` は
     共有しないことを §設計メモ で明示した(03 も同内容に訂正済み)
   - `Collect` が 02 の公式アクセサ **`BaselineHandle.Sample()`** から
     固定値を読む(unexported フィールドや Collector の cache を
     経由しない)ことを §collector 契約 と §テスト計画 で明示した

## 旧計画(旧03)からの変更点

第1回レビュー指摘を反映(v6 でも維持):

1. **advisor 閾値を v1 から全廃**(表示のみ)。特に
   「TIME_WAIT > port range の 50% → 枯渇警告」は誤警報になる:
   - 参照記事(ISUCON14)の TIME_WAIT 問題は競技サーバではなく
     **感想戦で改造していたベンチマーカー側の送信元ポート枯渇**
   - `sockstat` の `tw` は incoming/outgoing を区別せず、local ephemeral
     port を消費している socket だけを数えるわけでもない
   - 将来 advisor 化する場合も「outbound-heavy host である証拠」
     (ISUTOOLS_ROLE=proxy 等の宣言 + 10 のトポロジ情報)がある場合に限定
2. **単位の明確化**: `rx_mbit_per_s` / `tx_mbit_per_s`(Mbit/s)。
   NIC の speed(Mbit/s)と直接比較できる単位に統一
3. **新規 NIC の扱い**: base サンプルに無い NIC は `appeared: true` とし、
   **レートを算出しない**(累積値をデルタ扱いすると架空の巨大帯域になる)
4. **sysfs の分離注入**: 現行 procstats の注入 FS は /proc root のため
   /sys/class/net を読めない。03 と共有する procfs/sysfs 分離に乗る
   (README 共通契約 5)
5. **errors 列の出力追加**(読むのに型に無かった)
6. **専用 feature flag**: `ISUTOOLS_NETSTATS`(既定 on / off 可)。
   本機能単体の ABBA を可能にする
7. **限界の明示**: 区間平均のため瞬間飽和は検出できない。
   network namespace 依存(コンテナは自分の netns しか見えない)を
   ドキュメントと identity(03)で明示

## ゴール

ベンチ区間について、判断材料としての**数値表示**を提供する:

- TCP ソケット要約(inuse / tw / orphan、v4・v6)— 点観測
- NIC 別スループット(Mbit/s)・パケット・エラー・ドロップ — 区間デルタ
- リンク速度と **MTU**(読めた場合のみ)を併記し、
  飽和判断・Jumbo Frame の是非は**読者に委ねる**

## 非ゴール

- advisor 警告(v1)。5-tuple 列挙(/proc/net/tcp 全行パース)。
  瞬間値・時系列。
- **MTU の良し悪し判定**。11 §非ゴールと一致させる
  (Jumbo Frame は経路全体の MTU 一致が前提であり、
  片側の NIC の MTU だけからは推奨も警告も導けない)。
  MTU は**表示専用フィールド**であり、advisor の入力にしない

## 識別子の対応(名前が 3 系統あるので明示)

| 用途 | 値 | 出所 |
|---|---|---|
| Go パッケージ名 | `netstats` | 本計画 |
| collector 名(`Name()` / `Registration.Name`) | **`"network"`** | 02 §登録「baseline 型: … `network`(05) …」 |
| `meta.capabilities` 要素 | `netstats` | README 共通契約 4 |
| feature flag | `ISUTOOLS_NETSTATS` | README 共通契約 2 |
| snapshot のキー | `network` (`Snapshot.Network`) | 本計画 |

collector 名だけ `network` なのは 02 の一覧に合わせるためであり、
意図的な差である。02 が改名した場合は本計画も追従する。

## データモデル

```go
type NetworkStats struct {
    TCP        TCPSummary  `json:"tcp"`
    Interfaces []Interface `json:"interfaces"`
}

type TCPSummary struct {          // 点観測(/proc/net/sockstat{,6})。final サンプル時点
    InUse    int64 `json:"in_use"`
    TimeWait int64 `json:"time_wait"` // 方向・port 帰属は区別できない(表示にも注記)
    Orphan   int64 `json:"orphan"`
    InUse6   int64 `json:"in_use6"`
}

type Interface struct {
    Name         string   `json:"name"`
    Appeared     bool     `json:"appeared,omitempty"` // base サンプルに無い(レート無し)
    RxBytes      uint64   `json:"rx_bytes"`           // 区間デルタ
    TxBytes      uint64   `json:"tx_bytes"`
    RxMbitPerSec *float64 `json:"rx_mbit_per_s,omitempty"` // appeared 時は nil
    TxMbitPerSec *float64 `json:"tx_mbit_per_s,omitempty"`
    RxPackets    uint64   `json:"rx_packets"`
    TxPackets    uint64   `json:"tx_packets"`
    RxErrors     uint64   `json:"rx_errors"`
    TxErrors     uint64   `json:"tx_errors"`
    RxDropped    uint64   `json:"rx_dropped"`
    TxDropped    uint64   `json:"tx_dropped"`
    SpeedMbit    int64    `json:"speed_mbit,omitempty"` // /sys/class/net/<if>/speed。不採用時 0 → 省略
    MTU          int64    `json:"mtu,omitempty"`        // /sys/class/net/<if>/mtu。表示のみ(11 委譲)。不採用時 0 → 省略
}
```

- `lo` は既定除外。カウンタ後退(NIC リセット)は該当 NIC を
  appeared 相当(レート無し)に落として errors に記録
- 区間 `d = final.SampledAt.Sub(base.SampledAt)` が **`d <= 0`** の場合は
  レート nil(区間 0 秒。`SampledAt` は `time.Now()` 由来で単調時計成分を
  持つため通常 `d < 0` は起きないが、handle を JSON 経由で再構築した
  経路に備えて `<= 0` で判定する)
- `SpeedMbit` / `MTU` はともに `omitempty` 付き `int64`。
  **不採用は 0 で表現**し、JSON から消える(§sysfs 属性の受理規則)。
  0 は speed / MTU いずれにも実在しない値なので sentinel として安全

## sysfs 属性の受理規則(v6 新設)

`readLinkAttrs(sysFS fs.FS, ifname string) (linkAttrs, error)` が
`speed` と `mtu` を**別々の規則**で判定する。共通処理は
「ファイル読み → `strings.TrimSpace` → `strconv.ParseInt(s, 10, 64)`」
までで、そこから先は属性ごとに分岐する。

```go
type linkAttrs struct {
    SpeedMbit int64 // 不採用は 0
    MTU       int64 // 不採用は 0
    Notes     []string // 不採用理由。health detail に集約される("eth0:mtu=abc" 等)
}
```

| 属性 | パス | 受理範囲 | 不採用の扱い |
|---|---|---|---|
| speed | `/sys/class/net/<if>/speed` | `1 <= v <= 1000000`(Mbit/s。1 Tbit/s を上限とする)。**`-1` はカーネルが定義する "unknown" なので別扱い**(下表) | 0 を格納 → JSON 省略 |
| MTU | `/sys/class/net/<if>/mtu` | **`68 <= v <= 65536`**。unknown を表す規定値は**存在しない** | 0 を格納 → JSON 省略 |

MTU 範囲の根拠(コメントとしてコードに残す):

- 下限 **68** = `ETH_MIN_MTU`(RFC 791 の IPv4 最小再構成バッファ)。
  これ未満を設定できるデバイスは事実上無く、読めたなら破損値
- 上限 **65536** = Linux の `lo` が実際に取る最大値。
  これを超える値は破損値(IP の total length 上限も超える)
- **9000(Jumbo Frame)は範囲内なのでそのまま採用する**。
  Jumbo かどうかの判定・警告は行わない(§非ゴール)

不採用・欠損の切り分け:

| 事象 | speed | MTU | health |
|---|---|---|---|
| ファイル不在(`fs.ErrNotExist`) | 0(省略) | 0(省略) | **出さない**(仮想 NIC・netns・古いカーネルで日常的) |
| read が `EINVAL` で失敗(link down / 仮想 NIC) | 0(省略) | 0(省略) | 出さない |
| `-1`(speed の unknown 規定値) | 0(省略) | — (MTU に unknown 規定値は無いので範囲外扱い) | **出さない** |
| 内容が空 / 非数値(`"abc"`, `"1500 1500"`) | 0(省略) | 0(省略) | `netstats-sysfs-unreadable` |
| 範囲外(speed `0` / `-1` 以外の負値 / `>1000000`、MTU `0` `-1` `67` `65537`) | 0(省略) | 0(省略) | `netstats-sysfs-unreadable` |
| その他の read エラー(`EACCES`・sysFS 自体が注入されていない 等) | 0(省略) | 0(省略) | `netstats-sysfs-unreadable`(detail は `"<if>:mtu=<errno>"`) |

- health キーは **`netstats-sysfs-unreadable` の 1 つ**に集約し、
  detail に `"<if>:<attr>=<raw(先頭 32 byte まで)>"` を
  最大 8 件までカンマ連結する(NIC 数ぶん health が増えるのを防ぐ)
- **speed の `-1` だけ health を出さない**理由: `-1` は
  `ethtool` 由来の速度が取れないことを表す**規定の応答**であり、
  `docker0` / `veth*` / down 中の NIC が並ぶホストでは常態である。
  ここで health を出すと毎 run 必ず 1 件付いて意味を失う。
  **MTU にはこの逃げ道が無い**(unknown を表す規定値が無い)ため、
  `-1` を読んだら破損値として health を出す。
  v5 の「speed の -1 は omitempty で出さない」という結論は維持している

### base と final で属性が変わった場合

sysfs 属性は base / final の**両サンプルで読む**(`Collect` は I/O
禁止のため後読みできない)。値が食い違った場合:

- **final の値を採用する**(snapshot 時点の実効値を表示するため)
- health `netstats-link-changed` を出し、detail に
  `"<if>:mtu 1500->9000"` 形式で記録する
- 片方だけ採用可(もう片方が不採用)の場合は、**採用可な方**を使い
  health は出さない(ベンチ中に NIC が up した等の正常経路)

## collector 契約(02 v6 準拠)

netstats は 02 §登録の **baseline 型**
(`procstats / sqlrows(04) / dbpool(06) / network(05) / hoststats(03)`)
である。

```go
package netstats

// Collector は runctl.BaselineCollector を実装する。
// procFS = /proc、sysFS = /sys を**別 FS として**注入する
//(README 共通契約 5。現行 procstats の root=/proc では /sys を読めない。
// 03 hoststats と同一の注入設計を共有する)。
func New(procFS, sysFS fs.FS) *Collector

var Default = New(os.DirFS("/proc"), os.DirFS("/sys")) // isutools が所有する単一実体

func (c *Collector) Name() string { return "network" } // 02 §登録の一覧に合わせる

// /proc/net/sockstat, /proc/net/sockstat6, /proc/net/dev,
// および NIC ごとの /sys/class/net/<if>/{speed,mtu} を読み、
// deep copy 済みの不変 sample を BaselineHandle に内包させて返す。
func (c *Collector) CaptureBaseline(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error)
func (c *Collector) CaptureFinal(ctx context.Context, runID string, ep runctl.Epoch) (runctl.SampleResult, error)

// 固定済み 2 サンプルだけから *NetworkStats を作る。
// 読んでよいのは **`base.Sample()` / `final.Sample()`**(02 §`BaselineHandle.Sample()`)
// が返す固定値と `SampledAt` だけ。/proc・/sys・time.Now()・Collector の
// 可変内部フィールドへは一切アクセスしない。
func (c *Collector) Collect(base, final runctl.BaselineHandle) (any, error) // → *NetworkStats

// handle が内包する sample 参照を落とす。冪等(二重 Release は no-op)。
func (c *Collector) Release(h runctl.BaselineHandle)
```

handle が内包する不変値(`runctl.BaselineHandle.Sample()` の返り値。
netstats の `Collect` は `base.Sample().(*sample)` /
`final.Sample().(*sample)` と type assert して読む。unexported フィールドや
Collector 側の cache を経由する抜け道は作らない — 02 §`BaselineHandle.Sample()`):

```go
type sample struct {
    TCP  TCPSummary             // 点観測。出力には final 側のみを使う(base 側は捨てる)
    Devs map[string]devCounters // /proc/net/dev の生カウンタ(キー = NIC 名、lo 除外後)
    Link map[string]linkAttrs   // sysfs 由来(speed / mtu)。final 優先でマージ
}

type devCounters struct {
    RxBytes, RxPackets, RxErrors, RxDropped uint64
    TxBytes, TxPackets, TxErrors, TxDropped uint64
}
```

- `sample` は capture 時に**新規 map へコピーして構築**し、以後
  一切書き換えない(Collector の可変内部状態を指さない)。
  `Collect` 側も `Sample()` の返り値を**変更しない**
  (handle は複製・共有され得る。02 §`BaselineHandle.Sample()`)。
  `Sample()` の type assert が失敗した場合は panic させず
  `Code = "collect-failed"` の err を返す
- 区間秒数は `final.SampledAt.Sub(base.SampledAt)` で求める。
  `Collect` 内で `time.Now()` を呼ばない(02 の純粋性契約)
- `TCPSummary` は base / final の両方で採るが、**出力は final のみ**
  (点観測をデルタにしない)。base 側は将来の差分表示のために
  handle には残すが v1 では読まない

登録(isutools.go):

```go
// ISUTOOLS_NETSTATS=off のときは RegisterBaseline 自体を呼ばない
// (capabilities にも netstats を載せない = 単独 ABBA が成立する)
ctrl.RegisterBaseline(runctl.Registration{
    Name:       "network",
    Required:   false, // 02 §登録の既定(required は sqlstats / httpstats のみ)
    SerialOnly: false, // 小ファイルの読み取りのみで並列採取が安全
}, netstats.Default)
```

| 契約項目 | netstats の実装 | 出所 |
|---|---|---|
| `Committed` | 成功時は必ず `Committed = true` / `err = nil`。呼び出し冒頭で `ctx.Err() != nil` の場合のみ `Committed = false` + その err(zero value は返さない) | 02 §Committed セマンティクス |
| 冪等性 | (runID, epoch, phase) をキーに sample を cache し、再呼び出しは **`At` も含めて同一の `SampleResult`** を返す | 02 §Committed セマンティクス |
| 古い epoch | 保持中より古い epoch での呼び出しは `runctl.ErrStaleEpoch` | 02 §Committed セマンティクス |
| `Collect` の純粋性 | `base.Sample()` / `final.Sample()` が返す固定値のみを読む(`sample` への他経路を使わない)。`/proc`・`/sys`・`time.Now()` へ一切アクセスしない | 02 §`BaselineHandle.Sample()` / `BaselineCollector.Collect` / `TestBaselineCollect_UsesFrozenSamplesOnly` |
| per-collector 予算 | `PerCollectorBaselineBudget`(**3.5s**)の内側。05 は独自の秒数を定義しない。読むのは固定 3 ファイル + NIC 数 × 2 の小 sysfs ファイル | 02 §予算モデル |
| 並列採取 | `BaselineConcurrency = 8` の errgroup で他の baseline collector と並列に呼ばれる。`SerialOnly = false` | 02 §並列採取 |
| 境界ウィンドウ | `SampleResult.At` が `StartResult.BoundaryWindow` / `FinishAccepted.BoundaryWindow` に入る。数 ms で `SpreadLimitBoundary`(1500ms)を押し上げない | 02 §境界ウィンドウ |
| 失敗時の run 評価 | optional なので、capture 失敗時も run は `ValidityPartial` に留まり `ValidityInvalid` にはならない | 02 §結果表 |
| 部分失敗 | sysfs が読めなくても `/proc/net/dev` が読めれば **capture は成功**(fail-open)。理由は health に残す | 03 と同方針 |

## 設計メモ

- パッケージは `netstats`(**独立パッケージ**。v5 の hoststats
  サブモジュール案は §v6 での変更点 2 で撤回)。コンストラクタは
  `func New(procFS, sysFS fs.FS) *Collector` で、**hoststats の `Options`
  構造体は共有しない**(03 §「05(network)との関係」と同文言)。
  共有するのは **procfs / sysfs を別 `fs.FS` として注入する設計だけ**で、
  **lifecycle API は 02 v6 の `BaselineCollector` に従う**
  (`Reset()` / `Snapshot()` は使わない)。
  `hoststats` と `netstats` は 02 に**別々に登録**され、
  片方だけ off にできる
- flag は独立(`ISUTOOLS_NETSTATS`)。off で登録しないため
  単独 ABBA が成立する
- 出力は `Snapshot.Network *netstats.NetworkStats`(additive、omitempty)
- 表示: Host セクション内「Network」表。列は
  `NIC / Rx Mbit/s / Tx Mbit/s / Rx pkts / Tx pkts / errors / dropped /
  Speed / MTU`。**MTU 列は値をそのまま出すだけで、色・アイコン・
  推奨文言を付けない**(表示のみの担保)
- TimeWait には「incoming/outgoing の区別なし」の注記を付す
- capabilities: `netstats` を追加

## 実装ステップ(TDD)

1. sockstat / sockstat6 パーサ(フィールド順ゆらぎ・欠損)
2. `/proc/net/dev` パーサ(ヘッダ 2 行・`eth0:`/`eth0: ` 両形式・
   16 フィールドの列位置・`lo` 除外)
3. **`readLinkAttrs(sysFS, ifname)`(v6 新設)**: `speed` と `mtu` を
   **同一の sysfs 注入経路**で読み、§sysfs 属性の受理規則の表どおりに
   受理/不採用を決める。speed と MTU で受理範囲も unknown 規定値の
   有無も違うので **テストは属性ごとに分けて書く**
   (MTU 側は欠損・非数・範囲外・境界・Jumbo・空白除去・health 集約の
   7 群 — §テスト計画「MTU」)
4. `BaselineCollector` 実装: `CaptureBaseline` / `CaptureFinal` で
   不変 `sample` を構築 → `runctl.BaselineHandle` に内包。
   (runID, epoch) 冪等 cache・`ErrStaleEpoch`・`Committed`・
   `Release` の冪等を conformance test で固定(02 §collector 契約)
5. `Collect(base, final)`: デルタ + レート(appeared・カウンタ後退・
   区間 0 秒・Mbit 換算)+ sysfs 属性のマージ(final 優先・
   `netstats-link-changed`)。**I/O を起こさないこと**をテストで固定
6. web 表示(Network 表に **Speed / MTU 列**)+ capabilities + flag
7. docs: INTEGRATION.md「ネットワーク観測」節(netns 依存・
   MTU は表示のみで Jumbo の可否判定はしないこと・11 との関係)、
   README 環境変数表に `ISUTOOLS_NETSTATS`
8. 単独 ABBA(flag off↔on)で影響ゼロを実測し記録

## テスト計画

### パーサ / デルタ(v5 から維持)

- unit: sockstat / sockstat6 の境界すべて(欠損キー・順序違い)
- unit: `/proc/net/dev` の両表記・16 列の位置ずれ検出
- unit: レート nil 条件(`appeared` / カウンタ後退 / 区間 0 秒)
- unit: `lo` 除外・複数 NIC 同時
- unit: TimeWait 注記の固定文がテンプレートに出ること

### speed(v5 から維持)

- unit `TestReadLinkAttrs_SpeedMinusOne`: `"-1\n"` → `SpeedMbit == 0`
  → JSON に `speed_mbit` が出ない。**health も出ない**
  (unknown 規定値。§sysfs 属性の受理規則)
- unit `TestReadLinkAttrs_SpeedMissing`: ファイル不在 → 省略・health なし
- unit `TestReadLinkAttrs_SpeedNonNumeric`: `"unknown\n"` → 省略 +
  health `netstats-sysfs-unreadable`
- unit `TestReadLinkAttrs_SpeedOutOfRange`: `"0"` / `"-2"` /
  `"1000001"` → 省略 + 同 health(`-1` との差を固定する)

### MTU(v6 新設 — 本差し戻しの対象)

すべて `fstest.MapFS` で sysFS を差し替え、speed と**独立に**検証する。

- unit `TestReadLinkAttrs_MTUMissing`:
  `/sys/class/net/eth0/mtu` を置かない → `MTU == 0` →
  JSON に `mtu` キーが**出ない** → health も**出ない**
- unit `TestReadLinkAttrs_MTUNonNumeric`: 内容が
  `"abc\n"` / `""` / `"1500 1500"` / `"1.5e3"` の各ケースで
  `MTU == 0` + health `netstats-sysfs-unreadable` の detail に
  `"eth0:mtu=..."` が含まれる
- unit `TestReadLinkAttrs_MTUOutOfRange`: `"0"` / `"-1"` / `"67"` /
  `"65537"` / `"99999"` の各ケースで `MTU == 0` + 同 health
- unit `TestReadLinkAttrs_MTUBoundary`: `"68"` と `"65536"` は**採用**
  (`MTU == 68` / `65536`)、`"67"` と `"65537"` は不採用。
  受理範囲の境界を上下 1 で固定する
- unit `TestReadLinkAttrs_MTUJumbo`: `"9000\n"` → `MTU == 9000` が
  そのまま JSON に出る(丸め・単位変換をしない)
- unit `TestReadLinkAttrs_MTUTrailingWhitespace`: `"9000\n"` /
  `" 1500 \n"` を採用(`TrimSpace` の確認)
- unit `TestNetstatsMTU_NoAdvice`(**表示のみの退行防止**):
  MTU が `1500` / `9000` / 欠損 の 3 fixture で
  `advisor.Collect` の出力が**バイト等価**であることを assert する。
  MTU を入力に取る advice が将来足されたらこのテストが落ちる
- unit `TestNetstatsRender_MTUColumnPlain`: 描画 HTML に MTU の値は
  出るが、`warn` / `info` 相当の class・推奨文言が**付かない**
- unit `TestNetstatsLinkAttrsChanged_MTU`: base `1500` → final `9000` で
  出力は `9000`、health `netstats-link-changed` の detail に
  `"eth0:mtu 1500->9000"`
- unit `TestNetstatsLinkAttrsAppearedNIC_MTU`: base に無く final にだけ
  ある NIC の MTU は final から採用され、`appeared: true` かつ
  レートは nil のまま(MTU 採用がレート算出条件に影響しない)
- unit `TestReadLinkAttrs_MTUHealthCapped`: 32 NIC すべてが非数値でも
  health は 1 件、detail の列挙は 8 件で打ち切られる

### collector 契約(v6 新設)

- unit `TestNetstatsCapture_Idempotent`: 同一 (runID, epoch) の
  `CaptureBaseline` 再呼び出しが `At` を含めて同一の `SampleResult`
- unit `TestNetstatsCapture_Committed`: 成功時 `Committed == true`、
  期限切れ ctx で `Committed == false` かつ `err != nil`
  (zero value を返さない)
- unit `TestNetstatsCapture_StaleEpoch`: 保持中より古い epoch は
  `runctl.ErrStaleEpoch`
- unit `TestNetstatsCollect_UsesFrozenSamplesOnly`
  (02 `TestBaselineCollect_UsesFrozenSamplesOnly` に対応):
  `Open` が呼ばれたら panic する fake FS に差し替えてから
  `Collect(base, final)` を呼び、完走することを検証する。
  併せて、値が `base.Sample()` / `final.Sample()` 由来であること
  (Collector の cache を空にしても出力が変わらないこと)を assert する。
  **sysfs(speed / MTU)の後読みが混入したらここで落ちる**
- unit `TestNetstatsRelease_Idempotent`: 二重 `Release` が no-op
- unit `TestNetstatsCaptureDegraded_SysfsUnreadable`: sysFS 全体が
  読めなくても capture は成功し、`/proc/net/dev` 由来の値は出る

### integration

- integration: Linux CI スモーク(panic しない・JSON 生成・
  実 NIC の MTU が 1 件以上出る)

## リスク

| リスク | 対策 |
|---|---|
| コンテナで NIC 名/値が期待と違う | netns 依存を注記。identity(03)の NetNS を並記 |
| speed が仮想 NIC で無意味(-1) | unknown 規定値として静かに省略(health も出さない)。飽和判断を機械化しない |
| MTU 表示が「Jumbo にすべき」と誤読される | 判定・推奨文言を出さない。`TestNetstatsMTU_NoAdvice` / `TestNetstatsRender_MTUColumnPlain` で固定。INTEGRATION.md に経路全体の MTU 一致が前提である旨を明記(11 §非ゴールと同文言) |
| MTU の破損値をそのまま表示 | 受理範囲 `68..65536` の外は省略 + health(境界テストで固定) |
| 誤解を招く TimeWait 表示 | 注記固定文をテンプレートに含め、テストで固定 |
| 02 の collector 契約からのずれ | 02 §collector 契約の conformance test を 05 の受け入れ条件に含める(実装ステップ 4・5) |

## 見積もり

**2.0 日**(v5 の 1.5 日 + 0.5 日):

| 追加項目 | 増分 |
|---|---|
| 基本(sockstat / net dev パーサ・デルタ・レート・表示・flag) | 1.5 日 |
| MTU 列(11 から委譲。`readLinkAttrs` の分離 + MTU テスト 11 件) | +0.25 日 |
| `BaselineCollector` 化(不変 sample 内包 handle・(runID, epoch) 冪等 cache・`ErrStaleEpoch`・conformance test) | +0.25 日 |
| **合計** | **2.0 日** |

※ **[v6 監査反映]** v5 基準は **1.5 日**であり、11 から委譲された MTU 列
(+0.25 日)は **v5 では未計上**である。したがって v6 増分は
**1.5 → 2.0(+0.5)** で、plans/README.md §リリース対応の増分表
「05 | 1.5 | 2.0 | +0.5」と一致する。
v6 初稿にあった「README の v1.2.x 集計は 05 = 1.75 日(基本 + MTU)のまま」
「README 側の再算定が必要」という注記は、**MTU を v5 に先取り計上する
二重計上に基づく誤りだったため撤回する**(README 計上規則「二重計上の禁止」)。
README 側は既に訂正済みで、本計画との差分は無い。
