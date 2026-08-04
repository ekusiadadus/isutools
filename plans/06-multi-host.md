# 06: 複数台構成の横断計測

対象リリース: v1.4.0(スコープ最大。フェーズ分割) /
変更箇所: 新規 `cmd/isutools-agent`、`web`(peers)、schema_version 4

## 背景

ISUCON12/13 優勝チームは Netdata で全サーバを常時監視し、
app/DB 分離(ISUCON12: 11:20 の分離で +10k、DB4 台で 210k)や
役割分担(ISUCON13: c5.large×3 の分散、DNS の S3 分離)を主要な打ち手に
している。ISUCON10 も level 3 以降は「複数台へどう分散するか」が本題。

isutools は単一プロセス計測 + ローカル procstats のみで:

- **DB 専用ホストの CPU/iowait/メモリが見えない**(dbinspect は
  スキーマを取るだけ。MySQL ホストの OS 資源は観測外)
- 複数インスタンスの snapshot を突き合わせる手段が diff の手動運用しかない

## ゴール

1. アプリを載せないホスト(DB/DNS 専用)でも OS 資源 + advisor を
   計測できる軽量 agent を提供する
2. アプリ側 isutools が各 agent の snapshot を取り込み、
   **1 つのダッシュボードに全ホストの資源状況を並べて表示**する
3. reset / save の世代を全ホストで揃える(app 側の reset が agent にも伝播)

## 非ゴール

- SQL / HTTP 計測の複数ホスト集約(SQL はアプリプロセスに紐づくため
  各アプリインスタンスの数値をそのまま並べる。数値の合算はしない —
  二重計上や p95 の合成は誤解を生む)
- 常時監視・時系列グラフ(Netdata の代替はしない。区間サマリに徹する)
- 自動サービスディスカバリ(peer は明示列挙)

## 設計

### Phase 1: agent バイナリ(`cmd/isutools-agent`)

リポジトリ初のバイナリ。既存パッケージの再構成のみで作る:

```
isutools-agent:
  procstats(CPU/RSS + 計画03 の network)
  advisor(OS check + MySQL check: ISUTOOLS_AGENT_DSN があれば)
  web.NewHandler(SQL/HTTP/accesslog は nil skip)
  → 既存の admin サーバをそのまま起動(127.0.0.1:19191)
```

- 配布: `go run github.com/ekusiadadus/isutools/cmd/isutools-agent@latest`
  1 コマンド(ISUCON 当日にバイナリ配布不要)
- DB ホストで `ISUTOOLS_AGENT_DSN` を与えると advisor の MySQL check
  (max_connections / buffer_pool / slow_log)と計画 01 の sqlrows も動く
  → **slp 相当の計測を DB ホスト側で完結**できる
- セキュリティ: 既存の loopback 既定 + SSH トンネル前提をそのまま適用

### Phase 2: peers 取り込み(hub = アプリ側 isutools)

```bash
export ISUTOOLS_PEERS="db1=127.0.0.1:29191,dns=127.0.0.1:29192"
# ← SSH トンネル (ssh -L 29191:127.0.0.1:19191 db1) 越しの列挙を想定
```

- snapshot / save 時に各 peer の `GET /json` を並列取得
  (per-peer 2 秒タイムアウト、失敗は partial として peer 名と理由を記録)
- Snapshot に additive で格納し、**schema_version を 4 に bump**:

```go
Peers map[string]*PeerSnapshot `json:"peers,omitempty"`

type PeerSnapshot struct {
    FetchedFrom string              `json:"fetched_from"`
    Revision    string              `json:"revision"`   // バージョン齟齬検出
    Err         string              `json:"err,omitempty"`
    Proc        *procstats.Snapshot `json:"proc,omitempty"`
    Advisor     []advisor.Check     `json:"advisor,omitempty"`
    SQLRows     *sqlrows.Snapshot   `json:"sqlrows,omitempty"`
}
```

- peer の /json 全体は取り込まない(32MiB キャップ保護のため
  proc / advisor / sqlrows のみ抽出。per-peer 1MiB 上限)
- template: 「Hosts」セクションを新設し、ホスト×(CPU busy / iowait /
  top process / TIME_WAIT / NIC)のマトリクス表示。
  「どのホストが飽和しているか」を 1 画面で判断できることが目的
- advisor 統合: `hosts-imbalance` check —
  いずれかのホストの busy > 80% かつ別ホストの busy < 30% → info
  「役割再配置の余地(ISUCON12/13 の分離パターン)」

### Phase 3: reset 伝播

- app 側 `POST /reset` 時に各 peer の `POST /reset` を並列呼び出し
  (失敗は degraded で継続)。IMPLEMENTATION_STATUS の open item 7
  (cross-collector shared generation gate)をホスト間に拡張する形
- 世代番号は peer 間で独立のまま、snapshot に「reset 伝播の成否」を記録
  (時計合わせや世代番号同期はしない — 複雑化に見合わない)

## セキュリティ設計(SSH-only 原則の維持)

- peer アドレスは **loopback のみ許可**(SSH トンネル強制)。
  非 loopback を指定した場合は起動時に fail し、
  `ISUTOOLS_ALLOW_UNAUTHENTICATED=1` 相当の明示 opt-in でのみ許可
- agent 側も既存の admin サーバと同じ bind 制約(v1.1.0 で確立した
  SSH-only 決定に従う)

## 実装ステップ(TDD)

1. Phase 1: cmd/isutools-agent(main は配線のみ、テストは既存
   パッケージのユニットで担保 + agent 起動のスモークテスト)
2. Phase 2: peer fetch(httptest でフェイク peer、タイムアウト・
   partial・1MiB 上限・revision 齟齬 warn をテスト先行)
3. Hosts template + advisor `hosts-imbalance`
4. Phase 3: reset 伝播
5. schema_version 4 の互換テスト(v3 スナップショットの読み込み・diff)
6. docs: INTEGRATION.md「複数台構成」節(SSH トンネル手順、
   3 台構成の実例)、README

## テスト計画

- unit: peers パース(name=addr 列挙、非 loopback 拒否)
- unit: peer 応答の抽出と上限、遅延 peer のタイムアウト、全滅時 partial
- integration: agent の /json が SQL なし構成で正しく skip されること
- E2E(手動): private-isu を app+DB 2 コンテナに分離し、
  DB 側 agent の iowait / sqlrows が hub に出ることを確認

## リスク

| リスク | 対策 |
|---|---|
| スコープ肥大 | Phase 1 だけでも価値がある(DB ホストで単体起動)。Phase 順に PR を分ける |
| バージョン齟齬(hub と agent) | revision を snapshot に記録し不一致で warn |
| snapshot 肥大 | per-peer 1MiB + 抽出フィールド限定 |
| ISUCON レギュレーション(外部持ち込み) | go run 1 コマンド + 全計測が自ホスト内で完結する設計を維持 |
| cmd/ 追加によるモジュール肥大 | agent は既存パッケージの配線のみ。新規依存ゼロ |

## 見積もり

Phase 1: 1 日 / Phase 2: 2 日 / Phase 3: 0.5 日 / docs+E2E: 1 日。
計画 01〜04 の完了後に着手(agent の価値が network/sqlrows に依存)。
