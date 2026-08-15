# ISUCON13 specialist-tool workflow 実機検証（2026-08-15）

Issues #46–#54 で追加した access-log inspector、MySQL slow log / pt-query-digest、
runtime profile / trace、標準 pprof handoff、PGO candidate workflow を、
`ekusiadadus@ssh.almightty.org` の `isucon13` WSL2 環境で再実行した記録です。

この記録は「機能が成立した」「計測runがcorrectnessを通った」「PGOを採用すべき」を分けます。
単独runのscore差は因果効果ではなく、PGOだけを事前宣言したA-B-B-Aで判定しました。

![最終rollback後のISUCON13レポート](./images/isutools-isucon13-specialist-20260815.png)

画像はA2のPGO-off runです。`pass=true`、score `432,917`、source revision `5461a09`。
同じ画面で、HTTP demand、SQL demand、CPU/DB pool/I/O、collector欠損を分けて読めます。

## 固定した対象

| 対象 | 値 |
|---|---|
| isutools branch / validated code SHA | `agent/complete-specialist-tools` / `11f3110088a647b6eb857c0f2b6033fec167016a` |
| ISUCON13 app source | clean `5461a09381e3188bc4e2de40ea34d1f97aed3bcc` |
| Go / host | Go `1.24.0`; AMD Ryzen 9 3900X; 24 logical CPUs; 19.5 GiB |
| app PGO-off binary | `25e885ef4a48a6c762f95739ae82363c8e72de4c2da6a34dfdb72a6ede14e806` |
| pt-query-digest | Percona Toolkit `3.7.1-4` |
| admin endpoint | WSL guest loopback `127.0.0.1:19196` |
| raw evidence | `/home/isucon/isutools-specialist-validation-be2c1b9`、83 files、229,435,516 bytes |

MySQL slow logと追加profileは既定OFFのままです。検証後は一時systemd overrideを退避し、
MySQLを`slow_query_log=0`, `long_query_time=10`, `log_output=FILE`へ戻しました。
アプリはPGO-off binaryで稼働し、元のmutex/block/heap診断設定も復元済みです。

## 実測runとoverhead gate

単独機能runは、測定前に「OFF scoreから5%以上低ければflag、性能採用には使わない」と固定しました。
すべてofficial benchmarkの`pass=true`です。

| run | 有効化したもの | score | OFF比 | 判定 |
|---|---|---:|---:|---|
| OFF baseline | specialist instrumentationなし | 434,794 | — | 基準 |
| slow log | MySQL slow log (`long_query_time=0.01`) | 444,057 | +2.130% | 5%低下なし、単発差は改善主張にしない |
| allocs | allocs pairのみ | 439,845 | +1.162% | 5%低下なし |
| trace | 5秒traceのみ | 439,353 | +1.049% | 5%低下なし |
| CPU | run-aligned CPU profileのみ | 431,824 | -0.683% | 5%低下なし、PGO入力採取専用 |

run順、DB初期化、cache、同hostの短期driftがあるため、この表はprofileの性能効果を証明しません。
各artifactの成立と、大きな退行が観測されなかったことだけを示します。

## 1. Access log: 464,411 requestsをrunへexact attach

OFF baselineのnginx log区間をdevice/inode/offset/clockで固定しました。

- run: `run-88fa477d04ecfe4e`
- snapshot: `20260815-095745.997594951-000001_gen4_5461a09_score434794`
- snapshot SHA: `7f5584103ad10a45f791e2137028e03e5e8c8c0b03a8b010037a374a42881c6e`
- interval: device `2112`, inode `71214`, offset `2499701931..2587339778`
- input: 87,637,847 bytes、464,411 lines、malformed/partial/overflow `0`
- input SHA: `d453a2239e280996d722f878898215857766eecf3880c77a275118dea21ffb71`
- artifact: `07c09703358bfe3da6b72028a95c9938d3f3b3444ce4951f780d4f25518c8724` (`ready`)

上位の累計request timeは、icon read 277,067回 / 83.369秒、livecomment POST
25,452回 / 29.825秒、reaction POST 23,553回 / 19.384秒でした。これは「遅い1回」ではなく、
高頻度経路の累積負荷を先に見る材料です。JSON/Markdown/TSV/CSVを同じ入力から生成し、
portable JSONのsecret scan、HTTP 200、配信body SHA一致を確認しました。

## 2. Slow log / pt-query-digest: aggregateに埋もれるeventを分離

