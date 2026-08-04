# 10: 複数台横断計測 — v3(ADR 前提)

種別: アーキテクチャ変更 / 対象リリース: v1.4.0 以降 /
依存: 01(TargetID)、02(二段階境界 + run 契約)、03(hoststats/identity)、05(network)
規模: **2〜3 週間**

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

1. アプリ非搭載ホスト(DB/DNS/proxy)で hoststats + network + advisor +
   sqlrows を計測できる agent
2. hub が全ホストの **同一 run** の snapshot をホスト別に並べて表示
   (app peer は SQL/HTTP/accesslog/connections/counters を全て含む。
   合算はしない)
3. run の開始と終了が全 required peer で揃っていることを**証拠付き**で
   保証し、揃わない run は invalid として保存する

## run プロトコル(protocol_version = 1)

```
GET  /peer/info                     handshake(識別・互換性・capability)
POST /peer/runs                     ResetRun {run_id, nonce}    → ResetResult
POST /peer/runs/{run_id}/finish     FinishRun                   → FinishResult
GET  /peer/runs/{run_id}            immutable run snapshot 取得(LocalSnapshot)
```

### handshake(v2 から維持 + 判定変更)

`PeerInfo{schema_version, protocol_version, capabilities, identity(03),
agent_version, started_at}`。**required peer** は次のいずれかで
**preflight 失敗 = run を開始せず invalid**:
接続不可 / protocol_version 不一致 / schema_version 非互換 /
hub が要求する capability の欠如。optional peer は partial 記録で継続。
重複 identity(machine_id+boot_id+NetNS 一致)は設定エラー。

### ResetRun(開始バリア)

1. hub の Controller が run_id を発番(02)し、preflight(handshake 再検証)
2. 全 required peer へ `POST /peer/runs` を並列送信。peer は 02 の
   `Reset(WaitDrain:true)` を実行し、自身の ResetResult を ACK として返す
3. **全 required ACK が揃ってから** hub の `POST /reset` が応答する
   (bench 開始 = 全ホスト境界確定後)。1 つでも失敗 → **503 + invalid**
   (bench を開始させない)。optional の失敗は partial
4. hub は peer ごとに送信時刻と ACK 受信時刻を記録し、
   **boundary uncertainty = [送信, ACK] 区間**として snapshot に保存する
   (ホスト間の時計は比較しない。NTP 同期は前提にしない)

### FinishRun(終了バリア — v3 新設)

1. hub の `POST /save`(または `POST /collect`)が起点。まず hub 自身の
   collector を freeze(02 の BeginBoundary で世代を閉じ、accesslog を
   flush → snapshot を確定)
2. 全 peer へ `POST /peer/runs/{run_id}/finish` を並列送信。peer は
   accesslog collect の flush → 全 collector の境界確定 →
   **LocalSnapshot を run_id 付きで immutable に保存**し、FinishResult
   (各 collector の境界時刻)を ACK として返す
3. required peer の finish 失敗 → run を invalid で保存(データは
   得られた範囲で保持し、欠落 peer と理由を記録)。optional → partial
4. hub は ACK 後に `GET /peer/runs/{run_id}` で取得する。応答内の
   run_id を検証し、不一致は protocol error として invalid
5. 各 peer の計測区間は「その peer の ResetResult / FinishResult の
   境界時刻」で表示する(fetch 遅延はもはや区間に影響しない)

## wire DTO と容量 budget

```go
// LocalSnapshot: peer が保存・返却する自ホスト分のみの DTO。
// Peers / Prev を含まない(再帰なし)。schema v4 で hub 側 Snapshot が
// Peers map[string]*PeerResult を持つ。
type PeerResult struct {
    Info        PeerInfo      `json:"info"`
    Reset       ResetResult   `json:"reset"`             // ResetRun の ACK(immutable)
    Finish      FinishResult  `json:"finish"`
    BoundarySendAck [2]time.Time `json:"boundary_send_ack"` // hub 観測の不確実性区間
    Err         string        `json:"err,omitempty"`
    Dropped     []string      `json:"dropped,omitempty"`
    Local       *LocalSnapshot `json:"local,omitempty"`
}
```

budget(32MiB snapshot キャップ内、決定的優先順):

1. **hub 自身の snapshot 分を最初に確保**(実測サイズ。上限 16MiB)
2. 残りを required peer に等分(per-peer 上限、既定 4MiB)。
   セクション drop の優先順(accesslog 詳細 → HTTP 詳細 → …)で
   縮小してもなお超過する required peer は **invalid**(取り込み失敗は
   計測不成立)
3. optional peer は残量の範囲で取り込み、超過は Dropped 記録 + partial

## agent と配布(v3 変更)

- `cmd/isutools-agent`: 既存パッケージの配線のみ。
  複数 DB target は `ISUTOOLS_AGENT_TARGETS="name=dsn;name2=dsn2"`
  (01 の TargetID と同一の名前空間)
- 配布: **リリース tag 固定の事前 build 済み単一 binary + SHA-256
  checksum** を GitHub Releases に添付(make target で linux/amd64,
  arm64 を cross-build)。hub と agent は**同一 binary バージョン**を
  配布する運用を標準とし、INTEGRATION.md に scp 手順を記載。
  `go run @latest` は例示にも使わない
- bind は loopback 限定。peer 指定は literal loopback IP のみ
  (SSH トンネル強制)。redirect 禁止・Proxy 無効の専用 Transport・
  header/body/展開後サイズ上限・並行度 4・per-peer 2s / total 5s
  deadline・peer 数上限 8・重複 endpoint 拒否(v2 から維持)

## E2E マトリクス(v2 から維持 + 追加)

- 構成: bare metal/systemd、Docker host namespace、Docker 別 namespace
  (別 namespace では mysqld・物理 NIC が見えないことを確認し、
  identity で判別できることを検証)
- トポロジ: app+DB / app×2+DB / app+DB×4(shard)
- 障害: peer 再起動、SSH トンネル切断、fetch timeout、version skew、
  reset 中の peer 障害、**finish 中の peer 障害と復旧**、
  duplicate identity、32MiB 総上限、peer 数超過
- 検証: 同一 run の全 peer 区間表示(境界時刻 + uncertainty)、
  invalid run で bench が開始されないこと、
  **fetch 遅延が計測区間に影響しないこと**(FinishRun 後に故意に
  遅延させて取得)

## 実装ステップ

1. **ADR 作成・承認**(run プロトコル 2 バリア、invalid 契約、
   LocalSnapshot/schema v4、budget 決定則、配布方式)— 2 日
2. Phase A: agent binary + handshake + 複数 target — 3 日
3. Phase B: ResetRun バリア(503/invalid 含む)— 3 日
4. Phase C: FinishRun バリア + immutable run 取得 + budget — 4 日
5. E2E マトリクス + 配布(cross-build/checksum)+ docs — 3 日

計 **15 日程度**。ADR は上記の分散終了契約(FinishRun)まで要件に
含めた上でレビューに回す。

## リスク

| リスク | 対策 |
|---|---|
| バリア 2 回分の運用複雑化 | bench.sh 例を提供(reset→bench→save だけは従来どおり。バリアは hub 内部) |
| peer の immutable 保存領域 | run 1 つ分のみ保持(新 run で置換)。DataDir 不要の in-memory + サイズ上限 |
| namespace 不可視 | E2E で確定し identity 表示で判別可能にする |
| 32MiB との衝突 | budget 決定則(hub 優先・required invalid・optional partial) |
