# Agent / job 軌跡の可視化

`isutools-trajectory` は、アプリ固有の `chair` / `ride` / `taxi` を知らない
post-benchmark viewer です。adapter が read-only に書き出した NDJSON を、外部assetを
持たない1枚のHTMLへ変換します。

```sh
go run ./cmd/isutools-trajectory \
  -input /tmp/trajectory.ndjson \
  -output "$ISUTOOLS_DATA_DIR/trajectory_benchmark-abc123.html"
```

`ISUTOOLS_DATA_DIR`へ `trajectory_*.html` の名前で生成すると、管理画面トップの
**Trajectories** から開けます。通常のrun履歴・diff対象には混ぜません。

HTMLには再生・scrub・速度変更、agentの直近軌跡、jobのpickup/destination、現在の
assignment、waiting/assigned/finished件数、割当時のManhattan距離p95、再割当回数が
含まれます。複数の `assignment` を同じjobへ時系列で渡せるため、rolling windowや
lifelong schedulerが再割当した様子も表示できます。assignmentが1回だけなら、単発の
batch matchingを表示しているにすぎません。

## NDJSON schema v1

時刻はRFC 3339 (`time.RFC3339Nano`) です。行の順序は自由で、描画前に時系列へ
sortされます。

```jsonl
{"type":"meta","schema":1,"title":"benchmark abc123"}
{"type":"agent","id":"agent-1","label":"optional label","kind":"optional kind"}
{"type":"point","agent_id":"agent-1","at":"2026-08-05T12:00:00.123456Z","x":12,"y":34}
{"type":"job","id":"job-1","requested_at":"2026-08-05T12:00:01Z","pickup":{"x":20,"y":30},"destination":{"x":90,"y":10},"finished_at":"2026-08-05T12:00:10Z"}
{"type":"assignment","job_id":"job-1","agent_id":"agent-1","at":"2026-08-05T12:00:02Z"}
{"type":"assignment","job_id":"job-1","agent_id":"agent-2","at":"2026-08-05T12:00:03Z"}
{"type":"assignment","job_id":"job-1","agent_id":"","at":"2026-08-05T12:00:04Z"}
```

- `agent`: 移動主体。実装上の車両、椅子、robot等をadapterでこの語へ変換する。
- `point`: 観測座標。緯度経度に限定せず、有限な2次元座標ならよい。
- `job`: 時刻付きのpickup/destination。`finished_at`は省略可能。
- `assignment`: その時刻以降のjob→agent対応。空の`agent_id`は明示的な解除。
- 上限はagents 2,048、points 2,000,000、jobs 100,000、assignments 1,000,000。
  viewer用exportを無制限なtelemetry sinkにしない。

## isutools snapshotだけでは描けない理由

SQL統計には正規化したqueryと回数・時間を保存しますが、bind parameterは保存しません。
したがって `INSERT ... (chair_id, latitude, longitude)` が実行された証拠はあっても、
個々のIDや座標値は通常のJSON artifactにありません。access logもquery stringを除去し、
Scenario Storiesは安全な疑似sessionとrequest列だけを保持します。

座標をSQL collectorへ一般的に取り込むと、token・個人ID・任意payloadまで保存する危険と、
hot pathでの大きな計測負荷が生じます。このviewerはその境界を変えず、ベンチ終了後に
アプリ固有adapterが必要な列だけをread-only exportする設計です。NDJSONと生成HTMLは
benchmark artifactと同じ機密度で管理してください。

## ISUCON14 adapterの対応

ISUCON14では次のように写像します。

| ISUCON14 | 汎用schema |
|---|---|
| `chairs.id` | `agent.id` |
| `chair_locations.(created_at, latitude, longitude)` | `point.(at, x, y)` |
| `rides.id` + pickup/destination | `job` |
| matching後、最初の `ENROUTE` 時刻 + `rides.chair_id` | `assignment` |
| 最初の `COMPLETED` 時刻 | `job.finished_at` |

exportは計測区間の終了後に行い、run開始より前の最後の位置を各agentにつき1点だけ加えると、
最初のassignment距離も描けます。DBの全履歴ではなく、そのrunで作られたjobと関係agentの
pointだけに絞ります。

read-only queryの雛形は
[`case-studies/isucon14-config/export-trajectory.sql`](case-studies/isucon14-config/export-trajectory.sql)
です。冒頭のrun開始・終了・artifact IDを保存済みsnapshotに合わせ、MySQLのbatch/raw出力で
headerを除いて `.ndjson` へ保存します。DBの `DATETIME` はこの検証環境のJSTとして
`+09:00`を明示しているため、別timezoneの環境へコピーするときはoffsetを変更してください。

重要なのは、現在のISUCON14 schemaでは `rides.chair_id` は1つだけで、過去の再割当履歴を
保持しない点です。通常の1回割当は可視化できますが、将来rolling-window schedulerで
再割当するなら、`job_id / agent_id / assigned_at` のappend-only履歴をadapter側で残す必要が
あります。その履歴を複数の `assignment` 行へ変換すればviewer側の変更は不要です。
