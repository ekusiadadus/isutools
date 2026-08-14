#!/usr/bin/env bash
set -euo pipefail

admin_url=${ISUTOOLS_ADMIN_URL:-http://127.0.0.1:19196}
data_dir=${ISUTOOLS_DATA_DIR:-/home/isucon/isutools-data}
stage_dir=${ISUTOOLS_STAGE_DIR:-/mnt/c/Users/ekusi/isutools-isucon13-results-stage}
bench_result=/tmp/result.json

abort_run() {
  curl -fsS -X POST "$admin_url/abort" >/dev/null 2>&1 || true
}
trap abort_run ERR INT TERM

for command in curl python3 sha256sum; do
  command -v "$command" >/dev/null || {
    printf 'required command not found: %s\n' "$command" >&2
    exit 2
  }
done

curl -fsS --max-time 10 "$admin_url/json" >/dev/null
for unit in nginx mysql pdns isupipe-go; do
  systemctl is-active --quiet "$unit" || {
    printf 'required service is not active: %s\n' "$unit" >&2
    exit 1
  }
done

headers=$(mktemp)
trap 'rm -f "$headers"; abort_run' EXIT INT TERM
curl -fsS -D "$headers" -o /dev/null -X POST "$admin_url/reset"
run_id=$(awk 'tolower($1) == "x-isutools-run-id:" {gsub("\\r", "", $2); print $2}' "$headers")
test -n "$run_id" || {
  printf 'reset returned no X-Isutools-Run-Id\n' >&2
  exit 1
}
printf 'isutools_run_id=%s\n' "$run_id"

cd /home/isucon
if ! ./bench run --dns-port 1053 --enable-ssl; then
  printf 'official benchmark failed; aborting run %s\n' "$run_id" >&2
  exit 1
fi

test -s "$bench_result" || {
  printf 'official benchmark did not create %s\n' "$bench_result" >&2
  exit 1
}

read -r score pass language < <(
  python3 - "$bench_result" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as src:
    result = json.load(src)
score = result.get("score")
passed = result.get("pass")
if isinstance(score, bool) or not isinstance(score, (int, float)):
    raise SystemExit("benchmark result score is not numeric")
if not isinstance(passed, bool):
    raise SystemExit("benchmark result pass is not boolean")
print(score, str(passed).lower(), result.get("language", "unknown"))
PY
)

stamp=$(date '+%Y%m%d-%H%M%S')
durable_result="$data_dir/official-benchmark-$stamp-$run_id.json"
install -m 0640 "$bench_result" "$durable_result"

curl -fsS -X POST "$admin_url/collect" >/dev/null
save_response=$(curl -fsS -X POST \
  "$admin_url/save?score=$score&pass=$pass")
saved_file=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["file"])' <<<"$save_response")

mkdir -p "$stage_dir"
find "$data_dir" -maxdepth 1 -type f \
  \( -name '*.json' -o -name '*.html' -o -name '*.pprof' -o -name '*.meta.json' \) \
  -exec cp -f -- {} "$stage_dir/" \;
sha256sum "$durable_result" "$data_dir/$saved_file"

trap - EXIT INT TERM
rm -f "$headers"
printf 'benchmark_score=%s\nbenchmark_pass=%s\nbenchmark_language=%s\n' "$score" "$pass" "$language"
printf 'saved_file=%s\nstage_dir=%s\n' "$saved_file" "$stage_dir"
