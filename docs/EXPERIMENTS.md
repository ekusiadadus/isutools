# 実験条件・ホスト余力・SQL回数を一緒に確認する

レポートの「実験条件と結果」とRunsのDiffは、スコア、合否、減点、負荷条件、SQL発行回数、
各ホストのCPU余力を同じ表に並べます。警告は次に試す候補です。改善の採用や探索の停止は、
同条件での反復ベンチと整合性確認で決めてください。

## 各走行の条件を保存する

既存の`POST /save?score=...&pass=...`に、任意のJSON本文を追加できます。
次は形式の例です。数値・ホスト名は実際のベンチ結果と構成に置き換えてください。
`available_days`の例は推奨値ではなく、実値を記録するためのものです。

```json
{
  "experiment": {
    "label": "batch-query-B-1",
    "change": "投稿ごとの問い合わせを一括取得に変更",
    "conditions": "workload-v1-seed42-data-v3-60s",
    "load_parameters": {"available_days": "7", "concurrency": "20"},
    "penalty": 0,
    "throughput": 300,
    "p95_ms": 25,
    "error_rate": 0,
    "restarted": true,
    "hosts": [
      {"name": "bench-1", "role": "benchmark", "participation": "active"},
      {"name": "app-1", "role": "app", "participation": "active"},
      {"name": "app-2", "role": "app", "participation": "standby"},
      {"name": "app-3", "role": "app", "participation": "unused"},
      {"name": "db-1", "role": "db", "participation": "active"}
    ]
  }
}
```

上記を`experiment.json`に保存します。

```bash
curl -fsS -X POST 'http://127.0.0.1:19191/save?score=12345&pass=true' \
  -H 'Content-Type: application/json' --data-binary @experiment.json
```

- `conditions`: データセット・seed・ベンチバージョン・実行時間など比較条件の識別子。
  変更したアプリコードは`change`と既存のrevision欄で区別します。
- `load_parameters`: 問題ごとに公開された負荷パラメータの実値。
- `penalty`: ベンチが報告した減点量。省略は未記録で、0と区別します。
- `throughput`: ベンチから得たreq/s。`p95_ms`もベンチから得た値。
  経路ごとのp95を平均して全体p95を作ることはしません。
- `error_rate`: 0〜1の割合。比較間で判定基準を揃えます。
- `restarted`: 必要な再起動後に実施した走行かの申告。trueでも安定した最終スコアとは自動判定しません。
- `hosts`: `name`をlocal hostnameまたはpeer設定名に合わせます。役割は
  `app/db/benchmark/proxy/other`、参加状態は`active/standby/unused/unknown`。
  計測されていない申告ホストも「CPU未計測」として表に残ります。

本文は16 KiB、hostは16件、負荷パラメータは24件まで。ラベルは128 byte、変更内容は512 byte、
比較条件は256 byte、負荷パラメータのキーは64 byte・値は256 byteです。
不正な値・未知フィールドは400となり、runを終了しません。
申告値は独立した検証済み情報ではありません。公開HTML/JSONにも残るため、
パスワード、DSN、セッショントークンを入れないでください。

## 誤読しやすい値

**プロセスCPU 100%は約1コア分、ホストCPU 100%は全コア合計です。**
例えばプロセスCPUが100%でホストidleが73%なら、ホスト全体のCPUを使い切った証拠には
なりません。CPU制限、スレッド、処理の直列部分、負荷配分を調べる候補です。
idleの大きいホストも、単に未使用とは限りません。参加状態は申告欄で別に記録します。

全ホストの実測には、ベンチ機と待機中のアプリ機を含めて
[multi-host計測](INTEGRATION.md)のpeer/agentを登録します。
登録していないホストは自動検出しません。欠測・部分計測・区間のずれを確認し、
欠測をidle 100%として扱わないでください。

SQL形ごとの合計回数とHTTPに紐付いたSQL回数が異なる場合は、Contextが途中で失われて
いないかを優先して確認します。差にはバックグラウンドSQLや計測境界のずれも含まれるため、
差分をそのまま「未追跡SQL数」や追跡率とは呼びません。既存アプリの`db.Query` / `db.Exec`は
`QueryContext(r.Context(), ...)` / `ExecContext(r.Context(), ...)`などへ変更する必要があり、
ライブラリを更新するだけでは十分に紐付きません。

短いSQLでも回数が多ければ、一括取得・キャッシュ・DSN設定を試す候補になります。
`interpolateParams`は引数付きSQLのprepare往復を減らせる場合がありますが、
明示的prepared statementの再利用などでは条件が異なります。
設定単独の比較と結果の整合性を確認してください。

## 変更の効果を判断する

RunsのDiffでA/Bを選ぶと、条件・結果・各ホストを並べて比較できます。
負荷条件、ホスト構成、再起動状態が違う場合は警告します。
条件の文字列が一致しても、入力データや環境が実際に一致することを保証するものではありません。
旧runの未記録値は埋めずに表示します。

索引、一括取得、キャッシュ、DSN、負荷パラメータ、サーバ配置は、可能な限り一つずつ変えて
同時点の対照走行を残してください。複数変更後の上昇を最後の索引だけの効果とは扱いません。
`pass=true`でも減点を記録し、再起動後に反復してから安定した結果として扱います。
近いスコアが続いた場合も、未使用ホスト・負荷条件・SQL回数を確認する前に頭打ちとは判定しません。
