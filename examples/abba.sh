#!/bin/bash
# Release overhead gate. Runs at least three off/on/on/off blocks on one host,
# verifies the application fingerprint after every restart, records score,
# p95 and error rate, and gates on the upper bound of a paired 95% CI.
#
# BENCH must print a final JSON line:
#   {"score":123456,"p95_ms":42.1,"error_rate":0.001}
# FINGERPRINT must identify the exact deployed executable/image, for example:
#   FINGERPRINT='sha256sum ./app | cut -d" " -f1'
#
# Example:
#   RESTART='docker compose up -d --force-recreate app' \
#   WARMUP='./bench.sh --warmup' \
#   BENCH='./bench-json.sh' \
#   FINGERPRINT='docker inspect --format {{.Image}} private-isu-app-1' \
#   ./examples/abba.sh
set -euo pipefail

RESTART=${RESTART:?'set RESTART to restart the already-built application'}
WARMUP=${WARMUP:?'set WARMUP to a fixed warm-up command'}
BENCH=${BENCH:?'set BENCH to emit final score/p95_ms/error_rate JSON'}
FINGERPRINT=${FINGERPRINT:?'set FINGERPRINT to identify the deployed binary/image'}
ABBA_BLOCKS=${ABBA_BLOCKS:-3}
ABBA_OUTPUT=${ABBA_OUTPUT:-"abba-$(date -u +%Y%m%dT%H%M%SZ).tsv"}
SCORE_GATE_PCT=${SCORE_GATE_PCT:-2}
P95_GATE_PCT=${P95_GATE_PCT:-2}
ERROR_GATE=${ERROR_GATE:-0.001}

case "$ABBA_BLOCKS" in
  ''|*[!0-9]*) echo "ABBA_BLOCKS must be an integer >= 3" >&2; exit 2 ;;
esac
if [ "$ABBA_BLOCKS" -lt 3 ]; then
  echo "ABBA_BLOCKS must be >= 3 for a confidence interval" >&2
  exit 2
fi
if ! command -v jq >/dev/null && ! command -v python3 >/dev/null; then
  echo "jq or python3 is required" >&2
  exit 2
fi

json_number() {
  local field=$1
  local comparison=$2
  local input=$3
  if command -v jq >/dev/null; then
    if [ "$comparison" = positive ]; then
      jq -er --arg field "$field" '.[$field] | numbers | select(. > 0)' <<<"$input"
    else
      jq -er --arg field "$field" '.[$field] | numbers | select(. >= 0)' <<<"$input"
    fi
    return
  fi
  python3 -c '
import json, sys
field, comparison = sys.argv[1:]
value = json.load(sys.stdin).get(field)
valid_number = isinstance(value, (int, float)) and not isinstance(value, bool)
valid_bound = value > 0 if comparison == "positive" and valid_number else valid_number and value >= 0
if not valid_bound:
    raise SystemExit(1)
print(value)
' "$field" "$comparison" <<<"$input"
}

printf 'block\tposition\tmode\tscore\tp95_ms\terror_rate\tfingerprint\ttimestamp_utc\n' > "$ABBA_OUTPUT"
{
  printf 'timestamp_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf 'host=%s\n' "$(hostname)"
  printf 'uname=%s\n' "$(uname -a)"
  printf 'go=%s\n' "$(go version 2>/dev/null || echo unavailable)"
  printf 'git_head=%s\n' "$(git rev-parse HEAD 2>/dev/null || echo unavailable)"
  if [ -z "$(git status --porcelain --untracked-files=normal 2>/dev/null)" ]; then
    printf 'git_dirty=false\n'
  else
    printf 'git_dirty=true\n'
  fi
  printf 'blocks=%s score_gate_pct=%s p95_gate_pct=%s error_gate=%s\n' \
    "$ABBA_BLOCKS" "$SCORE_GATE_PCT" "$P95_GATE_PCT" "$ERROR_GATE"
} > "$ABBA_OUTPUT.meta"

declare -a MODES=(off on on off)
expected_fingerprint=''

