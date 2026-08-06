# ISUCON14 tuned configuration snapshot

This directory contains the non-secret configuration that accompanied the
2026-08-05 ISUCON14 case study. The same files are tracked under
`/home/isucon/webapp/ops` on the measured host.

## Live destinations

| tracked file | live destination |
|---|---|
| `nginx/nginx.conf` | `/etc/nginx/nginx.conf` |
| `nginx/isutools.conf` | `/etc/nginx/conf.d/isutools.conf` |
| `nginx/isuride.conf` | `/etc/nginx/sites-available/isuride.conf` |
| `systemd/isuride-go.service` | `/etc/systemd/system/isuride-go.service` |
| `systemd/isuride-matcher.service` | `/etc/systemd/system/isuride-matcher.service` |
| `mysql/isutools-tuning.cnf` | relevant entries in `/etc/mysql/mysql.conf.d/mysqld.cnf` |
| `env.tuning.example` | selected non-secret entries in `/home/isucon/env.sh` |

`env.sh`, TLS private keys, database passwords, and generated binaries are
deliberately excluded. Apply changes to the live files, validate with
`nginx -t` / `systemd-analyze verify`, and restart only the affected service.

After every nginx change, refresh the compatibility snapshot consumed by the
advisor:

```bash
sudo sh -c 'nginx -T 2>/dev/null > /etc/nginx/isutools-effective.conf'
sudo chmod 0644 /etc/nginx/isutools-effective.conf
```

The main README explains when the snapshot is needed and the three direct
MySQL grants required for EXPLAIN collection.