- run: `run-29a66eae0b559d8a`
- snapshot SHA: `d5007ed6680fafc482ef004462e2503750f49bd0231159c0149e736b0be553c6`
- exact interval: device `2112`, inode `94297`, offset `178..3326750`
- input: 3,326,572 bytes、29 events、20 classes、partial `0`
- input SHA: `916f499080f5bd356027f073a050e8b37db6e49cc9eb16670dac89b5086acda1`
- artifact: `10d761a2c99383cc78ee4eceecd29bf321f7874b7cf77c200b74b319e510cc10` (`ready`)

実ログには1 MiBを超える複数行の初期化INSERTがありました。そのためCLIへ
`--max-query-bytes` / `--max-line-bytes`を追加し、既定1 MiB・hard上限8 MiBの範囲でだけ
operatorが引き上げられるようにしました。上位eventは262.579 ms、165.796 ms、147.948 msの
単発INSERTで、whole-run digest平均とevent外れ値を別の証拠として保持できます。

native summaryはSQL literalを含まないportable artifactとしてHTTP 200、
pt-query-digest textはrestricted artifactとしてHTTP 403でした。portable summaryのSQL本文、
Cookie、Authorization、session、email、credential scanも通過しています。

## 3. Runtime / pprof / trace: 種類ごとの意味を保持

allocsはopen/close SHA付きpairとして保存し、`go tool pprof -base`で区間差分を再現しました。
区間allocationは8.31 GiBで、`net/textproto.readMIMEHeader`、JSON decoder、gob decoder、
`bytes.growSlice`等が上位でした。これはallocation削減候補であり、原因確定ではありません。

goroutineはinterval snapshot、threadcreateはcumulative open/close pairとして個別の短いrunで保存。
Go 1.24が提供しないgoroutineleakはempty successにせず
`degraded: goroutineleak=unsupported`になりました。

5秒traceは331,712 bytes、capture span 5.0019秒、`duration-complete`、SHA
`b2d32995b08f13f6193ad5fdaddfbaf71049b7536961bf9b9be6fca37135c4a5`。
Go 1.24の`go tool trace -d=parsed`とviewer起動を確認しました。

CPU bundleは次のchainを固定しました。

- run: `run-9fb196dddf014a88`
- snapshot SHA: `3b2d532d67ba05821bb5ef434a837710e1d7c74ed850f3d1fd9ac24f9c618171`
- CPU profile SHA: `166d76fb31bcec0fccabe40c48b642ea83092a1c6b4f0af8655403131dffa47f`
- captured/analyzed binary SHA: `25e885ef...14e806`、match verified
- bundle: `cf48d8234af5de9bebf9522990e83bb593ee2e52fc31b14f08bebaf320dbe357`

systemdの一時`Delegate=yes` unit内でmemory/pids controllerを委譲し、cgroup v2、
512 MiB memory、4 GiB address space、pids、SIGSTOP bootstrap、membershipをread-back検証しました。
matching binaryではweb/top/source/weblist/disasm/peek/focus/ignore/tagfocus/tagignoreと
2 sample-indexの全12 recipeが`ready`、wrong binaryでは全て`binary-match-required`でした。
通常cgroupでの最初の失敗も削除せず、profileを読む前のfail-closed証拠として残しています。

## 4. PGO: build成功と採用を分ける

candidate `df2ad01c76b7c3ede76047659863c596f78359e7c211dcba74f866d862f0d699`を
clean sourceから生成しました。source treeは変更せず、既存directory/fileは上書きしていません。

| variant | binary SHA | bytes | build time | build-info |
|---|---|---:|---:|---|
| PGO | `02d5d207c51e4498dc809a2c258f8cead7cba70031163782c338dc76e49fe6a0` | 21,638,423 | 9.303 s | Go 1.24.0, PGO on |
| off | `25e885ef4a48a6c762f95739ae82363c8e72de4c2da6a34dfdb72a6ede14e806` | 21,194,328 | 0.702 s | Go 1.24.0, PGO off |

事前条件はA-B-B-A、全block pass、PGO中央値がoff比+2%以上です。

| block | variant | score | pass | run ID |
|---|---|---:|---|---|
| A1 | off | 443,553 | true | `run-85ea26ae43290ff5` |
| B1 | PGO | 437,791 | true | `run-bb0b59bed769f3c0` |
| B2 | PGO | 437,677 | true | `run-d51d11e644fcde85` |
| A2 | off | 432,917 | true | `run-85133818f4580b28` |

off中央値438,235、PGO中央値437,734、差は-0.114%。+2%を満たさないため不採用とし、
off SHAへrollbackしました。ledger SHAは
`bf89d5b1cd90f46013c3019a94666c1a8609c3b5b54bac3ea1393fc1b6c69f70`です。

## 5. fresh private-isu: 独立volumeで再構築