for ((block = 1; block <= ABBA_BLOCKS; block++)); do
  for position in 0 1 2 3; do
    mode=${MODES[$position]}
    if [ "$mode" = off ]; then
      export ISUTOOLS=off
    else
      unset ISUTOOLS || true
    fi

    bash -c "$RESTART"
    fingerprint=$(bash -c "$FINGERPRINT" | head -n 1)
    if [ -z "$fingerprint" ]; then
      echo "FINGERPRINT returned an empty value" >&2
      exit 2
    fi
    case "$fingerprint" in
      *$'\t'*) echo "FINGERPRINT must not contain a tab" >&2; exit 2 ;;
    esac
    if [ -z "$expected_fingerprint" ]; then
      expected_fingerprint=$fingerprint
    elif [ "$fingerprint" != "$expected_fingerprint" ]; then
      echo "binary/image fingerprint changed: $expected_fingerprint -> $fingerprint" >&2
      exit 2
    fi

    bash -c "$WARMUP" >/dev/null
    result=$(bash -c "$BENCH")
    final_json=$(printf '%s\n' "$result" | tail -n 1)
    score=$(json_number score positive "$final_json")
    p95=$(json_number p95_ms positive "$final_json")
    error_rate=$(json_number error_rate nonnegative "$final_json")
    timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    printf '%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$block" "$((position + 1))" "$mode" "$score" "$p95" "$error_rate" \
      "$fingerprint" "$timestamp" | tee -a "$ABBA_OUTPUT"
  done
done

awk -F '\t' \
  -v score_gate="$SCORE_GATE_PCT" \
  -v p95_gate="$P95_GATE_PCT" \
  -v error_gate="$ERROR_GATE" '
function tcrit(n) {
  if (n == 3) return 4.303
  if (n == 4) return 3.182
  if (n == 5) return 2.776
  if (n <= 7) return 2.447
  if (n <= 10) return 2.262
  if (n <= 15) return 2.145
  if (n <= 30) return 2.045
  return 1.96
}
function report(label, sum, sumsq, n, gate, unit, mean, variance, half, upper) {
  mean = sum / n
  variance = n > 1 ? (sumsq - sum * sum / n) / (n - 1) : 0
  if (variance < 0) variance = 0
  half = tcrit(n) * sqrt(variance / n)
  upper = mean + half
  printf "%s: %.4f%s (95%% CI %.4f..%.4f%s; upper gate < %.4f%s)\n", label, mean, unit, mean-half, upper, unit, gate, unit
  if (upper >= gate) failed = 1
}
NR == 1 { next }
{
  block = $1
  if ($3 == "off") {
    off_score[block] += $4; off_p95[block] += $5; off_error[block] += $6; off_n[block]++
  } else {
    on_score[block] += $4; on_p95[block] += $5; on_error[block] += $6; on_n[block]++
  }
  if (block > blocks) blocks = block
}
END {
  for (b = 1; b <= blocks; b++) {
    os = off_score[b] / off_n[b]; ns = on_score[b] / on_n[b]
    op = off_p95[b] / off_n[b]; np = on_p95[b] / on_n[b]
    oe = off_error[b] / off_n[b]; ne = on_error[b] / on_n[b]
    score_effect = (os - ns) * 100 / os
    p95_effect = (np - op) * 100 / op
    error_effect = ne - oe
    ss += score_effect; ss2 += score_effect * score_effect
    ps += p95_effect; ps2 += p95_effect * p95_effect
    es += error_effect; es2 += error_effect * error_effect
  }
  report("score overhead", ss, ss2, blocks, score_gate, "%")
  report("p95 regression", ps, ps2, blocks, p95_gate, "%")
  report("error-rate delta", es, es2, blocks, error_gate, "")
  if (failed) { print "FAIL"; exit 1 }
  print "PASS"
}' "$ABBA_OUTPUT"

printf 'evidence: %s (provenance: %s.meta)\n' "$ABBA_OUTPUT" "$ABBA_OUTPUT"
