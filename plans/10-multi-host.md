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

02 の run lifecycle(Start → Finish → Abort → Ack/Expire)を
**一対一で wire に写す**(v5):

```
GET  /peer/info                     handshake(識別・互換性・capability)
POST /peer/runs                     StartRun {run_id, nonce}    → StartResult
POST /peer/runs/{run_id}/finish     FinishRun(freeze 受付)     → FinishAccepted
GET  /peer/runs/{run_id}            状態照会 + immutable snapshot 取得
                                    (finishing 中は 202 + 状態、finished で 200)
POST /peer/runs/{run_id}/abort      AbortRun(冪等)            → AbortResult
POST /peer/runs/{run_id}/ack        取得完了の明示 ACK(冪等)  → 204
```

### handshake(v2 から維持 + 判定変更)

`PeerInfo{schema_version, protocol_version, capabilities, agent_id,
identity(03), role, agent_version, started_at}`。**required peer** は
次のいずれかで **preflight 失敗 = run を開始せず invalid**:
接続不可 / protocol_version 不一致 / schema_version 非互換 /
hub が要求する capability の欠如。optional peer は partial 記録で継続。

**identity の 2 層分離(v4 修正)**: v3 の
「machine_id+boot_id+NetNS 一致 = 重複拒否」は、同一ホスト・同一 netns 上の
複数 app プロセスという正当な構成を拒否してしまう。

- **host identity**(machine_id/boot_id hash): hoststats の
  host 単位 dedup に使う(同一 host の複数 peer から hoststats を
  二重計上しない)。拒否はしない。
  **観測範囲を無視した dedup はしない(v5 修正)**: 同一ホストでも
  host namespace の agent と container 内 visible-root の agent では
  CPU/iowait/cgroup の観測値が異なる。dedup キーは
  (host identity, namespace 群, cgroup_scope) とし、**集約の代表値には
  明示的な host scope の agent だけを採用**する。host scope が無い場合は
  代表値を出さず、各観測を scope 付きで並記する
- **agent instance identity**(`agent_id`: 起動時に生成し DataDir に
  永続化する UUID + role/process 情報): **peer の識別に使う**。
  同一 agent_id の二重指定のみ設定エラー

### participant モデル(v4 修正)

v3 は「hub を先に freeze してから peer へ送る」「hub は boundary→flush、
peer は flush→boundary」と、hub と peer の扱い・順序が非対称だった。
v4 では **run_id の発番とローカル reset を分離し、hub 自身を
participant #0 として peer と同一のコードパス・同一の順序で扱う**:

- participant = hub-local + 各 peer。Start/Finish は全 participant へ
  **並列に**発行する(hub-local は関数呼び出し、peer は HTTP)
- 各バリアで participant ごとに送信/ACK 時刻を記録し、
  **uncertainty = [送信, ACK] 区間**として保存する(hub-local は
  実行区間そのもの)。ホスト間の時計は比較しない

### StartRun バリア

1. hub の Controller が run_id を発番し、preflight(handshake 再検証)。
   required 不適合 → **reset を開始せず invalid + 503**
2. 全 participant へ StartRun(run_id, nonce) を並列発行。各 participant は
   02 の StartRun(世代スワップ + baseline 同期採取)を実行し、
   不変の StartResult を ACK として返す
3. **全 required ACK が揃ってから** hub の `POST /reset` が応答する。
   required 失敗 → 503 + invalid(bench を開始させない)。optional → partial
4. **部分開始からの回復(v5・CRITICAL 対応)**: required 失敗時、hub は
   503 を返す**前に**、既に started になった全 participant へ
   **AbortRun(run_id) を並列送信**する(冪等・失敗しても各 peer の
   TTL で最終的に回収)。hub の invalid 記録には**部分開始 peer 一覧**と
   各 abort の成否を含める。これにより「成功 peer が started に残って
   次 run を 409 で拒否し続ける」停止状態を排除する。
   **E2E 必須ケース: 部分開始 → abort → 次 run 成功**

### FinishRun バリア(v5: 02 の終了契約を全 collector に適用)

v4 は freeze 対象が HTTP 世代と accesslog EOF だけで、
sqlstats/counters(世代型)や procstats/sqlrows/dbpool/network/
hoststats(baseline 型)の終了値を snapshot 生成時に読んでいたため、
Finish 後の負荷や fetch 遅延が混入し得た。v5 は 02 の FinishRun 契約を
そのまま使う:

1. **freeze phase(高速・同期)**: 全 generation collector の
   `Freeze`(sqlstats/counters を含む)+ 全 baseline collector の
   `CaptureFinal`(procstats/sqlrows/dbpool/network/hoststats の
   終了サンプル同期取得)。これが全セクション共通の計測終了境界