同じWindows hostのUbuntu WSL2へprivate-isu `0dc3be8b5b32d8519e0e841721da3ddf2c6a1542`と
PR head `0dbd6929000aea079723ed315f9d1def5a879075`を新規cloneしました。既存private-isuは停止せず、
Compose project `isutools-specialist-fresh`、専用named volumes、loopback port
`38080/39191/33306`で分離しました。Dockerは`27.0.3` / amd64、MySQLは`9.7.2`です。

`mysqladmin ping`後もdump投入途中では`users` tableが無いことを実測したため、最初の失敗runは
保存せずabortしました。検証用volumeだけを作り直し、`comments/posts/users`のschema readinessと
35,503-byteのトップページを確認してから再実行しました。

| run | 専用計測 | pass / score | run / snapshot SHA |
|---|---|---|---|
| baseline | 通常計測 | `true / 0` | `run-63060cc2f4864931` / `c3e3e48a...fbb3e4` |
| CPU diagnostic | run CPUのみ | `true / 0` | `run-dac4864f87c969b1` / `ddc11eb1...29004` |
| slow-log diagnostic | slow logのみ、CPU off | `true / 0` | `run-9d085737046fa02b` / `4444257e...551d6` |

score 0は初期private-isuのtimeoutを含む統合結果であり、性能成果ではありません。一方、各runは
correctnessの`pass=true`、snapshot durability `durable`、独立した計測設定を保持しています。

- access log: 109,569 bytes、781/781 parsed、malformed/partial `0`、SHA
  `368a744a73a6999903be3a57e7fd2529c40685208de9f0fc65f33e24b71b2bd1`。
  `/`は62回・累計546.421秒、p50 9.987秒、p99 10.036秒で、timeoutの支配を即座に確認できました。
- slow log: source device/inode `2144/33721189`、offset `180..548975`、548,795 bytes、
  input SHA `1e49428e7b9132bce9c4bbddf3372513c725f8744e68e0ef8877f3bbf2e84c39`。
  2,032 events / 16 classes / partial `0`をexact coverageとしてattachしました。artifact
  `eff7bc803007944e7044e7a84e8ac898738c88009294445f647f0b4b93cde6b0`は`ready`、
  pt-query-digest `3.7.1-4` textはrestrictedです。最多classは995回、平均417.018 ms、
  rows examined平均100,003で、N+1型の全表走査候補をSQL literalなしで示しました。
- pprof: bundle `5e7ce9b349ad726cdc5d4a0cfc7b5e96e309ef5d4883030412b8cfb9f9127aff`、
  CPU SHA `edd5e2fc8f47fdb786c2a1a77248b65a76a6e3729972df0342a601806ad1e1aa`、
  binary SHA `efdf86ff483036f6f3352b05921476ae45bcbc4a5db76c2f8eedad623113ee23`。
  Go 1.26 containerで標準`go tool pprof -top`を実行し、`main.makePosts`累計1.74秒などを確認しました。
- PGO: dirty sourceを`source-must-be-a-clean-git-checkout`、cleanな一時sourceでもsnapshot側の
  revision/toolchain provenanceが一致しないため`source-or-toolchain-provenance-mismatch`で拒否しました。
  private-isuでは候補を捏造せず、正のcandidate/build/ABBA証拠は上記ISUCON13だけに限定します。

portable access/slow-log/bundle manifestへCookie、Authorization、DSN、password、raw SQLが無いことを
再scanしました。終了時はCPU profileをoff、MySQLを`slow_query_log=0` / `long_query_time=10`へ戻し、
fresh containersはloopback限定で稼働しています。

## Evidence matrix

| Layer | 結果 |
|---|---|
| Unit / race / lint | root全package、race、vet、golangci-lint、adapter raceを分離実行してPASS |
| Fuzz / negative | artifact/access/slowlog fuzz、secret、oversize、filter、path、symlink/hardlink、mismatchをPASS |
| Local integration | MySQL 8.4 slow-log integration、schema/artifact round tripをPASS |
| Remote functional | ISUCON13 official workflowと、独立fresh private-isuの3 diagnostic workflowをPASS |
| Overhead | 各feature単独runは全てpass、事前5%低下gateに抵触なし |
| Performance | PGO A-B-B-Aは+2%条件を満たさずreject/rollback |
| Publication | portable 200 / restricted 403、content SHA一致、secret scan PASS |

2026-08-14の既存private-isu検証は[別記録](./private-isu-field-verification-20260814.md)のまま保持し、
今回のfresh private-isuやISUCON13数値へ混ぜていません。機能の使い分けと再実行手順は
[specialist-tool playbook](./SPECIALIST_TOOLS.md)、上限と脅威モデルは
[SECURITY_EXTERNAL_ANALYSIS.md](./SECURITY_EXTERNAL_ANALYSIS.md)を参照してください。
