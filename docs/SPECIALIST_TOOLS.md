# isutools / ALP / pt-query-digest / pprof 実戦プレイブック

調査日: 2026-08-15。参照版は ALP v1.0.21、Percona Toolkit / pt-query-digest 3.7.1-4、repository が固定する google/pprof revision。一次資料: [ALP](https://github.com/tkuchiki/alp)、[pt-query-digest](https://docs.percona.com/percona-toolkit/pt-query-digest.html)、[pprof](https://github.com/google/pprof/blob/main/doc/README.md)、[Go diagnostics](https://go.dev/doc/diagnostics)、[Go PGO](https://go.dev/doc/pgo)。

## 30秒で選ぶ

| 目的 | 最初に使うもの | isutools に加わる証拠 |
|---|---|---|
| 過去/別hostのproxy logを任意条件で絞る | `isutools inspect accesslog` または ALP | 同じrunのSQL、DB pool、host、score、User Flowとの境界・snapshot SHA |
| query classの総コストを見る | isutools Performance Schema / sqlstats | run差分、rows examined、no-index、EXPLAIN |
| 1回だけのlock spikeや外れ値を見る | slow log + pt-query-digest | raw SQLを出さない正規化summaryとrun coverage |
| CPU hot path、source、assembly、labelを見る | `go tool pprof` | capture時binary SHA、coverage、誤った`-base`を防ぐrecipe |
| scheduler/GC/syscall/latencyの短い窓を見る | `go tool trace` | duration/bytes/owner/sidecarを持つrun-bound trace |
| profileをcompiler入力として試す | `isutools-pgo` | source非変更、rollback、ABBA ledger、build provenance |

ALPは成熟した単体access-log UX、pt-query-digestは多数のMySQL inputと高度なquery event解析、pprofは対話的graph/source/disassemblyが強い。isutoolsはそれらを再実装せず、HTTP・SQL・host・scoreと同じrunへ安全に接続する。

## Access log: offlineで絞る

```bash
isutools inspect accesslog \
  --file /var/log/nginx/isutools.log \
  --format isutools-ltsv \
  --where 'method in [GET,POST] && status >= 500 && duration >= 100ms' \
  --percentiles 50,90,95,99 \
  --sort p99_ns --reverse --limit 20 --output markdown
```

stdinとfileは同じ結果になる。query value、Cookie、Authorization、session断片は出力しない。10,000 keyを超えるpathは`(other)`へ集約する。offline exact percentileはsampleを保持するため、巨大logでは`--max-input-bytes`、`--max-records`、`--max-keys`を先に下げる。

runへattachする場合は保存済みsnapshotと同じDataDirを渡す。

```bash
isutools inspect accesslog --file run.proxy.log --output json \
  --coverage \
  --start-device DEV --start-inode INODE --start-offset START \
  --start-clock 2026-08-15T12:00:00+09:00 \
  --end-device DEV --end-inode INODE --end-offset END \
  --end-clock 2026-08-15T12:01:00+09:00 \
  --data-dir /var/lib/isutools --run-id RUN_ID \
  --snapshot-base SNAPSHOT_BASE --snapshot-sha256 SNAPSHOT_SHA --snapshot-schema 3
```

`END - START`と入力file bytesが一致しない場合、またはdevice/inode/clockが連続しない場合は
attach自体は監査可能な`partial`として残るが、完全なrun区間とは表示しない。

## MySQL slow log: event外れ値を足す

採点runでは既定OFF。operatorがMySQL設定と権限を明示し、run前後のdevice/inode/offset/DB clockを記録する。rename、copytruncate、DB clock後退はpartialになる。

```bash
isutools analyze mysql-slowlog --file run.slow.log --pt-query-digest \
  --max-query-bytes 8388608 \
  --coverage \
  --start-device DEV --start-inode INODE --start-offset START \
  --start-db-clock 2026-08-15T12:00:00+09:00 \
  --end-device DEV --end-inode INODE --end-offset END \
  --end-db-clock 2026-08-15T12:01:00+09:00 \
  --data-dir /var/lib/isutools --run-id RUN_ID \
  --snapshot-base SNAPSHOT_BASE --snapshot-sha256 SNAPSHOT_SHA --snapshot-schema 3
```

巨大な複数行statementを意図して採る場合だけ`--max-query-bytes`を既定1 MiBから引き上げる（hard上限8 MiB）。一行の上限は`--max-line-bytes`（既定1 MiB、hard上限8 MiB）で別に制御する。

portable JSONはfingerprint hash、count、query/lock time、rows、first/last、outlierだけを持つ。pt-query-digest textはrestrictedでWebへ公開しない。外部processは既定60秒、512 MiB address space、16 MiB outputでhard-limitし、`prlimit`が無いhostでは実行しない。Performance Schemaとslow-log classは証明できるkeyがない限り推測結合しない。

## pprof / trace: 保存runから標準toolへ

```bash
isutools-pprof preflight --admin http://127.0.0.1:19191 --block-runs 4
isutools-pprof fetch --admin http://127.0.0.1:19191 \
  --snapshot-base SNAPSHOT_BASE --snapshot-sha256 SNAPSHOT_SHA --bundle-dir ./bundle
isutools-pprof recipes --bundle-dir ./bundle --binary ./matching-server \
  --source-root ./source --output shell
```

生成例:

```bash
go tool pprof -http=:0 ./matching-server cpu_RUN.pprof
go tool pprof -base allocs_open.pprof -http=:0 ./matching-server allocs_close.pprof
go tool pprof -list=FUNCTION_REGEXP ./matching-server cpu_RUN.pprof
go tool pprof -disasm=FUNCTION_REGEXP ./matching-server cpu_RUN.pprof
go tool pprof -tagfocus=isutools_tuple=TUPLE_ID ./matching-server cpu_RUN.pprof
go tool trace trace_CAPTURE.out
```

累積open/closeだけ`-base`、独立run比較だけ`-diff_base`を使う。`-normalize`はworkload量を正規化したい比較で明示的にだけ使う。binary SHA不一致、pair欠損、source欠損、trace incompleteはready commandにならない。Go公式もdiagnostic同士が干渉し得るとしているため、CPU、trace、memory/block系は個別runで測る。

追加profileは全てopt-in:

```bash
export ISUTOOLS_ALLOCS_PROFILE=on
# または個別runで次のどれか一つ
export ISUTOOLS_GOROUTINE_PROFILE=on
export ISUTOOLS_THREADCREATE_PROFILE=on
export ISUTOOLS_GOROUTINELEAK_PROFILE=on  # runtime対応時のみ
export ISUTOOLS_TRACE_SECONDS=5           # 1..30、他のmanaged profilerとは排他
export ISUTOOLS_TRACE_MAX_BYTES=67108864
```

## PGO: 自動採用しない

Go公式の代表workloadにおける一般的改善例はISUCON score保証ではない。passした代表runの完全なCPU profileだけを候補にする。

```bash
isutools-pgo prepare \
  --snapshot SNAPSHOT.json --snapshot-sha256 SNAPSHOT_SHA \
  --profile cpu_CAPTURE.pprof --binary ./captured-server \
  --source-dir ./clean-source --main-package ./cmd/server \
  --output-dir ./pgo-candidate \
  --rationale 'passing diagnostic run with representative official workload'

isutools-pgo build --candidate-dir ./pgo-candidate --source-dir ./clean-source --variant pgo
isutools-pgo build --candidate-dir ./pgo-candidate --source-dir ./clean-source --variant off
```

candidateは既存directoryへ上書きせず、source treeへ`default.pgo`をcopyしない。`build`は同じclean revisionを固定argvで構築し、PGO/off binaryのSHA、bytes、build time、Go/PGO build infoをprivate manifestへ保存する。manifestのABBA 4-block設計を測定前に埋め、全blockのpass、score、snapshot SHA、binary SHAを残す。correctness失敗ならscoreに関係なく不採用。profile採取runとPGO off/on score runを混ぜない。

## 証拠の読み方

- artifactあり ≠ 解析済み
- 解析済み ≠ binary/source一致
- toolが起動した ≠ score改善
- `pass=true, score=0` ≠ 性能成立
- fixture/local CI ≠ remote ISUCON実測

実機のraw値、tool version、artifact chain、secret scan、失敗とrollbackは環境別検証記録へ追記する。
