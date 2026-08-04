# 10: 複数台横断計測 — ADR からの再設計

種別: アーキテクチャ変更 / 対象リリース: v1.4.0 以降 /
依存: 01(registry)、02(coordinator)、03(hoststats/identity)、05(network)
規模: **2〜3 週間**(旧見積もり 4.5 日を撤回)

## 位置づけの訂正(レビュー反映)

旧 06 は本件を「実装コスト大の機能追加」として扱ったが、実際には
**分散計測プロトコル・世代境界・ホスト識別・名前空間・SSH トンネル運用を
新設するアーキテクチャ変更**である。したがって:

1. 実装前に **ADR(Architecture Decision Record)を作成し承認を得る**。
   本書は ADR に先立つ要件と設計方針の整理である
2. 目標を「Netdata 代替」と誤認させない。提供するのは
   **ベンチ区間に限定した、証拠(run_id・区間時刻・identity)付きの
   横断 snapshot** である
3. 3 段階(agent protocol → hub → distributed reset)に分割し、
   各段階を独立に出荷・検証する

## ゴール

1. アプリを載せないホスト(DB/DNS/proxy)でも hoststats(03)+
   network(05)+ advisor + sqlrows(04)を計測できる agent を提供する
2. hub(アプリ側 isutools)が全ホストの snapshot を取り込み、
   **ホスト別に並べて**表示する(合算はしない)。
   app peer については SQL / HTTP / accesslog / connections / counters を
   **全て**取り込む(旧計画の proc/advisor/sqlrows 限定は不足 —
   レビュー指摘どおり「ホスト別に並べる」機能が必要)
3. reset を全 required peer に伝播し、**全 peer の ACK が揃った場合のみ**
   run を valid とする。欠落があれば run は invalid(計測不成立)

## Phase A: agent とプロトコル

### agent(`cmd/isutools-agent`)

- 既存パッケージの配線のみ(新規依存ゼロ):
  hoststats + network + procstats + advisor(+ `ISUTOOLS_AGENT_DSN` が
  あれば sqlrows/dbinspect)+ 既存 admin サーバ
- 配布: `go run github.com/ekusiadadus/isutools/cmd/isutools-agent@latest`
- bind は loopback 限定(既存の SSH-only 決定に従う)

### handshake(`GET /peer/info`)

互換性判定は revision ではなく schema/protocol/capabilities で行う
(レビュー指摘。README 共通契約 4):

```go
type PeerInfo struct {
    SchemaVersion   int      `json:"schema_version"`
    ProtocolVersion int      `json:"protocol_version"` // 本計画で 1 を定義
    Capabilities    []string `json:"capabilities"`     // "hoststats","netstats","sqlrows",...
    Identity        hoststats.Identity `json:"identity"` // hostname/machine-id hash/boot-id hash/ns/role
    AgentVersion    string   `json:"agent_version"`
    StartedAt       time.Time `json:"started_at"`
}
```

- **重複 peer 検出**: machine_id_hash + boot_id_hash + NetNS が一致する
  peer が複数指定されたら設定エラー(同一ホストの二重計上防止)
- hub は protocol_version 不一致の peer を「接続はするが取り込まない」
  状態にし、run を invalid にはしない(handshake 失敗として表示)

## Phase B: hub(peer 取り込み)

### 設定と fetch 制約(レビューの資源・セキュリティ指摘を全数反映)

```bash
export ISUTOOLS_PEERS="db1=127.0.0.1:29191,dns=127.0.0.1:29192"
export ISUTOOLS_PEERS_REQUIRED="db1"     # reset ACK 必須の peer(既定: 全 peer)
```

- peer は **literal loopback IP のみ**(hostname 不可 → DNS rebinding 排除)。
  旧計画にあった非 loopback opt-in は**削除**(SSH-only の一貫性)
- peer 数上限 8。同一 endpoint の重複指定はエラー
- 専用 `http.Transport`: `Proxy: nil`(環境変数無視)、redirect 禁止
  (`CheckRedirect` で拒否)、`MaxResponseHeaderBytes` 制限、
  接続再利用は peer ごとに固定
- body は `io.LimitReader`(per-peer 上限。既定 4MiB、設定可)。
  展開後サイズも同上限で検査(圧縮爆弾対策)