2. participant は freeze 完了時点で **FinishAccepted**(collector 別
   frozenAt / final SampledAt)を即 ACK する(Drain・snapshot 構築は
   待たない — deadline 分離のため)
3. background で frozen handle を Drain し、**固定値だけから**
   LocalSnapshot を構築して immutable 保存(state=finished)
4. hub は `GET /peer/runs/{run_id}` を **polling**(finishing 中は
   202 + 状態、finished で 200 + LocalSnapshot)で取得する。
   run_id 不一致は protocol error
5. required の freeze 失敗 → invalid(部分開始と同様に abort 伝播)。
   optional → partial

### deadline の分離(v5・HIGH 対応)

v4 は per-peer 2s / total 5s の単一 deadline に対し 02 の Drain 上限が
10s で、正常な長寿命リクエストでも hub 側が先に timeout していた。

| フェーズ | deadline |
|---|---|
| StartRun バリア(境界+baseline は同期・高速) | per-peer 3s / total 6s |
| FinishRun freeze 受付(freeze phase のみ待つ) | per-peer 3s / total 6s |
| snapshot polling(Drain 10s + 構築を含む) | per-peer 20s / total 30s |
| fetch(200 応答の body 読み) | per-peer 5s |

### participant 状態機械(v5: 遷移表で一意化)

```
idle → started → finishing → finished → acknowledged
  ↑        └──────┴─→ aborting → aborted ─┘(TTL/ack 後に破棄 → idle)
```

**state × API の遷移表**(HTTP status と返却 DTO を一意に定義。
v4 の「保存済み結果を返す」と「finished 後 409」の矛盾を解消):

| state \ API | StartRun(同一 run_id+nonce) | StartRun(別 run_id) | FinishRun(同一) | FinishRun(別) | Abort(同一) | GET(同一) | Ack(同一) |
|---|---|---|---|---|---|---|---|
| idle | 200 保存済み StartResult(TTL 内) / 404(期限切れ) | 200 新規開始 | 404 | 404 | 200 no-op | 404 | 404 |
| started | 200 保存済み StartResult | **409** | 202 freeze 実行 → FinishAccepted | 409 | 200 abort | 202(未 finish) | 409 |
| finishing | 200 保存済み StartResult | 409 | 200 保存済み FinishAccepted | 409 | 200 abort | 202 + 状態 | 409 |
| finished | 200 保存済み StartResult | 409(hub は先に Abort か Ack) | 200 保存済み FinishAccepted | 409 | 200 abort | 200 + LocalSnapshot | 204 → acknowledged |
| aborted | 404(この run は不成立) | 200 新規開始 | 404 | 404 | 200 no-op | 410 + abort 理由 | 410 |

- **同一 run_id+nonce の再送は常に保存済みの不変結果**(表の 200 系)。
  「finished 後の同一 StartRun 再送も 409」は撤回し、冪等 200 に統一
- 別 run_id は当該 run が終端状態(acknowledged/aborted/期限切れ)に
  なるまで 409。hub は競合時に明示的に Abort してから新 run を開始する
- **Ack API(v5 新設)**: `POST /peer/runs/{id}/ack`(冪等)。
  hub が LocalSnapshot の受信・検証を終えた後に送る。
  ack または TTL(10 分)で保持を解放する(acknowledged 状態の
  「hub 取得済み」を peer 側が確定できなかった v4 の欠陥を解消)
- 保持は直近 2 run 分 + nonce は TTL 付き履歴(v4 から維持)

## wire DTO と容量 budget

```go
// LocalSnapshot: peer が保存・返却する自ホスト分のみの DTO。
// Peers / Prev を含まない(再帰なし)。schema v4 で hub 側 Snapshot が
// Peers map[string]*PeerResult を持つ。
type PeerResult struct {                                     // v5: 02 の DTO と一対一
    Info          PeerInfo       `json:"info"`
    Start         StartResult    `json:"start"`               // StartRun の ACK(immutable)
    Finish        FinishAccepted `json:"finish"`              // freeze 時刻群(immutable)
    StartSendAck  [2]time.Time   `json:"start_send_ack"`      // hub 観測の不確実性区間(開始)
    FinishSendAck [2]time.Time   `json:"finish_send_ack"`     // 同(終了)。単一配列に潰さない
    Aborted       *AbortResult   `json:"aborted,omitempty"`
    Err           string         `json:"err,omitempty"`
    Dropped       []string       `json:"dropped,omitempty"`
    Local         *LocalSnapshot `json:"local,omitempty"`
}
```

budget(32MiB snapshot キャップ内、決定的優先順):

