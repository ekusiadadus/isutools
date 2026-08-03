#!/bin/bash
# ABBA overhead gate: same binary, same host, alternating measurement
# off/on/on/off. The score difference between the paired off and on runs is
# the measurement overhead; the release criterion is < 2%.
#
# Usage (adapt RESTART/BENCH to your environment):
#   RESTART="docker compose up -d app" \
#   BENCH="./bench.sh"                 \
#   TOGGLE_ENV=ISUTOOLS               \
#   ./abba.sh
set -euo pipefail

RESTART=${RESTART:?"set RESTART to the command that restarts the app with \$ISUTOOLS in its environment"}
BENCH=${BENCH:?"set BENCH to the benchmark command that prints a final score"}

declare -a MODES=(off on on off)
declare -a SCORES=()

for mode in "${MODES[@]}"; do
  if [ "$mode" = off ]; then export ISUTOOLS=off; else unset ISUTOOLS || true; fi
  bash -c "$RESTART"
  sleep 5
  score=$(bash -c "$BENCH" | grep -oE '"score":[0-9]+' | tail -1 | cut -d: -f2)
  echo "mode=$mode score=$score"
  SCORES+=("$score")
done

off_avg=$(( (SCORES[0] + SCORES[3]) / 2 ))
on_avg=$(( (SCORES[1] + SCORES[2]) / 2 ))
echo "off avg: $off_avg / on avg: $on_avg"
awk -v off="$off_avg" -v on="$on_avg" 'BEGIN {
  overhead = (off - on) * 100.0 / off
  printf "overhead: %.2f%% (gate: < 2%%)\n", overhead
  exit (overhead >= 2.0)
}'
