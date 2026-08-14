#!/usr/bin/env bash
set -euo pipefail

example_dir=${ISUTOOLS_EXAMPLE_DIR:-/home/isucon/isutools-example}
dropin=/etc/systemd/system/isupipe-go.service.d/zz-flow-off.conf

if [ "${ISUTOOLS:-}" = off ]; then
  sudo install -o root -g root -m 0644 \
    "$example_dir/isupipe-go.flow-off.conf" "$dropin"
else
  # This exact temporary override is owned by this helper.
  sudo rm -f "$dropin"
fi
sudo systemctl daemon-reload
sudo systemctl restart isupipe-go
for _ in $(seq 1 50); do
  if curl -fsS --max-time 1 http://127.0.0.1:19196/json >/dev/null 2>&1; then
    exit 0
  fi
  sleep 0.1
done
echo "isupipe-go did not become ready" >&2
exit 1