- 並行度上限 4、per-peer 2 秒、**total fetch deadline 5 秒**
- 総量: peers 合計が snapshot 32MiB キャップを超える場合、
  超過 peer を Err 記録で除外(silent drop しない)

### PeerSnapshot(全セクション + 証拠)

```go
type PeerSnapshot struct {
    Info      PeerInfo  `json:"info"`
    FetchedAt time.Time `json:"fetched_at"`
    Err       string    `json:"err,omitempty"`
    Dropped   []string  `json:"dropped,omitempty"` // サイズ超過で落としたセクション名
    // peer の Snapshot 全体(SQL/HTTP/accesslog/connections/counters/
    // proc/host/network/advisor/sqlrows)。合算はせずホスト別に描画
    Snapshot  *Snapshot `json:"snapshot,omitempty"`
}
```

- schema_version は **4 に bump**(トップレベル `peers` の追加は additive
  だが、「複数ホストの run を 1 つの snapshot が表す」という
  **意味の変更**を伴うため。README 共通契約 4 の基準に従う)
- 表示: 「Hosts」セクション(役割・CPU busy・iowait・PSI・TIME_WAIT・
  NIC・メモリの行列)+ ホスト別タブで各 peer の全セクションを描画。
  diff ビューの peer 対応は本計画のスコープ外(将来課題として明記)

## Phase C: distributed reset(レビューの CRITICAL に対応)

旧計画の「degraded で継続・世代独立のまま」を撤回し、02 の契約を拡張する:

1. hub の `POST /reset` は coordinator が run_id を発番後、
   全 peer の `POST /reset`(run_id 付き)を並行呼び出しする
2. 各 peer は自分の ResetResult(reset_started_at / reset_completed_at /
   peer generation)を応答で返す(ACK)
3. **全 required peer の ACK が揃ってから** hub の reset 応答を返す。
   これにより「ベンチ開始は reset 応答後」という既存の運用契約
   (02 で明文化)がそのまま**全ホストの世代確定後**を意味する
4. required peer の ACK が得られない場合、run は **invalid**:
   - hub の応答は 503 + 理由(bench を開始させない)
   - それでも保存された snapshot には invalid と欠落 peer が記録される
5. snapshot には各 peer の ResetResult を **immutable** に保存し、
   reset の最大 skew(全ホストの reset_completed_at の最大差)と
   各ホストの実測区間を表示する
6. optional peer(`REQUIRED` 外)の失敗は partial として記録し
   run は継続する

## E2E マトリクス(レビュー要求の全項目)

構成 3 種 × 障害シナリオ:

- 構成: (a) bare metal / systemd、(b) Docker + host PID/network namespace、
  (c) Docker 別 namespace — (c) では agent から mysqld や物理 NIC が
  **見えないことを確認し**、identity の namespace 情報で判別できることを検証
- トポロジ: app+DB / app×2+DB / app+DB×4(shard)
- 障害: peer 再起動、SSH トンネル切断、fetch timeout、version skew、
  reset 中の peer 障害と復旧、duplicate/入れ替わった peer identity、
  32MiB 総上限到達、最大 peer 数超過
- 検証項目: 同一 run の全 peer interval 一致(skew 表示)、
  invalid run で bench が開始されないこと(bench スクリプト側の確認手順)
- 運用手順: SSH トンネルの張り方・teardown・reconnect を
  INTEGRATION.md に手順化

## 実装ステップ

1. **ADR 作成・承認**(プロトコル、schema v4、invalid run 契約、
   セキュリティモデル)— 2 日
2. Phase A: agent + handshake + identity(03 依存)— 3 日
3. Phase B: fetch 制約付き hub + Hosts 表示 — 4 日
4. Phase C: distributed reset(02 拡張)— 3 日
5. E2E マトリクス + docs — 3 日

計 **15 日程度(2〜3 週間)**。Phase A 単体でも
「DB ホストで agent を立てて個別に見る」価値があるため、
段階ごとに出荷判断する。

## リスク

| リスク | 対策 |
|---|---|
| スコープ肥大 | ADR で境界を固定。diff の peer 対応等は明示的に将来課題へ |
| namespace による不可視 | E2E (c) で挙動を確定し、identity 表示で利用者が判別可能にする |
| ベンチ規約(外部持ち込み) | go run 1 コマンド・全計測が自ホスト内で完結する設計を維持 |
| 32MiB キャップとの衝突 | per-peer 上限 + Dropped 記録(silent drop なし) |
