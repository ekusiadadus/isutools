#!/usr/bin/env bash
set -euo pipefail

container="isutools-slowlog-${RANDOM}-$$"
work="$(mktemp -d)"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

docker run -d --name "$container" \
  -e MYSQL_ROOT_PASSWORD=root \
  mysql:8.4 \
  --slow-query-log=ON \
  --slow-query-log-file=/var/lib/mysql/slow.log \
  --long-query-time=0 \
  --log-output=FILE >/dev/null

for _ in $(seq 1 60); do
  if docker exec -e MYSQL_PWD=root "$container" mysqladmin ping -h 127.0.0.1 -uroot --silent >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec -e MYSQL_PWD=root "$container" mysqladmin ping -h 127.0.0.1 -uroot --silent >/dev/null
docker exec -e MYSQL_PWD=root "$container" mysql -uroot -e \
  "CREATE DATABASE slowtest; SELECT SLEEP(0.02); SELECT 'private@example.test', 12345;" >/dev/null
docker exec -e MYSQL_PWD=root "$container" mysqladmin -uroot flush-logs >/dev/null
docker cp "$container:/var/lib/mysql/slow.log" "$work/slow.log" >/dev/null

go run ./cmd/isutools analyze mysql-slowlog --file "$work/slow.log" >"$work/report.json" 2>"$work/diagnostic.log"
jq -e '.schema == "isutools.mysql-slowlog/v1" and .health.events > 0 and (.classes | length) > 0' "$work/report.json" >/dev/null
if grep -E 'private@example\.test|12345|MYSQL_ROOT_PASSWORD|root:root@' "$work/report.json" "$work/diagnostic.log"; then
  echo "slow-log analyzer leaked a secret fixture" >&2
  exit 1
fi
echo "MySQL 8.4 slow-log integration: PASS"
