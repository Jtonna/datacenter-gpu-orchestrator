#!/bin/sh
# Usage: measure-needs.sh <base-url> <profile>     e.g. measure-needs.sh http://127.0.0.1:3122 gemma-bench-64k
# Design 7.2 "Measuring". <profile> should be a force_nodes profile (it skips the fit check)
# with the SAME model, ctx, kv_quant, flash_attn and parallel as the profile you are sizing.
# Loads it (POST /api/load), waits for ready, prints per-device MiB and the sum, then the
# needs_mib line to paste into the config. Leaves the model loaded: unload it afterwards.
# Env: WAIT_MAX_S (default 3600).
set -eu

[ $# -eq 2 ] || { echo "usage: $0 <base-url> <profile>" >&2; exit 2; }
BASE="${1%/}"; P="$2"
WAIT_MAX_S="${WAIT_MAX_S:-3600}"

# VERIFY against /api/status output: jq filter giving one TSV row per device of the instance
# for profile $p: node, model MiB, KV MiB, compute MiB. Host (CPU) buffers must NOT be included.
STATUS_DEVICE_TSV='.instances[]? | select(.profile == $p) | .devices[]? | [.node, .model_mib, .kv_mib, .compute_mib] | @tsv'
# VERIFY against /api/status output: the sum the scheduler itself reports (cross-check only).
STATUS_SUM_MIB='.instances[]? | select(.profile == $p) | .sum_mib'

have() { command -v "$1" >/dev/null 2>&1; }

echo "Requesting load of $P ..."
curl -fsS --max-time 10 -H 'Content-Type: application/json' -d "{\"model\":\"$P\"}" "$BASE/api/load"; echo

# Readiness from /running (shape fixed by design A.1), not from /api/status.
state() {
  curl -fsS --max-time 5 "$BASE/running" 2>/dev/null \
    | sed 's/},[ ]*{/}\n{/g' | grep "\"model\" *: *\"$P\"" | grep -o '"state" *: *"[a-z]*"' | head -1 \
    | sed 's/.*: *"//; s/"//' || true
}
T=0
while :; do
  S="$(state)"
  [ "$S" = "ready" ] && break
  if [ "$T" -ge "$WAIT_MAX_S" ]; then
    echo "timed out after ${WAIT_MAX_S}s (last state: ${S:-none}). See: docker logs llama-swap" >&2
    exit 1
  fi
  printf '\r  state=%s waited=%ss   ' "${S:-queued}" "$T"
  sleep 5; T=$((T + 5))
done
echo; echo "ready after about ${T}s"

STATUS="$(curl -fsS --max-time 5 "$BASE/api/status")"
if ! have jq; then
  echo "jq not found. Read the per-device model/KV/compute figures from this /api/status output"
  echo "and add them up by hand (host/CPU buffers excluded):"
  printf '%s\n' "$STATUS"
  exit 2
fi

ROWS="$(printf '%s' "$STATUS" | jq -r --arg p "$P" "$STATUS_DEVICE_TSV")"
if [ -z "$ROWS" ]; then
  echo "No device rows matched. Edit STATUS_DEVICE_TSV at the top of this script to match:" >&2
  printf '%s\n' "$STATUS" >&2
  exit 3
fi

echo
printf '%-28s %10s %10s %10s %10s\n' node model_MiB kv_MiB compute_MiB total_MiB
SUM="$(printf '%s\n' "$ROWS" | awk -F'\t' '
  { t=$2+$3+$4; sm+=$2; sk+=$3; sc+=$4; sum+=t
    printf "%-28s %10s %10s %10s %10d\n", $1, $2, $3, $4, t > "/dev/stderr" }
  END { printf "%-28s %10d %10d %10d %10d\n", "SUM", sm, sk, sc, sum > "/dev/stderr"
        print sum }')"
echo
echo "Scheduler-reported sum: $(printf '%s' "$STATUS" | jq -r --arg p "$P" "$STATUS_SUM_MIB" 2>/dev/null || echo n/a)  (should equal SUM above)"
echo
echo "Paste into the profile you are sizing (no margin: the measurement already includes compute buffers):"
echo "    needs_mib: $SUM"
echo
echo "Then unload:  curl -X POST $BASE/api/unload -H 'Content-Type: application/json' -d '{\"model\":\"$P\"}'"