1. **hub 自身の snapshot 分を最初に確保**(実測サイズ。上限 16MiB)
2. 残りを required peer に等分(per-peer 上限、既定 4MiB)。
   縮小は 2 段階を区別する(v4 修正 — 「drop して収めたら valid に
   見える」問題の解消):
   - **行数の top-N 縮小**(SQL/HTTP 表の下位行を削る): 許容。
     縮小した事実を Dropped に記録するのみ(run 状態は不変)
   - **セクション全欠落**(accesslog 全体を落とす等): required peer では
     **最低でも partial**。hub が必須と宣言した capability に対応する
     セクションの欠落は **invalid**
   - top-N 縮小でもなお per-peer 上限を超える required peer は invalid
3. optional peer は残量の範囲で取り込み、超過は Dropped 記録 + partial

## agent と配布(v3 変更)

- `cmd/isutools-agent`: 既存パッケージの配線のみ。
  複数 DB target は **JSON ファイル**で宣言する(v4 修正 — DSN には
  driver が含まれず `name=dsn` 形式では driver を特定できないため。
  区切り文字が DSN と衝突する問題も回避):

  ```bash
  export ISUTOOLS_AGENT_TARGETS_FILE=/etc/isutools/targets.json
  ```
  ```json
  [
    {"id": "shard1", "driver": "mysql", "dsn": "user:pass@tcp(127.0.0.1:3306)/isuconp"},
    {"id": "shard2", "driver": "mysql", "dsn": "user:pass@tcp(127.0.0.1:3307)/isuconp"}
  ]
  ```

  `driver` は必須項目。id は 01 の TargetID と同一の名前空間。
  **ファイルの安全性契約(v5)**: regular file であること・agent 実行
  ユーザー所有・mode 0600・サイズ上限 64KiB を起動時に検証し、
  symlink は拒否する。**DSN がログ・PeerInfo・snapshot・health の
  いずれにも出力されないことをテストで固定する**(表示は 01 の
  allowlist Display のみ)
- 配布: **リリース tag 固定の事前 build 済み単一 binary + SHA-256
  checksum** を GitHub Releases に添付(make target で linux/amd64,
  arm64 を cross-build)。hub と agent は**同一 binary バージョン**を
  配布する運用を標準とし、INTEGRATION.md に scp 手順を記載。
  `go run @latest` は例示にも使わない
- bind は loopback 限定。peer 指定は literal loopback IP のみ
  (SSH トンネル強制)。redirect 禁止・Proxy 無効の専用 Transport・
  header/body/展開後サイズ上限・並行度 4・peer 数上限 8・
  重複 endpoint 拒否(v2 から維持)。deadline は**フェーズ別**
  (「deadline の分離」の表を正とする — v5)

## E2E マトリクス(v2 から維持 + 追加)

- 構成: bare metal/systemd、Docker host namespace、Docker 別 namespace
  (別 namespace では mysqld・物理 NIC が見えないことを確認し、
  identity で判別できることを検証)
- トポロジ: app+DB / app×2+DB / app+DB×4(shard)
- 障害: peer 再起動、SSH トンネル切断、fetch timeout、version skew、
  reset 中の peer 障害、**finish 中の peer 障害と復旧**、
  **部分開始 → AbortRun 伝播 → 次 run 成功(v5・必須)**、
  duplicate identity、32MiB 総上限、peer 数超過
- 検証: 同一 run の全 peer 区間表示(境界時刻 + uncertainty)、
  invalid run で bench が開始されないこと、
  **fetch 遅延が計測区間に影響しないこと**(FinishRun 後に故意に
  遅延させて取得)

## 実装ステップ

1. **ADR 作成・承認**(run lifecycle の wire 写像(Start/Finish/
   Abort/Ack)、遷移表、invalid 契約、LocalSnapshot/schema v4、
   budget 決定則、配布方式)— 2 日
2. Phase A: agent binary + handshake + 複数 target — 3 日
3. Phase B: StartRun バリア + **AbortRun 伝播**(503/invalid 含む)— 4 日
4. Phase C: FinishRun freeze 受付 + polling 取得 + Ack/TTL + budget — 5 日
5. E2E マトリクス + 配布(cross-build/checksum)+ docs — 3 日

計 **17 日程度**(v5: Abort/Ack と遷移表の追加で +2 日)。ADR は
run lifecycle の完全な写像を要件に含めた上でレビューに回す。

## リスク

| リスク | 対策 |
|---|---|
| バリア 2 回分の運用複雑化 | bench.sh 例を提供(reset→bench→save だけは従来どおり。バリアは hub 内部) |
| peer の immutable 保存領域 | 直近 2 run 分を ACK/TTL(10 分)まで保持(状態機械の項)。in-memory + サイズ上限 |
| namespace 不可視 | E2E で確定し identity 表示で判別可能にする |
| 32MiB との衝突 | budget 決定則(hub 優先・required invalid・optional partial) |
