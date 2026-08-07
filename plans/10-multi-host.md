# 10: 複数台横断計測 — v6

種別: アーキテクチャ変更 / 対象リリース: v1.5.0 以降 /
依存: 01(TargetID / Purpose)、02(run lifecycle の唯一の正)、03(hoststats/identity)、05(network)
規模: **5〜6 週間**

**本ファイルは 02 の状態機械・予算・DTO を wire へ写すだけの計画**である。
状態名・予算値・collector 契約を独自に定義しない。02 に無い概念
(lease の hub 側トリガ、seal、budget の wire 化、deployment 形態)のみを
本ファイルが定義する。

## v6 での変更点(第5回レビュー差し戻し対応)

1. **[CRITICAL] started / finishing / aborting / finished に明示 lease を定義**。
   v5 は「abort 送信に失敗しても peer の TTL で回収される」と書いたが、
   TTL は finished snapshot にしか定義が無く、started/finishing に落ちた peer は
   02 の `StartedTTL`(30 分)まで新 run を 409 で拒否し続ける。
   → **`PeerStartedLease`(45s・hub が `POST /peer/runs/{id}/lease` で更新)**と
   **`PeerAckLease`(90s・更新しない)**を新設し、finishing/aborting は 02 の
   `FinishLease`(20s)/ `AbortJoinBudget`(2s)をそのまま使う。
   どの状態からでも **hub 沈黙後 `PeerMaxBlockingWindow` = 90 秒**で
   peer は新 run を受理できる状態に戻る(§lease)。
   E2E 必須: **abort 消失 → lease 満了 → 次 run 成功**
2. **[HIGH] peer の状態を `activeRunID` + `map[RunID]RunRecord` に分割**。
   v5 の「単一 state + 直近 2 run 保持」では、run A を abort して run B を
   開始した後の「run A への遅延 GET / Abort」を表現できず、遷移表に
   aborting / acknowledged / expired と「過去 run への API」が無かった。
   → **全 API 判定を RunRecord 単位**にし、遷移表の行を RunRecord state
   (02 の 9 状態すべて + 記録なし)に書き換えた(§状態機械)
3. **[HIGH] 失敗点 × required/optional の後始末(seal)マトリクスを追加**。
   v5 は polling/fetch/validation 失敗時に他 peer へ Ack を送るのか Abort を
   送るのかが未定義で、最大 TTL 分だけ系が停止し得た。
   → 全 participant に必ず 1 回だけ実行する **`sealRun(decision)`** を定義し、
   正常完了 + 6 失敗点 × required/optional の **16 行マトリクス**で
   decision・hub の run 状態・次 run の即時開始可否を確定した(§seal)
4. **[HIGH] app peer の deployment 経路を 2 形態に分離**。
   v5 は「app peer は SQL/HTTP/accesslog/connections/counters を全て含む」と
   書きながら配布経路が `cmd/isutools-agent` の別プロセスしか無く、別プロセスから
   アプリの in-process 状態は読めないため実現不能だった。
   → **(a) embedded `PeerHandler` / `ServePeer`(アプリプロセス内)**と
   **(b) standalone agent binary(DB/DNS/proxy ホスト)**を明示的に定義し、
   形態別に提供可能なセクション表を確定した。
   v5 の「hub と agent は同一 binary バージョン」は**撤回**し、
   library version / protocol version の互換要件に置き換えた(§deployment)
5. **[MEDIUM] wire DTO を項目単位で確定**。`FinishAccepted` / `AbortResult` /
   polling 応答に State/Error 構造が無く、optional participant の失敗を
   hub に伝えるフィールドも無かった。
   → 全 DTO をフィールド単位で定義し、**strict decode(未知フィールド拒否)**と
   **golden による schema 互換テスト**を必須にした(§wire DTO)
6. **[MEDIUM] 識別子の長さ・文字種と保持数の hard cap を定義**。
   TTL だけでは abort/start の高速反復下でメモリが有界にならなかった。
   → `run_id` / `nonce` / `role` / `agent_id` の上限と文字種、
   `RetainedRuns = 2` / `NonceHistoryMax = 64` / `MaxPeers = 8` の hard cap と
   peer あたりメモリ上限の算出式を定義した(§識別子と上限)
7. **[MEDIUM] per-peer budget を wire に載せた**。
   → `GET /peer/runs/{id}/snapshot?max_bytes=N` で hub が予算を渡す方式を採る
   (「受信後に hub が縮小する」案は採らない)。あわせて
   **hub 自身の snapshot が 16MiB を超えた場合の優先規則**を定義した(§budget)
8. **[MINOR] ヘッダを v3 → v6 に更新**。targets ファイルの permission 契約を
   固定 0600 から **`mode & 077 == 0`**(0400 も可)に変更。
   見積もりを 17 日 → **26.5 日**に再算定した(§見積もり)
9. **02 が指摘した deadline の矛盾を修正**。v5 の「StartRun バリア per-peer 3s」は
   02 の `StartRunBudget = 6s` より小さく成立しない。02 §予算モデルが指定した
   下限表を引用する形に置き換えた(§deadline)
