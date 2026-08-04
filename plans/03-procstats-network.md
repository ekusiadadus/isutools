# 03: TCP 接続状態・NIC 帯域のランタイム観測

対象リリース: v1.2.x / 変更箇所: `procstats`(拡張)、`advisor`、`web`

## 背景

- ISUCON14 感想戦: TIME_WAIT による接続枯渇を `ss`/`netstat` で監視し、
  複数 IP 付与・Keep-Alive 調整で回避
- ISUCON12 優勝: 終盤 CPU に余裕がある状態で**帯域**がボトルネック化

isutools の現状:

- advisor は `ip_local_port_range` / `somaxconn` を**静的に**検査するのみ
- `/proc/net` の利用は HTTP/3 の UDP listener 確認だけ
- procstats は CPU(全体 + プロセス別)と RSS のみ。ネットワークは
  accesslog の HTTP バイト数しかなく、wire レベルの飽和が見えない

## ゴール

ベンチ区間(reset→snapshot)について:

1. TCP ソケット状態の要約(established / TIME_WAIT / orphan)を表示
2. NIC 別の送受信スループット(MB/s)・パケット・ドロップを表示
3. TIME_WAIT がポートレンジを圧迫している / NIC が飽和している /
   ドロップが出ている場合に advisor が warn を出す

## 非ゴール

- コネクション単位の追跡(ss 相当の 5-tuple 列挙)。/proc/net/tcp の
  全行パースはソケット数に比例して高コストであり、要約で十分
- 帯域の「上限」自動判定が不可能な環境(仮想 NIC で speed が読めない)での
  飽和判定(その場合は実測値の表示のみ)

## データソース

すべて低コストの小さなファイル(数百バイト〜数 KB):

| ファイル | 取得内容 |
|---|---|
| `/proc/net/sockstat` | `TCP: inuse N orphan N tw N alloc N mem N` |
| `/proc/net/sockstat6` | `TCP6: inuse N`(v6 の点観測を併記) |
| `/proc/net/dev` | NIC 別 rx/tx bytes・packets・errs・drop(累積) |
| `/sys/class/net/<if>/speed` | リンク速度 Mbps(読めない/-1 なら省略) |

`/proc/net/tcp` は**使わない**(サイズがソケット数比例)。TIME_WAIT 数は
sockstat の `tw` で足りる。

## 設計

### procstats 拡張

既存の reset-to-snapshot デルタパターンに追加する。

```go
// Snapshot に追加(additive、omitempty)
Network *NetworkStats `json:"network,omitempty"`

type NetworkStats struct {
    TCP        TCPSummary  `json:"tcp"`
    Interfaces []Interface `json:"interfaces"`
}

type TCPSummary struct {          // snapshot 時の点観測
    InUse   int64 `json:"in_use"`
    TimeWait int64 `json:"time_wait"`
    Orphan  int64 `json:"orphan"`
    InUse6  int64 `json:"in_use6,omitempty"`
}

type Interface struct {           // 区間デルタ
    Name        string  `json:"name"`
    RxBytes     uint64  `json:"rx_bytes"`
    TxBytes     uint64  `json:"tx_bytes"`
    RxMBps      float64 `json:"rx_mbps"`      // デルタ / 区間秒
    TxMBps      float64 `json:"tx_mbps"`
    RxDropped   uint64  `json:"rx_dropped"`
    TxDropped   uint64  `json:"tx_dropped"`
    SpeedMbps   int64   `json:"speed_mbps,omitempty"` // 読めた場合のみ
}
```

- `lo` は既定で除外(表示ノイズ)。全 NIC 合計行は出さず、NIC 別のみ
- Reset() で /proc/net/dev の baseline を保持。カウンタ後退
  (NIC リセット)は該当 NIC のデルタを 0 にして errors に記録
- fs は既存 procstats と同じ注入方式(テストは fstest.MapFS)
- Linux 以外は従来どおり collector 無効(既存分岐)

### advisor 統合

`advisor.WithNetwork(checks []Check, net *procstats.NetworkStats, portRange int) []Check`
(snapshot 時差し替え。portRange は既存 OS check が読む
`ip_local_port_range` の幅を流用):

- `net-time-wait`: TimeWait > portRange の 50% → warn
  「エフェメラルポート枯渇が近い。upstream keepalive / 接続再利用
  (ISUCON14: TIME_WAIT 起因の接続失敗)を確認」。
  portRange 不明時は TimeWait > 20000 の絶対閾値
- `net-saturation`: speed が読めた NIC で (Tx or Rx MBps×8) >
  speed の 70% → warn「帯域ボトルネック(ISUCON12 終盤の事例)。
  gzip / 画像縮小 / Cache-Control による転送量削減を検討」
- `net-drops`: RxDropped+TxDropped > 0 → warn(リングバッファ/バックログ)
- Network なし → skip

### web 配線

- procstats.Snapshot 内に増えるだけなので Provider 変更は不要
- template: Processes セクションに「Network」表(TCP 要約 1 行 +
  NIC 別スループット)。diff ビューは対象外(v1)

## 実装ステップ(TDD)

1. procstats: sockstat / dev のパーサをテスト先行
   (正常系・欠損ファイル・カウンタ後退・lo 除外・speed -1)
2. デルタ計算と Snapshot 統合テスト
3. advisor `WithNetwork` 閾値テスト(50% / 70% / 絶対閾値の境界)
4. template 追加 + レンダリングテスト
5. docs: INTEGRATION.md に「ネットワーク観測」節
   (仮想環境で speed が読めない場合の説明を含む)

## テスト計画

- unit: sockstat 形式ゆらぎ(フィールド順・sockstat6 欠如)
- unit: /proc/net/dev のヘッダ 2 行スキップ・インターフェース名の
  コロン処理(`eth0:` と `eth0: ` の両形式)
- unit: 区間 0 秒(reset 直後 snapshot)のゼロ除算回避
- integration: Linux CI 上で実ファイルからのスモークテスト
  (値の妥当性ではなく panic しないこと)

## リスク

| リスク | 対策 |
|---|---|
| コンテナ内で /sys の speed 不可視 | omitempty + 飽和判定をスキップ |
| ホスト共有 NIC(コンテナ)で数値が host 全体 | INTEGRATION.md に注記(network namespace 次第) |
| tw に v6 が含まれるか環境差 | 実装時に実機確認し、doc に計測対象を明記 |
| NIC ホットプラグ | baseline にない NIC は Appeared 扱いでデルタ=累積 |

## 見積もり

パーサ+デルタ 1 日、advisor/web 0.5 日、docs/検証 0.5 日。
