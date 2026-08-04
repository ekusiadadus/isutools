# 05: ネットワーク観測(表示のみ)— 再設計版

種別: 機能 / 対象リリース: v1.2.x / 依存: 03(FS 注入設計を共有) / 変更箇所: `procstats` または `hoststats` 配下

## 旧計画(旧03)からの変更点

レビュー指摘を反映:

1. **advisor 閾値を v1 から全廃**(表示のみ)。特に
   「TIME_WAIT > port range の 50% → 枯渇警告」は誤警報になる:
   - 参照記事(ISUCON14)の TIME_WAIT 問題は競技サーバではなく
     **感想戦で改造していたベンチマーカー側の送信元ポート枯渇**
   - `sockstat` の `tw` は incoming/outgoing を区別せず、local ephemeral
     port を消費している socket だけを数えるわけでもない
   - 将来 advisor 化する場合も「outbound-heavy host である証拠」
     (ISUTOOLS_ROLE=proxy 等の宣言 + 10 のトポロジ情報)がある場合に限定
2. **単位の明確化**: `rx_mbit_per_s` / `tx_mbit_per_s`(Mbit/s)。
   NIC の speed(Mbit/s)と直接比較できる単位に統一
3. **新規 NIC の扱い**: baseline に無い NIC は `appeared: true` とし、
   **レートを算出しない**(累積値をデルタ扱いすると架空の巨大帯域になる)
4. **sysfs の分離注入**: 現行 procstats の注入 FS は /proc root のため
   /sys/class/net を読めない。03 で導入する procfs/sysfs 分離に乗る
5. **errors 列の出力追加**(読むのに型に無かった)
6. **専用 feature flag**: `ISUTOOLS_NETSTATS`(既定 on / off 可)。
   本機能単体の ABBA を可能にする
7. **限界の明示**: 区間平均のため瞬間飽和は検出できない。
   network namespace 依存(コンテナは自分の netns しか見えない)を
   ドキュメントと identity(03)で明示

## ゴール

ベンチ区間について、判断材料としての**数値表示**を提供する:

- TCP ソケット要約(inuse / tw / orphan、v4・v6)— snapshot 時点観測
- NIC 別スループット(Mbit/s)・パケット・エラー・ドロップ — 区間デルタ
- リンク速度(読めた場合のみ)を併記し、飽和判断は**読者に委ねる**

## 非ゴール

- advisor 警告(v1)。5-tuple 列挙(/proc/net/tcp 全行パース)。
  瞬間値・時系列。

## データモデル

```go
type NetworkStats struct {
    TCP        TCPSummary  `json:"tcp"`
    Interfaces []Interface `json:"interfaces"`
}

type TCPSummary struct {          // snapshot 時の点観測(/proc/net/sockstat{,6})
    InUse    int64 `json:"in_use"`
    TimeWait int64 `json:"time_wait"` // 方向・port 帰属は区別できない(表示にも注記)
    Orphan   int64 `json:"orphan"`
    InUse6   int64 `json:"in_use6"`
}

type Interface struct {
    Name         string   `json:"name"`
    Appeared     bool     `json:"appeared,omitempty"` // baseline に無い(レート無し)
    RxBytes      uint64   `json:"rx_bytes"`           // 区間デルタ
    TxBytes      uint64   `json:"tx_bytes"`
    RxMbitPerSec *float64 `json:"rx_mbit_per_s,omitempty"` // appeared 時は nil
    TxMbitPerSec *float64 `json:"tx_mbit_per_s,omitempty"`
    RxPackets    uint64   `json:"rx_packets"`
    TxPackets    uint64   `json:"tx_packets"`
    RxErrors     uint64   `json:"rx_errors"`
    TxErrors     uint64   `json:"tx_errors"`
    RxDropped    uint64   `json:"rx_dropped"`
    TxDropped    uint64   `json:"tx_dropped"`
    SpeedMbit    int64    `json:"speed_mbit,omitempty"` // /sys/class/net/<if>/speed(-1/不可時は省略)
}
```

- `lo` は既定除外。カウンタ後退(NIC リセット)は該当 NIC を
  appeared 相当(レート無し)に落として errors に記録
- 区間 0 秒(reset 直後の snapshot)はレート nil

## 設計メモ

- 配置は 03 hoststats と同一 collector 系(procfs/sysfs の 2 FS 注入、
  reset-to-snapshot デルタ)。実装上は hoststats のサブモジュールとし、
  flag だけ独立(`ISUTOOLS_NETSTATS`)にする
- 表示: Host セクション内「Network」表。TimeWait には
  「incoming/outgoing の区別なし」の注記を付す
- capabilities: `netstats` を追加

## 実装ステップ(TDD)

1. sockstat / sockstat6 パーサ(フィールド順ゆらぎ・欠損)
2. /proc/net/dev パーサ(ヘッダ 2 行・`eth0:`/`eth0: ` 両形式・
   16 フィールドの列位置)
3. デルタ+レート(appeared・後退・区間 0 秒・Mbit 換算)
4. speed 読み(-1・欠損・非数)
5. web 表示 + capabilities + flag
6. 単独 ABBA(flag off↔on)で影響ゼロを実測し記録

## テスト計画

- unit: 上記パーサ境界すべて + レート nil 条件
- unit: lo 除外・複数 NIC・カウンタ後退
- integration: Linux CI スモーク

## リスク

| リスク | 対策 |
|---|---|
| コンテナで NIC 名/値が期待と違う | netns 依存を注記。identity(03)の NetNS を並記 |
| speed が仮想 NIC で無意味(-1) | omitempty で出さない。飽和判断を機械化しない |
| 誤解を招く TimeWait 表示 | 注記固定文をテンプレートに含め、テストで固定 |

## 見積もり

1.5 日。
