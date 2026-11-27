#!/bin/sh
# Usage: smoke.sh <base-url> [--load <model>]        e.g. smoke.sh http://127.0.0.1:3122
# Env:   CONFIG=/opt/llm-scheduler/seed-config.yaml  (the seed file: expected profile names are read from it; the live configuration is in the database)
# Without --load nothing is ever started (only /health, /v1/models, /running, POST /api/plan).
# With --load: one tiny chat completion, which DOES load the model.
set -eu

[ $# -ge 1 ] || { echo "usage: $0 <base-url> [--load <model>]" >&2; exit 2; }
BASE="${1%/}"; shift
LOAD_MODEL=""
if [ $# -ge 2 ] && [ "$1" = "--load" ]; then LOAD_MODEL="$2"; fi
CONFIG="${CONFIG:-/opt/llm-scheduler/seed-config.yaml}"
PROFILES="${TMPDIR:-/tmp}/smoke-profiles.$$"

# jq filter giving one TSV row per device (node, model MiB, KV MiB, compute MiB) for the
# instance of profile $p. Device buffers sit in .instances[].nodes[].memory[]; host (CPU)
# buffers are in .host_memory and are deliberately not read here.
STATUS_DEVICE_TSV='.instances[]? | select(.profile == $p) | .nodes[]? | .id as $n | .memory[]? | [$n, .model_mib, .kv_mib, .compute_mib] | @tsv'

FAILS=0
pass() { printf 'PASS  %s\n' "$*"; }
bad()  { printf 'FAIL  %s\n' "$*"; FAILS=$((FAILS + 1)); }
have() { command -v "$1" >/dev/null 2>&1; }

# Profile names from the config: 2-space-indented keys under 'profiles:'.
profiles() {
  awk '
    /^profiles:/ {inp=1; next}
    /^[^ #]/ {inp=0}
    inp && /^  [^ #]/ {
      s=$0; sub(/^  /,"",s)
      if (substr(s,1,1)=="\"") { s=substr(s,2); sub(/".*/,"",s) } else { sub(/:.*/,"",s) }
      print s
    }' "$CONFIG"
}

if [ -r "$CONFIG" ]; then
  profiles > "$PROFILES"
else
  echo "WARN  cannot read $CONFIG: no expected-profile list, /api/plan loop skipped" >&2
  : > "$PROFILES"
fi
trap 'rm -f "$PROFILES"' EXIT

echo "== /health"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$BASE/health" || true)"
if [ "$code" = "200" ]; then pass "/health 200"; else bad "/health returned '$code'"; fi

echo "== /v1/models lists the expected profiles"
MODELS_JSON="$(curl -fsS --max-time 5 "$BASE/v1/models" || true)"
[ -n "$MODELS_JSON" ] || bad "/v1/models gave no answer"
while IFS= read -r p; do
  if printf '%s' "$MODELS_JSON" | grep -q "\"id\" *: *\"$p\""; then pass "listed: $p"; else bad "not listed: $p"; fi
done < "$PROFILES"

echo "== /running shape"
R="$(curl -fsS --max-time 5 "$BASE/running" || true)"
if printf '%s' "$R" | grep -q '"running" *: *\['; then pass "/running has a running array: $R"; else bad "unexpected /running body: $R"; fi

echo "== POST /api/plan for every profile (dry run, changes nothing)"
while IFS= read -r p; do
  out="$(curl -sS --max-time 10 -H 'Content-Type: application/json' -d "{\"model\":\"$p\"}" "$BASE/api/plan" || echo 'no answer')"
  printf '%s -> %s\n' "$p" "$out"
done < "$PROFILES"

if [ -n "$LOAD_MODEL" ]; then
  echo "== one tiny chat completion on $LOAD_MODEL (this loads it; a cold load over a slow link can take many minutes)"
  START="$(date +%s)"
  RESP="$(curl -sS --max-time 3600 -H 'Content-Type: application/json' \
    -d "{\"model\":\"$LOAD_MODEL\",\"max_tokens\":32,\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in five words.\"}]}" \
    "$BASE/v1/chat/completions" || true)"
  END="$(date +%s)"
  printf '%s\n' "$RESP" | head -c 600; echo
  WALL=$((END - START))
  OUT_TOK="$(printf '%s' "$RESP" | grep -o '"completion_tokens" *: *[0-9]*' | head -1 | grep -o '[0-9]*$' || true)"
  TPS="$(printf '%s' "$RESP" | grep -o '"predicted_per_second" *: *[0-9.]*' | head -1 | grep -o '[0-9.]*$' || true)"
  echo "wall time (includes any load): ${WALL}s; completion_tokens: ${OUT_TOK:-n/a}"
  echo "tokens/s (server timings.predicted_per_second): ${TPS:-n/a, not reported}"
  echo "== per-device memory from /api/status (node, model, kv, compute in MiB)"
  if have jq; then
    curl -fsS --max-time 5 "$BASE/api/status" | jq -r --arg p "$LOAD_MODEL" "$STATUS_DEVICE_TSV" \
      || echo "jq expression failed: check STATUS_DEVICE_TSV against /api/status"
  else
    echo "jq not found; raw /api/status:"
    curl -fsS --max-time 5 "$BASE/api/status"; echo
  fi
fi

if [ "$FAILS" -eq 0 ]; then echo "SMOKE PASSED"; else echo "SMOKE FAILED: $FAILS"; exit 1; fi
