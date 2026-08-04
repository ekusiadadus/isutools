#!/bin/bash
set -euo pipefail

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

output="$work/results.tsv"
summary=$(
  RESTART=true \
  WARMUP=true \
  BENCH='printf '\''{"score":100000,"p95_ms":50,"error_rate":0}\n'\''' \
  FINGERPRINT='printf '\''same-binary\n'\''' \
  ABBA_BLOCKS=3 \
  ABBA_OUTPUT="$output" \
  bash "$(dirname "$0")/abba.sh"
)

test -s "$output"
test "$(wc -l < "$output")" -eq 13
grep -q 'same-binary' "$output"
grep -q 'score overhead.*95% CI' <<<"$summary"
grep -q 'PASS' <<<"$summary"
