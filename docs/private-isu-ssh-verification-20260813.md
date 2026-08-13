# private-isu SSH実機確認（2026-08-13）

`ekusiadadus@ssh.almightty.org` の既存private-isu環境を、2026-08-13
18:23–18:27 JSTに確認した記録です。アプリコードやprivate-isuの設定は変更せず、
Docker再起動後に停止していた既存Composeコンテナだけを`docker compose start`で
再開しました。named volume、network、過去のisutools結果は再作成していません。

## 実行した確認

```bash
make status
make check
make bench
make verify-results
make pprof PPROF_SECONDS=5
make tunnel
```

`make check`ではprivate-isu HTTP、isutools HTTP、MySQLの全readinessが成功しました。
`make bench`は`reset -> benchmark -> collect -> save -> SCP`を完了し、次のdurableな
snapshotをMacの`~/isutools-private-isu-results`へ取得しました。

| 項目 | 実測値 |
|---|---|
| 時刻 | `2026-08-13T18:26:47+09:00` |
| run ID | `run-6297ea8527095979` |
| generation | `3` |
| score / pass | `0` / `true` |
| validity / partial | `valid` / `false` |
| SQL / HTTP rows | `28` / `32` |
| nginx access log | `808` lines、partial `0` |
| JSON SHA-256 | `e432fd1fdee200cc86c7873b2ecc1da2ec6943c08613324da4c5728bf3661e6d` |
| HTML SHA-256 | `79cc830596f644fc5a23f8d8febb7a6ab7871c2cdaf7bcb121b03f736998fe43` |

![2026-08-13 private-isu SSH verification report](./images/private-isu-ssh-verification-20260813.png)

ベンチマーカー出力には`success=723`、`fail=54`とtimeoutメッセージがあり、
scoreは`0`です。このrunは連携経路と保存耐久性の確認であり、性能改善の証明では
ありません。

`make pprof PPROF_SECONDS=5`は
`~/isutools-private-isu-results/cpu-20260813-182711.pprof`を取得し、
`go tool pprof -top`がGo CPU profile（build ID
`a3f9dd906d7b91ff4268eeeba3bd9b7cd0bf2355`）として読み込みました。ただし、
ベンチ終了後のアイドル区間を取得したためsamplesは0です。CPU hotspotやflame graphの
根拠として使うには、負荷とprofile取得を重ねて再実行する必要があります。

SSH forwardはMac上の`127.0.0.1:19191`で稼働しており、`make tunnel`は
`already available`を返しました。管理画面は外部公開せず、loopbackのまま使用します。