10. **v6 監査反映**(v6 草案に対する横断監査で見つかった自己矛盾の解消。
    設計変更ではなく整合化のみ):
    - **セクション表を 2 分割**。旧 v6 の 13 行 1 枚の表には 02 が collector
      として登録しない `connections` / `dbinspect` / `queryplan`(09)/
      `advisor-static`(11)が混在し、`TestPeerSectionsMatchRegistry` が
      成立しなかった。「02 に登録された collector セクション(9 行)」と
      「非 collector セクション(4 行)」に分け、同テストの検証範囲を
      前者に限定した(§deployment)
    - **`Origin` の値集合を `"peer" | "local"` の 2 値に統一**。
      §participant モデルだけで使っていた `"local-hub"` を撤回し、
      hub-local は `PeerResult` の位置(#0)で識別することにした
      (§participant モデル)
    - **「plans/README.md の再算定が必要」を撤回**。README v6 が全計画を
      再算定済みで、本ファイルの 26.5 日と一致することを確認した(§見積もり)
    - **lease 由来の 2 辺が 02 の外部トリガ行の写像であることを明記**。
      `finished → acknowledged`(自己 Ack)と `started → aborting`
      (started lease 失効)が 02 の状態機械への追加ではないことを
      注記し、1 対 1 写像の主張を保った(§状態機械)

## v5 から撤回する主張

| v5 の記述 | 撤回理由 | v6 の扱い |
|---|---|---|
| 「abort 送信に失敗しても各 peer の TTL で最終的に回収される」 | TTL は finished snapshot にしか定義が無い。started/finishing の peer は 30 分間 409 を返し続ける | started/finished に明示 lease を定義し、最大待ちを 90s に有界化(§lease) |
| 「participant 状態機械 = 単一 state。保持は直近 2 run 分」 | 単一 state では「run B 進行中に run A へ GET/Abort」が表現できない | `activeRunID` + `map[RunID]RunRecord`。判定は RunRecord 単位(§状態機械) |
| 「`GET /peer/runs/{run_id}` は finishing 中 202 + 状態、finished で 200 + LocalSnapshot」 | 02 は同 endpoint を `RunStatus` の写像と規定している。状態照会と本体取得を同一 URL に載せると polling deadline と fetch deadline を分離できない | **状態照会 `GET /peer/runs/{id}`(常に 200 + RunStatusDTO)**と **本体取得 `GET /peer/runs/{id}/snapshot`** に分離(§エンドポイント) |
| 「aborted への GET は 410 + abort 理由」 | GET は 02 の `Status`(ok=true を返す)の写像であり、記録がある限り 200 で state を返すべき。410 だと hub が「中止された run」と「未知 run」を区別できない | 記録があれば **200 + RunStatusDTO**(state=aborted/expired)。記録破棄後のみ 404(§状態機械) |
| 「hub と agent は同一 binary バージョンを配布する」 | app peer は binary ではなくアプリにリンクされる **library** であり、同一 binary という概念が成立しない | `protocol_version` 完全一致 / `schema_version` hub ≥ peer / `library_version` は不一致を許容し記録のみ(§deployment) |
| 「app peer は SQL/HTTP/accesslog/connections/counters を全て含む」(無条件) | standalone agent 形態では in-process 状態を読めないため実現不能 | **形態別のセクション能力表**を定義。形態が要求を満たさない場合は preflight 失敗(§deployment) |
| 「StartRun バリア per-peer 3s / total 6s」「FinishRun freeze 受付 per-peer 3s / total 6s」 | 02 の `StartRunBudget` / `FinishSyncBudget` がいずれも 6s であり、hub 側が先に timeout する | 02 §予算モデルの下限表を引用(per-peer 8s / total 12s ほか)(§deadline) |
| 「budget は hub が測定してから決めるが wire 項目は無し」 | peer に予算を伝える手段が無く、hub が受信後に縮小する規定も無かった | `?max_bytes=N` を fetch request に載せる(§budget) |

## v3 での変更点(レビュー差し戻し対応)

1. **[CRITICAL] 終了バリア(FinishRun)の追加**。v2 は開始 reset しか
   同期しておらず、live snapshot を順に fetch すると各 peer の計測終了
   時刻が最大 5 秒以上ずれ、fetch 中のバックグラウンド処理も混入する。
   accesslog の collect も伝播されない。
   → **ResetRun / FinishRun の 2 バリア**と immutable run snapshot を導入
2. **[CRITICAL] required peer の不適合は reset 前に invalid**。v2 の
   「protocol 不一致 peer は接続するが取り込まない」は
   「全 required peer の ACK が揃った場合のみ valid」と矛盾していた。
   → required peer は接続・protocol・schema・必須 capability の
   いずれか不適合で **reset 前に invalid + 503**。optional のみ partial 継続
3. **[HIGH] 配布から `go run @latest` を廃止**(再現不能・ネットワーク
   制限に弱い・version skew の自作)。→ tag/commit 固定の
   事前 build 済み単一 binary + checksum を標準配布経路に
4. **[MEDIUM] wire DTO の分離**。`PeerSnapshot.Snapshot *Snapshot` は
   schema v4 で Snapshot 自身に Peers が加わると再帰する。
   → Peers/Prev を含まない `LocalSnapshot` DTO を新設
5. **[MEDIUM] 容量 budget の決定則**。hub 自身の分を先に確保し、
   required 超過は invalid、optional は partial という決定的優先順位を定義
6. ホスト間 skew は時計比較をやめ、**hub 観測の送信/ACK 区間**で
   不確実性として表現する(02 の方針と整合)

## ゴール

1. アプリ非搭載ホスト(DB/DNS/proxy)で hoststats + netstats + sqlrows +
   dbinspect + queryplan + 静的 advisor check を計測できる standalone agent
2. アプリ搭載ホストで **アプリプロセスに埋め込む peer**(embedded
   `PeerHandler`)を提供し、SQL/HTTP/accesslog/connections/counters を含む
   全セクションを同一 run として hub に渡す
3. hub が全ホストの **同一 run** の snapshot をホスト別に並べて表示する
   (合算はしない)
4. run の開始と終了が全 required peer で揃っていることを**証拠付き**で
   保証し、揃わない run は invalid として保存する
5. **どの失敗経路からも、hub 沈黙後 90 秒以内に全 peer が次の run を
   受理できる状態へ戻る**(v6 で追加。停止状態を作らない)

## 非ゴール

- peer 間の直接通信(全通信は hub ↔ peer の 1 対多)
- hub の HA / フェイルオーバ(hub は単一プロセス。落ちたら lease で回収)
- 複数 hub からの同一 peer 制御(`agent_id` 単位で最後の hub が preempt する)

## deployment 形態(v6・HIGH 対応)

peer は **2 形態**ある。**両者は同じ wire protocol を話すが、提供できる
セクションが異なる**。hub は `PeerInfo.form` と `PeerInfo.sections` で
形態を判別し、preflight でセクション要求を検証する。

### (a) embedded PeerHandler(アプリプロセス内 — app peer 用)

アプリのプロセスに**ライブラリとして**リンクされ、02 の singleton
Controller と全 collector をそのまま共有する。別プロセスから読めない
in-process 状態(httpstats の世代、sqlstats の digest、counters、
`*sql.DB` の pool 統計)を提供できるのはこの形態だけである。

```go
package isutools

type PeerOptions struct {
    Token     string // 共有 secret。32 バイト以上。空なら PeerHandler は起動エラー
    Role      string // 03 の Identity.Role。既定 "app"。文字種は §識別子と上限
    MaxBytes  int64  // 自ホスト snapshot の自主上限(既定 PeerSelfCapBytes = 8MiB)
    AccessLog string // accesslog を読む場合の path(空なら accesslog セクションなし)
}

// PeerHandler は peer protocol の http.Handler を返す。
// パスは "/peer/..." を前提とし、mux 側で http.StripPrefix しない。
// 02 の singleton Controller を包むだけで、独自の状態機械を持たない。
func PeerHandler(o PeerOptions) (http.Handler, error)

// ServePeer は loopback 専用 listener を作って PeerHandler を提供する簡便形。
// addr は literal loopback のみ("127.0.0.1:PORT" / "[::1]:PORT")。
// それ以外の addr は ErrPeerListenerNotLoopback で起動失敗する。
func ServePeer(ctx context.Context, addr string, o PeerOptions) error
```

- **推奨配線は `ServePeer` の専用 listener**。`PeerHandler` は
  「既に loopback 専用の管理用 listener を持つアプリ」向けの下位 API であり、
  **アプリの公開 mux へ mount する構成は非サポート**とする
  (公開ポートから peer protocol が到達可能になるため)。
  この 2 点を INTEGRATION.md に明記する
- feature flag(README 共通契約 2): **`ISUTOOLS_PEER`(既定 off)**。
  listener を増やす機能なので既定 off とし、`ServePeer` / `PeerHandler` は
  flag が off のとき `ErrPeerDisabled` を返す
- 認証: 全 endpoint で `Authorization: Bearer <token>` を要求し、
  `crypto/subtle.ConstantTimeCompare` で検証する。token は
  `ISUTOOLS_PEER_TOKEN` から取る。不一致・欠落は **401**(本文は
  `ErrorDTO{code:"unauthorized"}` のみ。状態を漏らさない)
- run の origin: この handler 経由で開始された run は
  `RunRecord.Origin = "peer"` になり、**§lease の対象**になる。
  ローカルの `POST /reset` で開始された run は `Origin = "local"` で
  lease の対象外(02 の `StartedTTL` が backstop)

### (b) standalone agent(`cmd/isutools-agent` — DB/DNS/proxy ホスト用)

自プロセスに独自の 02 Controller を持ち、**ホスト/OS/DB セクションのみ**を
登録する。アプリの in-process 状態は原理的に読めない。

### 形態別セクション能力表(hub の preflight 判定の根拠)

**v6 監査反映**: 旧 v6 は 1 枚の表で「`PeerInfo.sections` は登録済み
collector から導出する」と書いたが、表には 02 §登録が collector として
挙げていない `connections` / `dbinspect` / `queryplan`(09)/
`advisor-static`(11)が混ざっており、`TestPeerSectionsMatchRegistry` が
そのままでは成立しなかった。表を **02 登録 collector** と **非 collector** の
2 枚に分割し、同テストの検証範囲を前者に限定する。

#### (1) 02 に登録された collector セクション

02 §登録の一覧(世代型: `httpstats` / `sqlstats` / `accesslog` / `counters`、
baseline 型: `procstats` / `sqlrows`(04)/ `dbpool`(06)/ `network`(05)/
`hoststats`(03))と **行が 1 対 1** である。行の増減は 02 側の変更を伴う。

| セクション | 02 の種別 | (a) embedded | (b) agent | 理由 |
|---|---|---|---|---|
| `httpstats`(HTTP) | 世代型 | ○ | **✕** | in-process middleware の世代型 collector |
| `sqlstats`(digest) | 世代型 | ○ | **✕** | proxy driver 経由の in-process 観測 |
| `accesslog` | 世代型 | ○ | △(`--accesslog` 指定時のみ。既定 ✕) | ファイル読みなので nginx ホストでも可 |
| `counters` | 世代型 | ○ | **✕** | in-process カウンタ |
| `procstats` | baseline | ○(アプリプロセス) | **✕**(agent 自身は計測対象ではないので出さない) | 対象プロセスが違う |
| `sqlrows`(04) | baseline | ○ | ○ | 01 の `Inspect(ctx, id, PurposeStats, ...)` |
| `dbpool`(06) | baseline | ○ | **✕** | `*sql.DB` の in-process 統計 |
| `network`(05) | baseline | ○ | ○ | /proc・/sys。**collector 名は `network`**(Go パッケージ名は `netstats`。05 §命名の対応表に従う) |
| `hoststats`(03) | baseline | ○ | ○ | /proc・/sys |

#### (2) 非 collector セクション(snapshot に載るが Controller に登録しない)

02 の `RegisterGeneration` / `RegisterBaseline` を経由せず、snapshot 構築時に
直接組み立てるセクション。**run 境界の `CollectorBoundary` を持たない**ため、
`TestPeerSectionsMatchRegistry` の対象外である。

| セクション | (a) embedded | (b) agent | 導出元 | 理由 |
|---|---|---|---|---|
| `connections` | ○ | **✕** | in-process の `net.Conn` 追跡 | httpstats の conn トラッカから直接読む |
| `dbinspect` | ○ | ○ | 01 の `Inspect(ctx, id, PurposeStats, ...)` | スキーマ/インデックスの一時点情報。区間値ではない |
| `queryplan`(09) | ○ | ○ | 01 の `Inspect(ctx, id, PurposeExplain, ...)` 登録時 | EXPLAIN 結果。区間値ではない |
| `advisor-static`(11) | ○ | ○ | 設定ファイル読みのみ | 静的 check。区間値ではない |

- `PeerInfo.sections` = **(1) を登録済み collector から実行時に導出したもの**
  ∪ **(2) のうち当該形態で提供可能なもの**。定数表を持つのは (2) だけである
- テスト **`TestPeerSectionsMatchRegistry`** は **(1) の表と 02 Controller の
  実際の登録内容の一致だけ**を固定する。旧 v6 の
  「上表(= 13 セクション全部)と登録内容の一致を固定する」という記述は
  **撤回**する((2) は Controller に登録されないため一致し得ない)。
  (2) の形態別提供可否は **`TestPeerNonCollectorSections`** で別途固定する
- hub は `HubConfig.RequiredSections[peer] []string` を持ち、
  preflight で `sections ⊇ RequiredSections` を検証する。欠落は
  required peer で **503 + invalid**、optional peer で **partial**
  (v5 の「app peer は全セクションを含む」という無条件の主張はここで撤回)

### バージョン互換の要件(v5 の「同一 binary」を置換)

| 項目 | 取得元 | required peer の判定 | optional peer の判定 |
|---|---|---|---|
| `protocol_version`(int。本計画は **1**) | 定数 | **完全一致でなければ preflight 失敗** | 不一致は除外 + partial |
| `schema_version`(LocalSnapshot の schema。README 共通契約 4) | 定数 | **hub ≥ peer** でなければ失敗。additive 差は許容 | 同左、失敗時は partial |
| `library_version`(module version) | embedded は `debug.ReadBuildInfo()`、agent は buildinfo | **一致を要求しない**。差異は hub の表示と health `multihost-version-skew` に記録 | 同左 |
| `capabilities`(README 共通契約 4) | 実行時導出 | hub の必須 capability を満たさなければ失敗 | partial |
| `sections`(上表) | 実行時導出 | `RequiredSections` を満たさなければ失敗 | partial |

`library_version` を一致要件にしないのは、embedded peer が
「アプリの go.mod が指す isutools のバージョン」で決まり、hub 側から
差し替えられないためである。互換の権威は `protocol_version` と
`schema_version` だけに置く。

## run プロトコル(protocol_version = 1)

02 の run lifecycle を **一対一で wire に写す**。

### エンドポイント

```
GET  /peer/info                          handshake(識別・互換性・sections)  → PeerInfoDTO
POST /peer/runs                          StartRun                            → StartResultDTO
POST /peer/runs/{run_id}/lease           started lease の更新                → LeaseDTO
POST /peer/runs/{run_id}/finish          FinishRun(freeze 受付)             → FinishAcceptedDTO
GET  /peer/runs/{run_id}                 状態照会(02 の RunStatus の写像)   → RunStatusDTO
GET  /peer/runs/{run_id}/snapshot        immutable snapshot 取得(max_bytes) → LocalSnapshot
POST /peer/runs/{run_id}/abort           AbortRun(冪等)                     → AbortResultDTO
POST /peer/runs/{run_id}/ack             Ack(冪等)                          → 204
```

**02 の `Await` は wire に写さない**(意図的な逸脱)。HTTP で長時間
ブロックする endpoint を作らず、hub は `GET /peer/runs/{id}` の polling で
同じ役割を果たす(§deadline の polling 行)。この 1 点以外は 02 の
公開 API と 1 対 1 である。

### wire run_id と local runID の対応(02 の API を変えないための規約)

02 の `StartRunOptions` に `RunID` フィールドは**無く**、runID は
Controller が発番する。したがって hub の run_id を peer の runctl へ
そのまま押し込むことはできない。peer は **1 対 1 の対応表**を持つ:

```go
type RunRecord struct {
    RunID       string          // hub が発番した wire 上の run_id(本表のキー)
    LocalRunID  string          // runctl が発番した peer プロセス内の runID
    Nonce       string
    Origin      string          // "peer" | "local"
    State       runctl.RunState // 02 の 9 状態をそのまま使う
    Validity    runctl.Validity
    Epoch       runctl.Epoch
    Start       *runctl.StartResult
    Finish      *runctl.FinishAccepted
    Abort       *runctl.AbortResult
    Snapshot    *LocalSnapshot  // finished / acknowledged のみ
    LeaseExpiry time.Time       // started: PeerStartedLease / finished: PeerAckLease
    AckedBy     string          // 02 §AckedBy の値集合(explicit|save|preempt|hub|lease)。
                                    // 10 は値を追加しない。local origin の run は save / explicit も取り得る
    ExpiryReason string         // "started-lease-expired" | "ack-lease" | ""
    UpdatedAt   time.Time
}
```

- peer 側の runctl 呼び出しは常に `LocalRunID` を使う。
  `StartRunOptions{Nonce: <hub の nonce>, Preempt: <wire の preempt>,
  Reason: "hub", Trigger: "hub:" + RunID}`
  (`Reason = "hub"` は 02 の `StartRunOptions.Reason` の定義済み値)
- 02 の nonce 冪等キャッシュ(`NonceTTL` 10m / `NonceHistoryMax` 64)は
  そのまま効く。wire 側も同じ nonce をキーに保存済み DTO を返すため、
  2 層で同一の結果になる
- DTO には `run_id`(wire)と `local_run_id`(参考・デバッグ用)の
  両方を載せる。hub は結合に `run_id` のみを使う

### peer の状態保持(v6・HIGH 対応)

```go
type peerStore struct {
    mu          sync.Mutex
    activeRunID string                 // "" = 新 run を即受理できる
    runs        map[string]*RunRecord  // 最大 RetainedRuns = 2 件(02 と同値)
    nonces      *nonceLRU              // NonceHistoryMax = 64 / NonceTTL 10m
    clock       Clock                  // テストで注入(lease 検証のため必須)
}
```

- **`activeRunID` の定義**: `State ∈ {starting, started, finishing, finished,
  aborting}` の RunRecord。これは 02 遷移表 A で
  `StartRun(other, Preempt=false)` が `ErrRunActive` を返す集合と**同一**である
- `acknowledged` / `aborted` / `expired` は **past run** であり
  `activeRunID` から外れる(= 新 run をブロックしない)
- `runs` は最大 2 件。新 run を開始するとき既に 2 件あれば、
  **最古の非 active record を TTL を待たずに破棄**する。したがって
  「2 世代前の run」への API は 404 になる(§識別子と上限)

### handshake(v2 から維持 + 形態判定を追加)

`GET /peer/info` → `PeerInfoDTO`(§wire DTO)。**required peer** は
次のいずれかで **run を開始せず invalid + 503**:
接続不可 / `protocol_version` 不一致 / `schema_version` 非互換 /
必須 capability の欠如 / **必須 section の欠如(v6 追加)**。
optional peer は participant から除外し partial 記録で継続する。

**identity の 2 層分離(v4 修正・v5 精緻化 — v6 で維持)**:

- **host identity**(03 の `Identity.MachineIDHash` / `BootIDHash`):
  hoststats の host 単位 dedup に使う。拒否はしない。
  **観測範囲を無視した dedup はしない**: dedup キーは
  (host identity, namespace 群(`PIDNS`/`NetNS`/`MntNS`/`CgroupNS`),
  `cgroup_scope`) とし、**集約の代表値には `cgroup_scope == "host"` の
  agent だけを採用**する。host scope が無い場合は代表値を出さず、
  各観測を scope 付きで並記する
- **agent instance identity**(`agent_id`: 起動時に生成し DataDir に
  永続化する UUID v4): **peer の識別に使う**。
  同一 `agent_id` の二重指定のみ設定エラー

### participant モデル(v4 修正 — v6 で維持)

- participant = hub-local(#0)+ 各 peer。Start/Finish/Abort/Ack/seal は
  全 participant へ**並列に**発行する(hub-local は関数呼び出し、
  peer は HTTP)。分岐は「送信手段」だけで、状態機械と順序は同一
- 各バリアで participant ごとに送信/ACK 時刻を記録し、
  **uncertainty = [送信, ACK] 区間**として保存する(hub-local は
  実行区間そのもの)。**ホスト間の時計は比較しない**
- hub-local participant の RunRecord は `Origin = "local"` とする。
  **v6 監査反映**: 旧 v6 がここだけで使っていた `Origin = "local-hub"` は
  **撤回**する(`RunRecord.Origin` と `RunStatusDTO.origin` の値集合は
  `"peer" | "local"` の 2 値のみと定義しており、3 値目は存在しない)。
  hub-local であることは **`PeerResult` の位置(participant #0)**で識別し、
  `Origin` では区別しない。`Origin == "local"` なので **wire lease の
  対象外**(hub 自身の生存が run の生存であるため)であり、02 の
  `StartedTTL` が backstop になる

### StartRun バリア

1. hub が run_id / nonce を発番(§識別子と上限の規則)し、preflight
   (handshake 再検証 + section 検証)。required 不適合 →
   **run を開始せず invalid + 503**(seal マトリクス #1)
2. 全 participant へ `POST /peer/runs` を並列発行。各 participant は
   02 の `StartRun`(start-boundary 逐次 → start-baseline 並列)を実行し、
   不変の `StartResultDTO` を返す
3. **全 required ACK が揃ってから** hub の `POST /reset` が応答する。
   required 失敗 → 503 + invalid(bench を開始させない)。optional → partial
4. **部分開始からの回復(v5・CRITICAL 対応を維持)**: required 失敗時、
   hub は 503 を返す**前に** `sealRun(SealAbort)` を実行する(§seal)。
   invalid 記録には**部分開始 participant 一覧**と各 abort の成否を含める
5. 成功したら hub は **lease renewer goroutine** を起動する(§lease)

### FinishRun バリア(02 の終了契約を全 collector に適用)

1. **freeze phase(高速・同期)**: 各 participant で 02 の
   `FinishRun` を呼ぶ(finish-freeze 逐次 → finish-final 並列)。
   これが全セクション共通の計測終了境界である。
   ただし 02 §境界ウィンドウのとおり境界は**瞬間ではなく幅を持つ区間**であり、
   `BoundaryWindow{Min,Max,Spread}` として記録する
2. participant は freeze 完了時点で **`FinishAcceptedDTO`** を即返す
   (Drain・snapshot 構築は待たない — deadline 分離のため)
3. peer は background で `Drain` → `Collect` → **固定値だけから**
   `LocalSnapshot` を構築して immutable 保存(state=`finished`)。
   この worker の生存期限は 02 の `FinishLease`(20s)で、超過時は
   02 遷移表 B により `aborting → aborted`(理由 `finish-lease-expired`)
4. hub は `GET /peer/runs/{run_id}` を **polling** して
   `state == "finished"` を待つ(202 は返さない。常に 200 + RunStatusDTO)
5. hub は自分の snapshot サイズを実測して per-peer budget を決め(§budget)、
   `GET /peer/runs/{run_id}/snapshot?max_bytes=N` で本体を取得する
6. hub は取得結果を検証し(§wire DTO の strict decode)、
   **必ず `sealRun` を実行する**(§seal)

### deadline(02 §予算モデルの下限表を引用 — v5 の値は撤回)

02 は「peer 側は 02 の予算をそのまま使うため、hub の deadline は
**02 の予算 + RTT マージン**でなければならない」と規定している。
v5 の「StartRun バリア per-peer 3s」は `StartRunBudget = 6s` より小さく
成立しないため撤回する。

| フェーズ | per-peer | total | 出所 |
|---|---|---|---|
| StartRun バリア | **8s** | **12s** | 02: `StartRunBudget`(6s)+ 2s |
| FinishRun freeze 受付 | **8s** | **12s** | 02: `FinishSyncBudget`(6s)+ 2s |
| snapshot polling(`GET /peer/runs/{id}`) | **25s** | **40s** | 02: `FinishLease`(20s)+ 5s |
| snapshot fetch(`GET .../snapshot` の body 読み) | **10s** | **20s** | 4MiB / 8 peer / 並行度 4 の実測見込み。本計画が定義 |
| AbortRun 伝播 | **4s** | **8s** | 02: `AbortJoinBudget`(2s)+ 2s |
| Ack 伝播 | **3s** | **6s** | map 更新のみ。本計画が定義 |
| lease 更新 | **2s** | **4s** | 同上 |
| 検証(decode + schema + サイズ) | — | **5s**(`ValidateBudget`) | 本計画が定義 |

- polling の間隔は `PollInterval = 250ms`(指数バックオフしない。
  上限は total で押さえる)
- 定数テスト `TestHubDeadlinesExceedPeerBudgets`: **02 に対応する予算定数がある
  4 フェーズ**(StartRun / FinishRun freeze / polling / AbortRun 伝播)について、
  hub の per-peer 値が 02 の定数より**厳密に大きい**ことを CI で固定する。
  fetch / Ack / lease / 検証は 02 に対応物が無いため本計画が単独で定義する

## lease(v6・CRITICAL 対応)

**問題**: v5 は「abort 送信に失敗しても各 peer の TTL で回収される」と
書いたが、TTL(`FinishedTTL`)は **finished snapshot にしか定義が無い**。
started / finishing に落ちた peer を回収するのは 02 の
`StartedTTL = 30m` だけで、その間 `StartRun(other, Preempt=false)` は
**409 を返し続ける**。この主張は撤回する。

### lease 一覧

| lease | 対象 RunRecord state | 既定値 | 更新 | 満了時の動作 |
|---|---|---|---|---|
| **`PeerStartedLease`**(本計画) | `starting` / `started` | **45s**(hub が `lease_ms` で要求可。上限 `PeerStartedLeaseMax` = **90s**) | hub が `POST /peer/runs/{id}/lease` を `PeerLeaseRenewInterval`(**10s**)ごとに送る | `runctl.AbortRun(LocalRunID, "hub-abort")` → `aborted`。`ExpiryReason = "started-lease-expired"`。health `multihost-lease-expired` |
| `FinishLease`(**02 を引用**) | `finishing` | 20s | **更新しない** | 02 遷移表 B: `aborting → aborted`(理由 `finish-lease-expired`) |
| `AbortJoinBudget`(**02 を引用**) | `aborting` | 2s | — | 02: `aborted`(`Detached=true`)。以後 aborting に留まらない |
| **`PeerAckLease`**(本計画) | `finished` | **90s** | **更新しない** | 自己 `runctl.Ack(LocalRunID)` → `acknowledged`。`AckedBy = "lease"`。**snapshot は破棄しない** |
| `FinishedTTL`(**02 を引用**) | `finished` / `acknowledged` | 10m | — | `expired`(snapshot 解放・tombstone) |
| `TombstoneTTL`(**02 を引用**) | `aborted` / `expired` | 10m | — | 記録破棄 → 以後 404 |

- lease は **`Origin == "peer"` の RunRecord にのみ適用**する。
  ローカルの `/reset` で始まった run(`Origin == "local"`)は
  02 の `StartedTTL`(30m)のままで、単一ホスト運用を壊さない
- 満了検査は **1 秒周期の watchdog**(02 の `FinishLease` watchdog と同一の
  仕組み)で行う。`clock` は注入可能にし、テストで実時間を待たない

### なぜ `PeerAckLease` が「finished でも new run を通す」ための鍵か

02 遷移表 A では `finished` に対する `StartRun(other, Preempt=false)` は
`ErrRunActive`(409)だが、`acknowledged` に対しては **新規開始**である。
`PeerAckLease` 満了で自己 Ack して `acknowledged` へ移すことで、
hub が消えても finished の run が新 run をブロックしなくなる。
`FinishedTTL`(10m)は変えないので、**遅れて来た GET は snapshot を
そのまま受け取れる**(データを捨てずに閉塞だけを解く)。

### 有界性(この節の結論)

```
PeerMaxBlockingWindow
  = max(PeerStartedLeaseMax 90s,          // 既定は PeerStartedLease 45s
        FinishLease 20s + AbortJoinBudget 2s,
        PeerAckLease 90s)
  = 90s
```

> **どの状態からでも、hub が沈黙してから 90 秒以内に peer は
> 新しい run を受理できる状態(`acknowledged` / `aborted`)へ戻る。**

不等式(定数テスト `TestPeerLeaseBounds` で固定):

```
PeerLeaseRenewInterval(10s) * 3            <  PeerStartedLease(45s)
StartRun バリア total(12s)                 <  PeerStartedLease(45s)
PeerStartedLease(45s)                      <= PeerStartedLeaseMax(90s)
PeerStartedLeaseMax(90s)                   <= PeerMaxBlockingWindow(90s)
FinishLease(20s) + AbortJoinBudget(2s)     <  PeerAckLease(90s)
polling total(40s) + fetch total(20s) + ValidateBudget(5s) = 65s  <  PeerAckLease(90s)
PeerAckLease(90s)                          <= FinishedTTL(10m)
PeerMaxBlockingWindow(90s)                 == max(PeerStartedLeaseMax,
                                                  FinishLease + AbortJoinBudget,
                                                  PeerAckLease)
```

`PeerStartedLeaseMax` を `PeerAckLease` と同値の 90s で頭打ちにするのは、
hub が `lease_ms` に大きな値を要求しても `PeerMaxBlockingWindow` が
90 秒を超えないようにするためである(この上限が無いと有界性の主張が崩れる)。

4 行目が重要で、**正規の hub による Ack は必ず自己 Ack より先に届く**。
自己 Ack が先行しても 02 の `Ack` は `acknowledged` に対して no-op 成功
なので、hub の Ack が失敗扱いになることはない。

### hub 側の即時回復(preempt)

lease は「hub が消えた」場合の backstop であり、hub が生きている場合は
**待たずに回復できる**。02 v6 が追加した `Preempt` をそのまま wire に写す:

- `StartRunRequest.preempt` は 02 の `StartRunOptions.Preempt` に直結する
- hub の設定 `HubPreemptPolicy`:
  - **`"on-unacked"`(既定)**: hub のローカル記録に「その peer で
    ack も abort も確認できていない run」が残っている場合、および
    409 `run_active` を受けた直後の 1 回だけ再送する場合に `preempt=true`
  - **`"never"`**: 常に `preempt=false`。**lease 経路の E2E で使う**
- preempt された run は 02 の規定どおり `aborted` + `ValidityInvalid` として
  記録される(`AbortResult.Reason = "preempted-by:<新 runID>"`)。
  ただし `finished` の run を preempt した場合は snapshot を保持し
  `AckedBy = "preempt"` になる(02 遷移表 A)

### 必須 E2E(v6)

| ケース | 手順 | 期待 |
|---|---|---|
| **abort 消失 → lease 満了 → 次 run 成功** | `HubPreemptPolicy="never"`。run A を start → abort 送信を fault injection で握り潰す → 注入 clock を `PeerStartedLease` + 1s 進める → run B を start | peer の RunRecord A が `aborted`(`ExpiryReason="started-lease-expired"`)、run B が **201** |
| **finished の閉塞が ack lease で解ける** | run A を finish まで進めた後 hub を停止 → clock を `PeerAckLease` + 1s 進める → 新 hub が run B を start | A が `acknowledged`(`AckedBy="lease"`)、B が **201**、A への GET が **200 + snapshot あり** |
| **abort 消失 → preempt → 即時に次 run 成功** | 既定 policy。abort を握り潰し、clock を進めずに run B を start | B が **201**、A は `aborted` + `Reason="preempted-by:B"` |

## 状態機械(v6・HIGH 対応 — 判定は RunRecord 単位)

状態名は 02 の `runctl.RunState` を**そのまま**使う。

```
idle ─StartRun─▶ starting ─成功─▶ started ─FinishRun─▶ finishing ─worker完了─▶ finished
                                                                                  │
                                                                    Ack ──────────┤
                                                                                  ▼
                                                                            acknowledged
                                                                                  │
   finished / acknowledged ─FinishedTTL─▶ expired ◀────────────────FinishedTTL────┘

   starting(required 失敗)                    ┐
   started(PeerStartedLease 満了 / StartedTTL) ├─ AbortRun / Preempt ─▶ aborting ─join─▶ aborted
   finishing(FinishLease 満了)                 ┘
   finished ─PeerAckLease 満了─▶ acknowledged(自己 Ack。snapshot は保持)

   aborted / expired ─TombstoneTTL─▶ 記録破棄(以後 404)
```

**`PeerAckLease` / `PeerStartedLease` の 2 辺について(v6 監査反映)**:
上図の `finished ─PeerAckLease 満了─▶ acknowledged`(自己 Ack)と
`started ─PeerStartedLease 満了─▶ aborting` は、**10 が 02 に無い状態遷移を
足したものではない**。いずれも 02 v6 が §遷移表 B に追加する
**外部トリガ行**(外部からの Ack / 外部 lease 失効による回収)を wire 側から
叩くだけであり、10 が定義するのは**トリガの契機と既定値**
(`PeerAckLease` 90s / `PeerStartedLease` 45s)だけである。
したがって「10 の状態機械は 02 の 1 対 1 の写像である」という主張は
この 2 辺を含めても成立する。

### 遷移表(HTTP status と返却 DTO を一意に定義)

**行の読み方**(v6 で明文化):

- **`StartRun(別 run_id)` の 2 列だけ**は、行を **`activeRunID` が指す
  RunRecord の state** として読む(要求 run_id の記録はまだ無いため)
- それ以外の全列は、行を **path の `run_id` が指す RunRecord の state** として読む
- `same` = 同一 run_id かつ同一 nonce。**StartRun において** run_id が一致して
  nonce が異なる場合は **409 `nonce_mismatch`**(他の API は nonce を運ばない)
- `記録なし` 行の `StartRun` 列は **`activeRunID` が無い場合の値**である。
  `activeRunID` が有る場合は、その active RunRecord の行の
  `StartRun(別, preempt=…)` 列に従う

| RunRecord state | StartRun(same) | StartRun(別, preempt=false)<br>※行=active | StartRun(別, preempt=true)<br>※行=active | Finish(same) | Lease(same) | GET(same) | Snapshot(same) | Abort(same) | Ack(same) |
|---|---|---|---|---|---|---|---|---|---|
| **記録なし**(未知 / 破棄済み run_id) | **201** + StartResultDTO(新規開始) | 201 新規開始 | 201 新規開始 | 404 `unknown_run` | 404 | 404 | 404 | **200** + AbortResultDTO(no-op。冪等) | 404 |
| `starting` | **200** + StartResultDTO(完了を ≤`StartRunBudget` 待つ。超過は 409 `run_transitioning`) | **409** `run_active` | **201**(fence→cancel→join→新規) | 409 `run_transitioning` | **200** + LeaseDTO(expiry 更新) | **200** + RunStatusDTO | **409** `not_finished` | **200** + AbortResultDTO | 409 `run_active` |
| `started` | **200** 保存済み StartResultDTO | 409 `run_active` | 201 | **200** + FinishAcceptedDTO | **200** + LeaseDTO | 200 + RunStatusDTO | 409 `not_finished` | 200 + AbortResultDTO | 409 `run_active` |
| `finishing` | 200 保存済み StartResultDTO | 409 `run_active` | 201 | **200** 保存済み FinishAcceptedDTO(冪等) | **409** `lease_not_renewable`(02 の `FinishLease` が支配) | 200 + RunStatusDTO | 409 `not_finished` | 200 + AbortResultDTO | 409 `run_active` |
| `finished` | 200 保存済み StartResultDTO | 409 `run_active` | **201**(snapshot 保持・`AckedBy="preempt"`) | 200 保存済み FinishAcceptedDTO | 409 `lease_not_renewable` | 200 + RunStatusDTO | **200** + LocalSnapshot | 200 + AbortResultDTO(**snapshot 破棄**) | **204** → `acknowledged` |
| `acknowledged`(**過去 run**) | 200 保存済み StartResultDTO | ―(active でない) | ―(active でない) | 200 保存済み FinishAcceptedDTO | 409 `lease_not_renewable` | 200 + RunStatusDTO | **200** + LocalSnapshot(`FinishedTTL` 内) | **200** no-op(**snapshot は保持**) | **204** no-op |
| `aborting` | **409** `run_transitioning` | 409 `run_active` | **201**(join 完了を待って新規) | **410** `run_aborted` | 409 `lease_not_renewable` | 200 + RunStatusDTO | 410 `run_aborted` | **200**(join 完了を待って成功) | 410 `run_aborted` |
| `aborted`(**過去 run**) | **410** `run_aborted` | ―(active でない) | ―(active でない) | 410 `run_aborted` | 410 `run_aborted` | **200** + RunStatusDTO(state=aborted, reason) | **410** `run_aborted`(snapshot は存在しない) | **200** no-op | 410 `run_aborted` |
| `expired`(**過去 run**) | **404** `unknown_run` | ―(active でない) | ―(active でない) | 404 | 404 | **200** + RunStatusDTO(state=expired) | **410** `gone`(snapshot 解放済み) | 200 no-op | 404 |

**この表が保証すること(HIGH 指摘への直接回答)**:

- run A を abort してから run B を開始しても、A の RunRecord は
  `aborted` として `TombstoneTTL`(10m)まで残る。
  **A への遅延 GET は 200 + RunStatusDTO(state=aborted)**、
  **A への遅延 Abort は 200 no-op** であり、いずれも run B に影響しない
- run A が `acknowledged` のまま run B が進行中でも、
  **A への遅延 `GET .../snapshot` は 200 + LocalSnapshot** を返す
  (`FinishedTTL` 内)。hub の再取得・再検証が常に可能
- **2 世代前の run は記録が破棄され 404**(`RetainedRuns = 2` は
  進行中の run を含むため、保持される過去 run はちょうど 1 本)。
  hub は run 終了時に必ず `sealRun` を完了させる(§seal)ので、
  2 世代前の記録を要求することはない

**v5 からの変更点(明示)**:

- v5 の「aborted への GET は 410」は**撤回**。記録がある限り 200 で
  state を返し、404(未知)と区別できるようにする
- v5 の「GET が finishing 中 202 / finished で 200 + snapshot」は**撤回**。
  状態照会は常に 200、本体取得は別 endpoint(§エンドポイント)
- v5 の「保持は直近 2 run 分」は `RetainedRuns = 2` の hard cap として
  §識別子と上限で数値化した

### テスト

- `TestPeerTransitionTable`: 上表の全セル(**9 行 × 9 列 = 81 セル**。
  「―」の 6 セルは「その行が activeRunID を持たないこと」の検証に充てる)を
  table-driven で網羅する。fake runctl Controller に state を注入する
- `TestPastRunSurvivesNewRun`: run A abort → run B start →
  A への GET/Snapshot/Abort/Ack が上表どおりで、B の state が不変であること
- `TestRunRecordEviction`: run を 3 本回すと最古の記録が 404 になること

## seal(後始末)マトリクス(v6・HIGH 対応)

**問題**: v5 は preflight / StartRun / freeze / polling / fetch / validation の
どこで失敗した場合に、**他の participant** へ Ack を送るのか Abort を送るのかを
定義していなかった。結果として required snapshot が壊れていた場合や
optional peer だけ Finish に失敗した場合に、他 peer が finished のまま
`PeerAckLease` 相当の時間だけ系を塞ぐ。

### `sealRun` — 必ず 1 回だけ実行する終端処理

```go
type SealDecision int
const (
    SealAbort SealDecision = iota // run を成立させない
    SealAck                       // run を(invalid でも)保存して閉じる
)

// hub の run controller は run_id 発番直後に defer で登録し、
// **どの return 経路でも必ず 1 回だけ**実行する(sync.Once)。
func (h *Hub) sealRun(ctx context.Context, runID string, d SealDecision) SealReport
```

participant ごとの送信内容は decision と participant の state から決定的に決まる:

```
for each participant p (hub-local #0 を含む・並列・並行度 4):
    if d == SealAbort                     → POST /peer/runs/{id}/abort   ("hub-abort")
    else if p.State == "finished"         → POST /peer/runs/{id}/ack
    else                                  → POST /peer/runs/{id}/abort   ("hub-abort")
```

- seal の送信に失敗した participant は `SealReport.Failed` に記録し、
  **1 回だけ再送**する(deadline は §deadline の Abort/Ack 行)。
  2 回失敗した participant は §lease で回収される(最大 90s)
- seal 完了後、hub は lease renewer goroutine を停止する
- テスト `TestSealAlwaysRunsOnce`: hub の全 return 経路
  (panic を含む)で `sealRun` が **ちょうど 1 回**呼ばれること

### 失敗点 × participant 種別 → 決定表

| # | 失敗点 | participant | decision | hub の run 状態(`RunState` / `Validity`) | `/reset` or `/save` の応答 | 次 run の即時開始 |
|---|---|---|---|---|---|---|
| 0 | **失敗なし**(正常完了) | — | **SealAck** | `finished` / `valid` | `/save` **200** | ○ |
| 1 | preflight(接続/protocol/schema/capability/section) | required | **SealAbort**(この時点で started の participant は 0 件なので実質 no-op) | run 未発番 → `aborted` / `invalid` | `/reset` **503** | **○**(誰も started でない) |
| 2 | preflight | optional | ―(participant から除外して継続) | `started` / `partial` | 通常応答 | ○ |
| 3 | StartRun バリア(timeout / 5xx / `StartResult.Validity=invalid`) | required | **SealAbort** | `aborted` / `invalid` | `/reset` **503** | ○(abort 成功時)/ 最大 90s(abort 失敗時は §lease) |
| 4 | StartRun バリア | optional | ―(当該 peer にのみ Abort を送って除外) | `started` / `partial` | 通常応答 | ○ |
| 5 | hub-local(#0)の StartRun 失敗 | 常に required 扱い | **SealAbort** | `aborted` / `invalid` | `/reset` **503** | ○ |
| 6 | freeze 受付(`POST .../finish` が timeout / 5xx / `Validity=invalid`) | required | **SealAck** | `finished` / `invalid` | `/save` **200**(invalid として保存) | ○ |
| 7 | freeze 受付 | optional | **SealAck** | `finished` / `partial` | `/save` 200 | ○ |
| 8 | polling(`deadline` 超過 / peer の state が `aborted`/`expired`) | required | **SealAck** | `finished` / `invalid` | `/save` 200 | ○ |
| 9 | polling | optional | **SealAck** | `finished` / `partial` | `/save` 200 | ○ |
| 10 | snapshot fetch(timeout / 5xx / `MaxSnapshotBytes` 超過) | required | **SealAck** | `finished` / `invalid` | `/save` 200 | ○ |
| 11 | snapshot fetch | optional | **SealAck** | `finished` / `partial` | `/save` 200 | ○ |
| 12 | validation(strict decode 失敗 / schema 非互換 / `run_id` 不一致 / 必須 section 欠落) | required | **SealAck** | `finished` / `invalid` | `/save` 200 | ○ |
| 13 | validation | optional | **SealAck** | `finished` / `partial` | `/save` 200 | ○ |
| 14 | hub-local の Finish / Collect 失敗 | 常に required 扱い | **SealAck** | `finished` / `invalid` | `/save` 200 | ○ |
| 15 | budget 枯渇(`MinPeerBytes` 未満) | required / optional | **SealAck** | required=`finished`/`invalid`、optional=`finished`/`partial` | `/save` 200 | ○ |

**決定規則(1 行で言える形)**:

> **開始側(preflight / StartRun)の required 失敗は `SealAbort`。
> 終了側(freeze 以降)の失敗は種別を問わず `SealAck`。**

これは 02 の原則
「開始側の required 失敗は run を**成立させない**、終了側の required 失敗は
run を **invalid として保存する**」を分散に写したものであり、独自規則ではない。
終了側で `SealAbort` を選ばないのは、peer 側の snapshot が破棄されて
調査に使えなくなるためである。

- **#2 / #4(optional の開始側失敗)は run を終わらせない**。当該 peer を
  participant から外して計測を続け、`sealRun` は run の末尾で #0 または
  #6〜#15 のいずれかとして 1 回だけ発火する
- 「次 run の即時開始 ○」は **`sealRun` の送信が成功した場合**の値である。
  seal 送信が 2 回とも失敗した participant は §lease で回収されるため、
  最悪でも `PeerMaxBlockingWindow`(90s)後には次 run を受理する。
  **どの行でも「TTL まで停止」は起こらない**
- 失敗した participant は hub の `PeerResult.Failure`
  (`ParticipantFailureDTO`)に phase / code / detail / at を記録する。
  **これが optional participant の失敗を hub 側に残す唯一の経路**である
- `Validity` は 02 と同じく**単調に悪化するのみ**(valid → partial → invalid)。
  複数失敗があれば最も悪い値になる
- テスト `TestSealMatrix`: 上表 16 行を table-driven で網羅。
  fault injection で各失敗点を再現し、**全 participant に届いたメッセージ種別**、
  hub の `RunState`/`Validity`、および**直後の StartRun が 201 になること**を検証する
- テスト `TestSealSendFailure_RecoveredByLease`: seal 送信を全滅させ、
  注入 clock を `PeerMaxBlockingWindow` + 1s 進めると次 run が 201 になること

## wire DTO(v6・MEDIUM 対応 — 項目単位で確定)

### 共通規約

- `Content-Type: application/json; charset=utf-8` を要求(不一致は **415**)
- **strict decode**:
  ```go
  dec := json.NewDecoder(io.LimitReader(r.Body, MaxRequestBytes))
  dec.DisallowUnknownFields()
  if err := dec.Decode(&req); err != nil { return badRequest("malformed_request") }
  if dec.More() { return badRequest("trailing_data") }
  ```
  未知フィールド・後続データ・型不一致はすべて **400 `malformed_request`**
- 必須フィールドの存在検査は zero value に頼らず、**ポインタ受け + 明示検査**で行う
- 時刻はすべて RFC3339Nano(UTC)。**hub は peer の時刻を比較に使わない**
  (§participant モデル)
- 02 が JSON tag を定義済みの型(`CollectorBoundary` / `BoundaryWindow` /
  `RunStatus`)は**そのまま埋め込む**。10 で再定義しない

### `ErrorDTO`(全 4xx/5xx 共通)

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `code` | string | ✔ | 安定コード(下記の閉じた集合のみ) |
| `message` | string | | 人間向け。機械判定に使わない |
| `run_id` | string | | 対象 run(あれば) |
| `active_run_id` | string | | `code=="run_active"` のとき、閉塞している run |
| `active_state` | string | | 同上。02 の `RunState` 値 |
| `lease_expires_in_ms` | int64 | | 同上。**hub が何秒待てばよいか**を示す |

`code` の閉じた集合: `unauthorized` / `malformed_request` / `trailing_data` /
`invalid_run_id` / `invalid_nonce` / `invalid_role` / `nonce_mismatch` /
`run_active` / `run_transitioning` / `run_aborted` / `unknown_run` /
`not_finished` / `gone` / `lease_not_renewable` / `payload_too_large` /
`protocol_mismatch` / `internal`。

### `PeerInfoDTO`(`GET /peer/info`)

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `protocol_version` | int | ✔ | 本計画は 1。完全一致要件 |
| `schema_version` | int | ✔ | LocalSnapshot の schema。hub ≥ peer 要件 |
| `library_version` | string | ✔ | module version。不一致は許容(記録のみ) |
| `agent_id` | string | ✔ | UUID v4。peer の識別子 |
| `form` | string | ✔ | `"embedded"` \| `"agent"` |
| `role` | string | ✔ | 03 の `Identity.Role`(自由記述。`app`/`db`/`dns`/`proxy` を推奨)。**文字種と長さのみ強制**する(§識別子と上限) |
| `sections` | []string | ✔ | 提供可能セクション(登録 collector から導出) |
| `capabilities` | []string | ✔ | README 共通契約 4 の capability 名 |
| `identity` | 03 の `Identity` | ✔ | machine_id_hash / boot_id_hash / 各 ns / cgroup_ns |
| `cgroup_scope` | string | ✔ | 03 の 4 値。dedup の代表値選定に使う |
| `targets` | []`TargetSummaryDTO` | | `{id, driver, display, schema, purposes}`(01 の `TargetInfo`。**DSN は含めない**) |
| `active_run_id` | string | | 現在閉塞している run(あれば) |
| `started_at` | time | ✔ | peer プロセスの起動時刻 |

### `StartRunRequest`(`POST /peer/runs`)

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `run_id` | string | ✔ | hub が発番。§識別子と上限の規則 |
| `nonce` | string | ✔ | 同上。02 の nonce 冪等キャッシュへ渡す |
| `preempt` | bool | ✔ | 02 の `StartRunOptions.Preempt` に直結 |
| `trigger` | string | | 02 の `StartRunOptions.Trigger`(記録用) |
| `lease_ms` | int64 | | hub が要求する started lease 長。peer は `min(要求, PeerStartedLeaseMax = 90s)` を採用する。省略時は `PeerStartedLease`(45s)。採用値は `StartResultDTO.lease_ms` で返す |

### `StartResultDTO`(201 / 200)

02 の `StartResult` の写像。

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `run_id` | string | ✔ | wire の run_id |
| `local_run_id` | string | ✔ | peer 内の runctl runID(参考) |
| `nonce` | string | ✔ | |
| `epoch` | uint64 | ✔ | 02 の `Epoch` |
| `state` | string | ✔ | 02 の `RunState`(通常 `"started"`) |
| `validity` | string | ✔ | 02 の `Validity`(`valid`/`partial`/`invalid`) |
| `collectors` | []`runctl.CollectorBoundary` | ✔ | 02 の型をそのまま。name/kind/required/phase/at/committed/code/err/dropped |
| `generation_window` | `runctl.BoundaryWindow` | ✔ | min/max/spread |
| `boundary_window` | `runctl.BoundaryWindow` | ✔ | 同上 |
| `preempted_run_id` | string | | preempt した場合のみ |
| `lease_expires_at` | time | ✔ | 採用された started lease の満了時刻(peer の時計) |
| `lease_ms` | int64 | ✔ | 採用された lease 長 |
| `started_at` | time | ✔ | |

### `FinishRunRequest` / `FinishAcceptedDTO`(`POST .../finish`)

request は `{}`(フィールド無し。strict decode の対象)。

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `run_id` | string | ✔ | |
| `epoch` | uint64 | ✔ | |
| `state` | string | ✔ | 通常 `"finishing"` |
| `validity` | string | ✔ | |
| `collectors` | []`runctl.CollectorBoundary` | ✔ | frozenAt / final SampledAt を含む |
| `generation_window` | `runctl.BoundaryWindow` | ✔ | |
| `boundary_window` | `runctl.BoundaryWindow` | ✔ | |
| `finish_lease_expires_at` | time | ✔ | 02 の `FinishLease` 満了時刻。**hub の polling deadline の根拠** |
| `accepted_at` | time | ✔ | |

### `RunStatusDTO`(`GET /peer/runs/{id}` — 常に 200)

02 の `RunStatus` を埋め込み、10 が polling に必要な項目を additive に足す。

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `run_id` / `epoch` / `state` / `validity` / `reason` / `acked_by` / `detached` / `since` | 02 の `RunStatus` | ✔ | **02 の JSON tag をそのまま使う** |
| `local_run_id` | string | ✔ | |
| `origin` | string | ✔ | `"peer"` \| `"local"` |
| `lease_expires_at` | time | | started / finished lease の満了時刻 |
| `expiry_reason` | string | | `"started-lease-expired"` \| `"ack-lease"` |
| `snapshot_ready` | bool | ✔ | `state ∈ {finished, acknowledged}` かつ snapshot 保持中 |
| `snapshot_bytes` | int64 | | 縮小前の実測直列化サイズ(hub の budget 計算の入力) |
| `issues` | []`SectionIssueDTO` | | **degraded の報告経路**(下記) |

```go
type SectionIssueDTO struct {
    Section string `json:"section"`          // "sqlstats" / "hoststats" / ...
    Code    string `json:"code"`             // 02 の CollectorBoundary.Code の値
                                             // + 10 独自: "size-shrunk" / "size-dropped" /
                                             //   "not-supported-by-form" / "target-unregistered"
    Detail  string `json:"detail,omitempty"`
}
```

### `AbortRequest` / `AbortResultDTO`(`POST .../abort` — 冪等)

request: `{"reason": "<= 128 bytes printable ASCII>"}`(省略可)。

- peer は **02 の `AbortRun(LocalRunID, "hub-abort")` を呼ぶ**。
  02 の `AbortResult.Reason` は閉じた集合であり、hub 起因は `"hub-abort"` 一択なので、
  request の `reason` を runctl へ渡すことはしない
- request の `reason` は peer の `RunRecord` と peer 側ログにのみ記録する
  (診断用の自由記述)。wire 上の細分は下表の `peer_reason` が担う

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `run_id` | string | ✔ | |
| `epoch` | uint64 | ✔ | |
| `state` | string | ✔ | 02 の `RunState`(通常 `"aborted"`) |
| `reason` | string | ✔ | 02 の `AbortResult.Reason` をそのまま |
| `peer_reason` | string | ✔ | wire 側の細分: `"explicit"` \| `"started-lease-expired"` \| `"finish-lease-expired"` \| `"preempted"` \| `"no-op"` |
| `detached` | bool | ✔ | 02 の join timeout |
| `partial` | []string | | 02 の `AbortResult.Partial`(部分開始 collector 名) |
| `snapshot_discarded` | bool | ✔ | `finished` からの abort で snapshot を捨てたか |
| `aborted_at` | time | ✔ | |

### `LeaseDTO`(`POST .../lease` — 200)

lease 更新は状態を変えないが、hub が**更新後の満了時刻**を知る必要があるため
204 ではなく **200 + LeaseDTO** を返す。

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `run_id` | string | ✔ | |
| `state` | string | ✔ | |
| `lease_expires_at` | time | ✔ | 更新後の満了時刻 |
| `lease_ms` | int64 | ✔ | |

### `LocalSnapshot`(`GET .../snapshot?max_bytes=N`)

`Peers` / `Prev` を**含まない**自ホスト分のみの DTO(v3 から維持。再帰なし)。

| フィールド | 型 | 必須 | 内容 |
|---|---|---|---|
| `schema_version` | int | ✔ | |
| `run_id` / `local_run_id` / `epoch` | | ✔ | 結合キー。**hub は `run_id` 不一致を validation 失敗として扱う** |
| `validity` | string | ✔ | |
| `meta` | object | ✔ | capabilities / boundary window / spread / identity / cgroup_scope |
| `sections` | object | ✔ | セクション名 → 各 collector の区間値 |
| `issues` | []`SectionIssueDTO` | | 縮小・除外・失敗の記録 |
| `budget` | `SnapshotBudgetDTO` | ✔ | `{max_bytes, encoded_bytes, shrunk_sections[], dropped_sections[]}` |

### hub 側 `PeerResult`(hub の Snapshot に載る。schema v4)

```go
type PeerResult struct {
    Info          PeerInfoDTO             `json:"info"`
    Required      bool                    `json:"required"`            // v6 追加
    Form          string                  `json:"form"`                // v6 追加("embedded"|"agent")
    Start         *StartResultDTO         `json:"start,omitempty"`
    Finish        *FinishAcceptedDTO      `json:"finish,omitempty"`
    Status        *RunStatusDTO           `json:"status,omitempty"`    // v6 追加(最後に見た state)
    Aborted       *AbortResultDTO         `json:"aborted,omitempty"`
    Failure       *ParticipantFailureDTO  `json:"failure,omitempty"`   // v6 追加
    Sealed        string                  `json:"sealed"`              // v6 追加("ack"|"abort"|"failed")
    StartSendAck  [2]time.Time            `json:"start_send_ack"`      // hub 観測の不確実性区間(開始)
    FinishSendAck [2]time.Time            `json:"finish_send_ack"`     // 同(終了)。単一配列に潰さない
    Issues        []SectionIssueDTO       `json:"issues,omitempty"`
    Local         *LocalSnapshot          `json:"local,omitempty"`
}

type ParticipantFailureDTO struct {
    Phase  string    `json:"phase"`  // "preflight"|"start"|"finish"|"poll"|"fetch"|"validate"|"seal"
    Code   string    `json:"code"`   // "unreachable"|"protocol-mismatch"|"schema-incompatible"|
                                     // "capability-missing"|"section-missing"|"timeout"|"http-status"|
                                     // "malformed"|"run-id-mismatch"|"size-exceeded"|
                                     // "aborted-by-peer"|"budget-exhausted"
    Detail string    `json:"detail,omitempty"`
    At     time.Time `json:"at"`
}
```

### schema 互換テスト(必須)

- **`TestWireDTOGolden`**: 全 DTO の代表値を `testdata/wire/v1/*.json` に
  golden として固定し、marshal 結果の完全一致を検証する。
  フィールドの追加・削除・リネーム・tag 変更で**必ず落ちる**
- **`TestWireDTOStrictDecode`**: 各 golden に未知フィールドを 1 つ足した
  変種を decode し、**すべて 400 `malformed_request`** になること。
  末尾に余分な JSON 値を付けた変種は `trailing_data`
- **`TestWireDTOFieldSet`**: `reflect` で全 DTO のフィールド名 + JSON tag +
  型を列挙した一覧を `testdata/wire/fieldset.txt` に固定する。
  差分が出たら fail し、`protocol_version` の bump 要否
  (README 共通契約 4: additive なら bump 不要、意味変更・削除・型変更なら bump)を
  レビューで明示させる
- **`TestWireDTOMirrors02`**: `StartResultDTO` / `FinishAcceptedDTO` /
  `RunStatusDTO` / `AbortResultDTO` のフィールドが 02 の
  `StartResult` / `FinishAccepted` / `RunStatus` / `AbortResult` の
  公開フィールドを**すべて含む**ことを reflect で検証する
  (02 に項目が増えたときに 10 が取りこぼさないため)
- **`TestErrorCodeSet`**: `ErrorDTO.code` が上記の閉じた集合のみを取ること

## 識別子と上限(v6・MEDIUM 対応)

### 識別子の長さと文字種

比較は **byte 単位の完全一致**(01 の TargetID と同じ規則。大小同一視・
Unicode 正規化・前後空白の trim を一切行わない)。

| 識別子 | 長さ | 文字種 | 違反時 |
|---|---|---|---|
| `run_id` | 1〜64 バイト | `[A-Za-z0-9._-]`(ASCII のみ) | **400 `invalid_run_id`** |
| `nonce` | 1〜64 バイト | 同上 | **400 `invalid_nonce`** |
| `role` | 1〜32 バイト | `[a-z0-9-]`(値の集合は制限しない — 03 が自由記述と規定) | **400 `invalid_role`**(agent は起動失敗) |
| `agent_id` | 36 バイト固定 | UUID v4 canonical(`[0-9a-f-]`) | 起動失敗 |
| `target id` | 01 の規則(1〜64 バイト・`[A-Za-z0-9._-]`) | 01 に従う | 01 の `ErrInvalidTargetID` |
| セクション名 | 1〜32 バイト | `[a-z0-9_-]` | 起動失敗(定数のため) |
| abort `reason` | 0〜128 バイト | printable ASCII(0x20〜0x7E) | 400 `malformed_request` |
| `trigger` | 0〜64 バイト | printable ASCII | 400 `malformed_request` |

hub が発番する `run_id` の推奨形式: `r-` + base32(RFC 4648・小文字・
padding なし, 10 バイトの crypto/rand)= 18 バイト。
`nonce` も同形式で 26 文字(16 バイト)。

### hard cap(TTL だけでは有界にならないためのメモリ上限)

| 上限 | 値 | 出所 | 超過時 |
|---|---|---|---|
| `RetainedRuns` | **2**(進行中を含む) | 02 と同値 | 最古の**非 active** record を TTL を待たず即破棄 |
| `NonceHistoryMax` | **64** | 02 と同値 | LRU で最古を破棄(`NonceTTL` 10m と併用) |
| `MaxPeers` | **8** | v2 から維持 | 設定エラー(起動失敗) |
| `MaxRequestBytes` | **64KiB** | 本計画 | **413 `payload_too_large`** |
| `MaxHeaderBytes` | **8KiB** | 本計画 | 431 |
| `PeerSelfCapBytes` | **8MiB** | 本計画 | peer が build 時に top-N 縮小し `issues` に `size-shrunk` を記録 |
| `MaxSnapshotBytes`(hub の受信上限) | `max_bytes` + 1MiB | 本計画 | fetch を打ち切り validation 失敗(seal #10) |

**peer あたりのメモリ上限(算出式・テストで固定)**:

```
peer_mem  <=  RetainedRuns(2) * PeerSelfCapBytes(8MiB)
            + NonceHistoryMax(64) * (nonce 64B + run_id 64B + StartResult 実測上限 8KiB)
            + 定数
          <=  16MiB + 512KiB + α  ≈ 17MiB
```

- **abort/start の高速反復下でも上限は変わらない**。`RetainedRuns = 2` の
  即時 eviction と `NonceHistoryMax = 64` の LRU が TTL とは独立に効くため
- テスト `TestPeerMemoryBounded`: 1000 回の start→abort を回して
  `len(runs) <= 2` かつ `len(nonces) <= 64` であること(注入 clock で
  TTL を一切進めずに検証する = TTL に依存していないことの証明)

## budget(v6・MEDIUM 対応 — wire 化と hub overflow)

### 定数

| 定数 | 値 | 意味 |
|---|---|---|
| `TotalSnapshotCap` | **32MiB** | hub が保存する snapshot 全体の上限 |
| `HubSelfReserve` | **16MiB** | hub 自身の snapshot の予約枠 |
| `PerPeerDefaultBytes` | **4MiB** | peer 1 件あたりの既定上限 |
| `MinPeerBytes` | **512KiB** | これを下回る予算しか出せない場合の判定閾値 |
| `BudgetReserve` | **4KiB** | 直列化見積もり誤差の余裕 |

### `max_bytes` を fetch request に載せる(採用案)

「hub が受信後に縮小する」案は**採らない**。受信後縮小では
`TotalSnapshotCap` を超える転送が実際に発生し、SSH トンネル越しの
転送時間と hub のピークメモリを抑えられないためである。

```
GET /peer/runs/{run_id}/snapshot?max_bytes=4194304
```

- peer は**保存済みの immutable snapshot は変更せず**、
  **直列化時にのみ top-N 縮小**を適用して `max_bytes - BudgetReserve` を狙う
- 直列化結果が `max_bytes` を超えた場合は縮小段階を 1 つ進めて**再直列化**する
  (最大 3 回)。3 回で収まらない場合は `sections` を空にした
  `LocalSnapshot`(meta + validity + issues + budget のみ)を返す
- 応答の `budget` に `{max_bytes, encoded_bytes, shrunk_sections, dropped_sections}`
  を必ず載せる。hub はこれを `PeerResult.Issues` に写す
- `max_bytes` 省略時は `PeerSelfCapBytes`(8MiB)を上限とする

### 順序(hub の budget 決定が可能な理由)

1. Finish バリア(全 participant へ並列)
2. hub-local の snapshot 構築(02 の `SnapshotBuildBudget` 5s)と
   peer の polling(`GET /peer/runs/{id}`)は**並行に進める**。
   polling は本体を転送しないため、この時点で hub の実測サイズが確定していなくてよい
3. hub-local の snapshot が確定したら **実測直列化サイズ `hubBytes`** を得る
4. per-peer 予算を決める:
   ```
   remaining = TotalSnapshotCap - hubBytes
   perPeer   = min(PerPeerDefaultBytes, remaining / len(peers))
   ```
   `RunStatusDTO.snapshot_bytes`(縮小前サイズ)が分かっているので、
   小さい peer の余りを大きい peer へ回す **1 パスの再配分**を行う
   (小さい順に確定 → 残りを未確定 peer で等分)
5. `GET .../snapshot?max_bytes=<perPeer>` を並行度 4 で fetch

### hub 自身が `HubSelfReserve` を超えた場合の優先規則(v6 追加)

1. **まず hub 自身に top-N 縮小を適用**して 16MiB 以内に収める
   (peer の取り分を先に削らない)。縮小の事実は hub の `issues` に
   `size-shrunk` として記録するが、**run は valid のまま**
2. top-N 縮小(下限: 各表 20 行)でも 16MiB を超える場合、
   **hub のセクション全欠落は行わず** hub の snapshot をそのまま採用し、
   `perPeer = (TotalSnapshotCap - hubBytes) / len(peers)` を再計算する
3. `perPeer < MinPeerBytes`(512KiB)になった場合(seal マトリクス #15):
   - **required peer**: run を **invalid**(health `multihost-budget-exhausted`)。
     ただし **snapshot は保存する**(02 の「終了側 required 失敗は invalid として
     保存」と整合)。seal decision は `SealAck`
   - **optional peer**: 取り込まず `dropped_sections` に記録して **partial**
4. `TotalSnapshotCap` は**決して超えない**。保存直前に実測し、超過する場合は
   **最も大きい optional peer から順に丸ごと落とす**(それでも超える場合は
   required peer を落として invalid にする)

### 縮小の 2 段階の区別(v4 修正を維持)

- **行数の top-N 縮小**(SQL/HTTP 表の下位行を削る): 許容。
  `issues` に `size-shrunk` を記録するのみで run 状態は不変
- **セクション全欠落**: required peer では**最低でも partial**。
  hub が必須と宣言した capability / section に対応するセクションの欠落は **invalid**
- top-N 縮小でもなお per-peer 上限を超える required peer は invalid

### テスト

- `TestBudgetOrdering`: polling と hub-local build が並行に進み、
  fetch が hub 実測サイズ確定後であること
- `TestHubOverflowPriority`: hub 20MiB / peer 4 件のとき、
  上記 1→2→3 の順で判定され、`TotalSnapshotCap` を超えないこと
- `TestMaxBytesHonored`: `max_bytes` を段階的に下げても
  `encoded_bytes <= max_bytes` が常に成り立つこと(3 回の再直列化を含む)

## agent と配布

- `cmd/isutools-agent`: 既存パッケージの配線のみ(§deployment(b))。
  複数 DB target は **JSON ファイル**で宣言する(v4 修正 — DSN には
  driver が含まれず `name=dsn` 形式では driver を特定できないため):

  ```bash
  export ISUTOOLS_AGENT_TARGETS_FILE=/etc/isutools/targets.json
  ```
  ```json
  [
    {"id": "shard1", "driver": "mysql", "dsn": "user:pass@tcp(127.0.0.1:3306)/isuconp"},
    {"id": "shard2", "driver": "mysql", "dsn": "user:pass@tcp(127.0.0.1:3307)/isuconp"}
  ]
  ```

  - 各エントリは 01 の `RegisterDBTarget(id, driver, dsn)` と等価
    (= `PurposeApp`)。`driver` は必須項目。`id` は 01 の TargetID と同一の
    名前空間・同一の文字種規則
  - EXPLAIN 用の最小権限 credential は `explain_dsn` / `explain_driver` を
    同エントリに書き、01 の
    `RegisterDBInspector(id, PurposeExplain, driver, dsn)` に写す
  - **ファイルの安全性契約(v6 修正)**: regular file であること・
    agent 実行ユーザー所有・**`mode & 077 == 0`**(group/other に
    いかなる権限も無いこと。0600 と 0400 の双方を受理する。
    v5 の「mode 0600 固定」は撤回)・サイズ上限 64KiB を起動時に検証し、
    symlink は拒否する
  - **DSN がログ・`PeerInfoDTO`・`LocalSnapshot`・health のいずれにも
    出力されないことをテストで固定する**(表示は 01 の allowlist
    `Display` と非 secret の `Schema` のみ)
- 配布: **リリース tag 固定の事前 build 済み単一 binary + SHA-256
  checksum** を GitHub Releases に添付(make target で linux/amd64,
  arm64 を cross-build)。`go run @latest` は例示にも使わない。
  **v5 の「hub と agent は同一 binary バージョンを配布する」は撤回**し、
  §deployment のバージョン互換要件(protocol_version 完全一致 /
  schema_version hub ≥ peer / library_version 不問)に置き換える。
  embedded peer はアプリの go.mod が指す isutools のバージョンで決まり、
  binary の同一性という概念が成立しない
- transport(v2 から維持): bind は loopback 限定。peer 指定は
  literal loopback IP のみ(SSH トンネル強制)。redirect 禁止・
  Proxy 無効の専用 `http.Transport`・header/body/展開後サイズ上限・
  並行度 4・`MaxPeers = 8`・重複 endpoint 拒否・
  `Authorization: Bearer` の定数時間比較。
  deadline は §deadline の表を正とする

## E2E マトリクス

- **構成**: bare metal/systemd、Docker host namespace、Docker 別 namespace
  (別 namespace では mysqld・物理 NIC が見えないことを確認し、
  03 の identity で判別できることを検証)
- **形態**(v6 必須):
  - embedded `ServePeer` を組み込んだアプリ peer が
    `httpstats`/`sqlstats`/`counters`/`connections`/`dbpool` を含む
    `sections` を返し、実際に snapshot に載ること
  - standalone agent peer が上記 5 セクションを `sections` に含まず、
    hub が `RequiredSections` にそれらを要求すると
    **preflight 失敗 → 503 + invalid**(seal マトリクス #1)になること
- **トポロジ**: app+DB / app×2+DB / app+DB×4(shard)
- **障害**(v6 必須ケースを太字):
  - peer 再起動、SSH トンネル切断、fetch timeout、version skew、
    reset 中の peer 障害、finish 中の peer 障害と復旧
  - 部分開始 → `sealRun(SealAbort)` 伝播 → 次 run 成功(v5 から維持)
  - **abort 消失 → `PeerStartedLease` 満了 → 次 run 成功**
    (`HubPreemptPolicy="never"` + 注入 clock)
  - **hub 停止 → `PeerAckLease` 満了で自己 Ack → 次 run 成功、
    かつ旧 run の snapshot が `FinishedTTL` 内で取得できる**
  - **abort 消失 → preempt → 次 run が即時成功**
  - **optional peer だけ Finish に失敗 → 他 participant に Ack が届き、
    hub の run は `finished`/`partial`、次 run が即開始できる**(seal #7)
  - **required peer の snapshot が strict decode に失敗 → 全 participant に
    Ack、hub の run は `finished`/`invalid`、次 run が即開始できる**(seal #12)
  - **run A abort 後に run B を開始し、A への遅延 GET/Snapshot/Abort/Ack が
    遷移表どおりで B に影響しないこと**
  - **未知フィールド付き DTO を送ると 400 `malformed_request`**
  - duplicate `agent_id`、`MaxPeers` 超過、`TotalSnapshotCap` 到達、
    **hub 自身が `HubSelfReserve` 超過**
- **検証**: 同一 run の全 peer 区間表示(境界時刻 + uncertainty +
  02 の `BoundaryWindow.Spread`)、invalid run で bench が開始されないこと、
  **fetch 遅延が計測区間に影響しないこと**(FinishRun 後に故意に
  遅延させて取得し、snapshot の値が変わらないこと)

## 実装ステップ

1. **ADR 作成・承認**(02 lifecycle の wire 写像、遷移表、seal マトリクス、
   lease、2 つの deployment 形態、`LocalSnapshot`/schema v4、budget 決定則、
   DTO 一式、配布方式)— **2.5 日**
2. Phase A: wire DTO 定義 + strict decode + golden/fieldset テスト +
   識別子検証 + 上限 — **3 日**
3. Phase B: peer 側 `peerStore`(`activeRunID` + `map[RunID]RunRecord`)+
   遷移表の table-driven テスト + 02 Controller への写像 — **3.5 日**
4. Phase C: embedded `PeerHandler` / `ServePeer` + `cmd/isutools-agent` +
   `sections` 導出 + handshake/preflight — **4 日**
5. Phase D: lease(started lease・renew endpoint・ack lease・watchdog・
   注入 clock)+ 3 つの lease E2E — **2.5 日**
6. Phase E: hub の StartRun/Finish バリア + polling + fetch + `sealRun` +
   seal マトリクス 16 行の fault injection テスト — **5 日**
7. Phase F: budget の wire 化(`max_bytes`・再直列化)+ hub overflow 優先規則
   + 再配分 — **2 日**
8. E2E マトリクス + 配布(cross-build/checksum)+ docs
   (INTEGRATION.md の「embedded peer の配線」「agent の scp 手順」
   「targets.json の権限」)— **4 日**

## 見積もり

**26.5 日**(v5 の 17 日 + 下表の 9.5 日)。ADR は 02 lifecycle の完全な写像・
seal マトリクス・lease の有界性証明を要件に含めた上でレビューに回す。

| 追加項目(第5回レビュー由来) | 増分 |
|---|---|
| lease 機構(started lease / renew endpoint / ack lease / watchdog / 注入 clock)+ 3 つの lease E2E | +1.5 日 |
| `activeRunID` + `map[RunID]RunRecord` + wire↔local runID 対応 + 遷移表 81 セルの table-driven 化 | +1.5 日 |
| `sealRun` と失敗点マトリクス 16 行 + fault injection テスト | +1.5 日 |
| embedded `PeerHandler` / `ServePeer`(token 認証・loopback 強制)+ `sections` 導出 + 形態別 E2E | +2.0 日 |
| wire DTO の項目単位定義 + strict decode + golden / fieldset / 02 ミラーの互換テスト | +1.5 日 |
| 識別子検証(長さ・文字種)+ hard cap + `TestPeerMemoryBounded` | +0.5 日 |
| `max_bytes` の wire 化(再直列化ループ)+ hub overflow 優先規則 + 予算再配分 | +1.0 日 |
| **合計増分** | **+9.5 日** |

**README との整合(v6 監査反映)**: 旧 v6 の
「**plans/README.md の再算定が必要**」という但し書きは**撤回**する。
README v6 §リリース対応が全計画を再算定済みで、`v1.5.0以降: 10 = 26.5 日 →
+30% で 34.5 日` と記載されている。本ファイルの raw 見積もり **26.5 日**は
README の表と**一致**しており、不一致は無い(README が 34.5 日、本ファイルが
`26.5 × 1.3 = 34.45` を「≈34 日」と表記している差は、README §リリース対応が
明記しているとおり **0.5 日単位への丸めのみ**で、数値の矛盾ではない)。

## リスク

| リスク | 対策 |
|---|---|
| バリア 2 回分の運用複雑化 | `bench.sh` 例を提供(reset→bench→save だけは従来どおり。バリアは hub 内部) |
| peer の immutable 保存領域 | `RetainedRuns = 2` × `PeerSelfCapBytes` 8MiB + nonce 64 件 ≈ 17MiB の算出式を `TestPeerMemoryBounded` で固定 |
| lease 満了で正常な長時間 run が中断される | `PeerStartedLease` 45s は hub が 10s ごとに更新する。更新は 3 連続失敗まで許容。`Origin == "local"` の run は lease 対象外 |
| lease の時計依存 | `clock` を注入可能にし、E2E も注入 clock で実施。**ホスト間の時計は比較しない**(lease は各 peer のローカル単調時計のみで判定) |
| embedded peer が公開ポートに露出 | `ServePeer` の loopback 強制 + Bearer token 必須 + 公開 mux への mount を非サポートと明記 |
| namespace 不可視 | E2E で確定し 03 の identity 表示で判別可能にする |
| `TotalSnapshotCap` との衝突 | budget 決定則(hub 優先 → hub 縮小 → 再配分 → required invalid / optional partial) |
| DTO 変更で peer と hub が食い違う | `TestWireDTOFieldSet` / `TestWireDTOMirrors02` / golden で機械検出し、`protocol_version` bump の要否をレビューで明示 |
| seal 送信が全部失敗して系が止まる | seal は 1 回再送し、最終的に §lease が 90s で回収する。`PeerMaxBlockingWindow` を定数テストで固定 |

## 02 / 01 から引用する契約(独自定義しない項目)

| 項目 | 出所 | 本ファイルでの扱い |
|---|---|---|
| `RunState` の 9 値(idle/starting/started/finishing/finished/acknowledged/aborting/aborted/expired) | 02 §用語と基本型 | wire の `state` にそのまま使う |
| `Validity` の 3 値(valid/partial/invalid) | 02 | wire の `validity` |
| `Epoch` | 02 | wire の `epoch` |
| `StartResult` / `FinishAccepted` / `AbortResult` / `RunStatus` / `CollectorBoundary` / `BoundaryWindow` | 02 §API | DTO に埋め込む(JSON tag も 02 のもの) |
| `StartRunOptions{Nonce, Preempt, Reason:"hub", Trigger}` | 02 §API | `StartRunRequest` の写像先 |
| `AbortResult.Reason = "hub-abort"` | 02(10 用として定義済み) | hub 起因の abort は常にこの値 |
| sentinel error → HTTP(`ErrRunActive`/`ErrRunTransitioning`→409、`ErrRunAborted`→410、`ErrUnknownRun`→404) | 02 §API の「10 への写像」 | 遷移表の status |
| `StartRunBudget` 6s / `FinishSyncBudget` 6s / `FinishLease` 20s / `AbortJoinBudget` 2s / `DrainBudget` 10s / `SnapshotBuildBudget` 5s | 02 §予算モデル | hub deadline の下限計算に引用 |
| hub deadline 下限表(8s/12s、8s/12s、25s/40s、4s/8s) | 02 §下流計画への指示 | §deadline にそのまま採用 |
| `FinishedTTL` 10m / `TombstoneTTL` 10m / `StartedTTL` 30m / `NonceTTL` 10m / `NonceHistoryMax` 64 / `RetainedRuns` 2 | 02 §lease / TTL 一覧 | 引用。10 が追加するのは `PeerStartedLease` / `PeerStartedLeaseMax` / `PeerLeaseRenewInterval` / `PeerAckLease` / `PeerMaxBlockingWindow` の 5 つだけ |
| `Preempt` の意味論(fence→cancel→join→新規開始、finished は snapshot 保持) | 02 §preempt | `StartRunRequest.preempt` の写像先 |
| TargetID の文字種・長さ・byte 完全一致 | 01 §明示 ID の制約 | `run_id` / `nonce` / target id に同じ規則を適用 |
| `RegisterDBTarget(id, driverName, dsn)` / `RegisterDBInspector(id, purpose, driver, dsn)` / `Inspect(ctx, id, purpose, fn)` / `TargetInfo{ID,Driver,Display,Schema,Purposes}` | 01 | targets.json の写像先・`PeerInfoDTO.targets` |
| `Identity{Hostname, MachineIDHash, BootIDHash, PIDNS, NetNS, MntNS, CgroupNS, Role, AgentVersion}` / `cgroup_scope` の 4 値 | 03 | `PeerInfoDTO.identity` / dedup キー |
| schema version 契約(additive は bump しない、capabilities で宣言) | README 共通契約 4 | `TestWireDTOFieldSet` の判定基準 |
