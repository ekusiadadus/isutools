# 03: hoststats(ホスト資源とホスト同一性)

種別: 基盤 / 対象リリース: v1.2.x / 変更箇所: 新規 `hoststats`、`web`

## 背景(レビュー指摘)

旧 06 は「agent で OS 資源が見える」と書いたが、現行 procstats +
旧 03(network)で取れるのは CPU・プロセス RSS・NIC 平均・TCP 点観測のみ。
DB ホストの診断に必要な以下が**存在しない**:

- 使用可能メモリ、page cache、swap、major fault
- ディスク read/write throughput・IO 時間・キュー(/proc/diskstats)
- PSI(/proc/pressure/{cpu,memory,io})
- filesystem 使用率、cgroup 制限

また 10(multi-host)の peer 識別に必要なホスト同一性情報
(machine-id、boot-id、namespace)も未整備。

## ゴール

1. ベンチ区間の memory / disk / PSI をローカルホストで観測する
2. ホスト同一性(identity)を snapshot に含め、10 の重複 peer 検出と
   コンテナ可視性の判定材料にする
3. v1 は**表示のみ**(advisor 閾値はフィールド実測後)

## 非ゴール

- Netdata 代替の常時時系列監視(区間サマリに徹する)
- 全 mount / 全 device の網羅(主要対象に限定し、限定を明示する)

## データソース

| ファイル | 取得内容 | 型 |
|---|---|---|
| `/proc/meminfo` | MemTotal / MemAvailable / Cached / Dirty / SwapTotal / SwapFree | 点観測(reset 時と snapshot 時の両方を表示) |
| `/proc/vmstat` | pgmajfault | 区間デルタ |
| `/proc/diskstats` | device 別 read/write セクタ(×512B)・IO 時間 ms・加重 IO 時間(キュー) | 区間デルタ(仕様: docs.kernel.org/admin-guide/iostats.html) |
| `/proc/pressure/{cpu,memory,io}` | some/full の avg10・avg60(点観測)+ total(区間デルタ) | 両方(仕様: docs.kernel.org/accounting/psi.html)。カーネル非対応(<4.20 / 無効)は skip |
| `statfs` | `/` と DataDir の使用率 | 点観測 |
| cgroup(下記) | cpu.max、memory.max / memory.current | 点観測。v2 階層のみ対応、v1 は skip |

### cgroup の解決(v3 修正)

`/sys/fs/cgroup` 直下(root)を固定で読むと、systemd service / container
では**自プロセスの実 cgroup ではなく cgroup root** を測ってしまう。
さらに agent と mysqld が別 cgroup の場合、agent の limit を DB の limit と
誤読する危険がある。対応:

- `/proc/self/cgroup` + `/proc/self/mountinfo` から**自プロセスの実 cgroup
  パスを解決**して読む
- snapshot に `cgroup_scope: "visible-root" | "agent-cgroup" |
  "configured-cgroup" | "host"` を必ず記録する。
  **「root と実パスが同一なら host」とは判定しない**(v4 修正):
  cgroup namespace 内では /proc/self/cgroup と mountinfo の見え方自体が
  仮想化され、コンテナ内の現在 cgroup が `/` に見える
  (cgroup_namespaces(7))。既定は **visible-root** とし、
  `host` は明示設定(`ISUTOOLS_CGROUP_SCOPE=host`)または初期 cgroup
  namespace であることの外部証拠がある場合に限定する
- identity(前節)に **CgroupNS**(`/proc/self/ns/cgroup`)を追加し、
  scope の解釈材料として常に併記する
- `ISUTOOLS_CGROUP_PATH` で対象 cgroup(例: mysqld の service cgroup)を
  明示指定できる(scope=configured-cgroup)。指定パスが読めない場合は skip。
  **パス境界の検証(v5)**: 指定は解決済み cgroup mount からの
  **相対パスに限定**し、absolute path・`..` を含むパス・cgroup FS 外へ
  抜ける symlink は拒否する(拒否時は skip + health に理由)。
  escape fixture(`../`、symlink)をテストに含める
- 表示は scope を併記し、「agent の limit ≠ 観測対象サービスの limit」で
  あり得ることを注記する

### identity

```go
type Identity struct {
    Hostname     string `json:"hostname"`
    MachineIDHash string `json:"machine_id_hash"` // sha256(machine-id)[:16]
    BootIDHash   string `json:"boot_id_hash"`     // sha256(boot_id)[:16]
    PIDNS        string `json:"pid_ns"`   // readlink /proc/self/ns/pid
    NetNS        string `json:"net_ns"`
    MntNS        string `json:"mnt_ns"`
    CgroupNS     string `json:"cgroup_ns"` // cgroup_scope の解釈材料(v4)
    Role         string `json:"role,omitempty"` // ISUTOOLS_ROLE=app|db|dns|proxy(自由記述)
    AgentVersion string `json:"agent_version"`  // buildinfo
}
```

- machine-id / boot_id は生値を出さずハッシュ短縮(識別には十分、
  値自体は host 固有情報のため)
- namespace ID は「agent がホストの何を見ているか」の証拠になる
  (コンテナ内 agent の可視性問題 → 10 の E2E で使用)

## 設計

- 新パッケージ `hoststats`。procstats と同じ reset-to-snapshot デルタ +
  collector パターン(`New(procFS, sysFS)` / `Reset()` / `Snapshot()`)
- **procfs と sysfs は別 FS として注入**(README 共通契約 5。
  現行 procstats の root=/proc では /sys を読めない問題の解消。
  05 network とも共有する設計)
- 出力は `Snapshot.Host *hoststats.Snapshot`(additive、omitempty)。
  meta.capabilities に `hoststats` を追加
- feature flag: `ISUTOOLS_HOSTSTATS=off` で無効化(既定 on。
  読み取りは小ファイルのみで、単独 ABBA で影響ゼロを確認して出荷)
- 表示: 「Host」セクション新設。memory(available の reset→snapshot
  変化)、disk(device 別 MB/s・IO util%)、PSI(avg10)、fs 使用率、
  identity(折りたたみ)
- Linux 以外・コンテナで読めないファイルは項目単位で skip し
  health に理由を残す(fail-open)

## 実装ステップ(TDD)

1. meminfo / vmstat / diskstats / PSI パーサをテスト先行
   (フォーマットゆらぎ・欠損・カーネル非対応)
2. デルタ計算(カウンタ後退 = 再起動検出 → 区間 invalid 扱いで
   Err 記録)
3. identity 取得(readlink 不可・machine-id 欠如の劣化)
4. web 配線 + template + capabilities
5. docs: INTEGRATION.md「ホスト資源観測」節(コンテナでの可視性の
   注意 — namespace 依存であることを明記)

## テスト計画

- unit: PSI 無効カーネル(ファイル欠如)で PSI のみ skip
- unit: diskstats のセクタ→バイト換算・パーティション行の除外
  (主デバイスのみ。判定は `/sys/block` 配下の存在で行う)
- unit: cgroup v1 環境(cpu.max 欠如)で cgroup のみ skip
- integration: Linux CI でのスモーク(panic しない・JSON 生成)

## リスク

| リスク | 対策 |
|---|---|
| device 名の多様性(nvme/vd/xvd/dm) | /sys/block 由来の実在確認で機械判定。フィルタ規則を doc 化 |
| コンテナで meminfo がホスト値 | identity の namespace 情報と併記し、解釈を読者に委ねる注記 |
| PSI の some/full の誤読 | 表示ラベルに some/full を明記。閾値判定はしない |

## 見積もり

2 日。
