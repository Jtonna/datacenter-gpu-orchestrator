#!/bin/sh
# Usage: preflight-host.sh [config-path]    (default /opt/llm-scheduler/seed-config.yaml)
# Run on the host machine BEFORE cutover. Read-only: changes nothing, adds no firewall rule.
# Exit 0 = every check passed, 1 = at least one FAIL, 2 = cannot run.
set -eu

CONFIG="${1:-/opt/llm-scheduler/seed-config.yaml}"
MODELS_HOST_DIR="${MODELS_HOST_DIR:-/srv/llama-swap/models}"
BASE_IMAGE="${BASE_IMAGE:-llama-swap-whisper:rpc-330f018}"
FLEET_URL="${FLEET_URL:-http://127.0.0.1:8090}"
SUBNET="172.30.0.0/24"
TMP="${TMPDIR:-/tmp}/preflight-models.$$"

FAILS=0
ok()   { printf 'OK    %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; FAILS=$((FAILS + 1)); }
warn() { printf 'WARN  %s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

[ -r "$CONFIG" ] || { echo "cannot read config: $CONFIG" >&2; exit 2; }
have curl || { echo "curl is required" >&2; exit 2; }

echo "== 1. Docker and images"
if have docker; then ok "docker present ($(docker --version))"; else fail "docker not found"; fi
if have docker && docker image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
  ok "base image present: $BASE_IMAGE"
else
  fail "base image missing: $BASE_IMAGE (rebuild per apps/llama-swap/README.md in host-1-fleet-manager)"
fi
if have docker && docker image inspect llm-scheduler:local >/dev/null 2>&1; then
  ok "llm-scheduler:local is built"
else
  warn "llm-scheduler:local not built yet (runbook section B)"
fi

echo "== 2. Model files referenced by the config"
# kind: llama profiles: a 4-space-indented 'model:' key. kind: command: any /models/... path.
{
  sed -n 's/^    model: *//p' "$CONFIG" | sed 's/ *#.*//; s/^"//; s/"$//'
  grep -o '/models/[A-Za-z0-9_./-]*' "$CONFIG" | sed 's#^/models/##' || true
} | sort -u > "$TMP"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  if [ -f "$MODELS_HOST_DIR/$f" ]; then
    printf 'OK    %s\n' "$f"
  else
    printf 'FAIL  missing: %s/%s\n' "$MODELS_HOST_DIR" "$f"
    FAILS=$((FAILS + 1))
  fi
done < "$TMP"
rm -f "$TMP"

echo "== 3. Fleet console at $FLEET_URL"
FLEET_JSON="$(curl -fsS --max-time 5 "$FLEET_URL/api/fleet" 2>/dev/null || true)"
if [ -z "$FLEET_JSON" ]; then
  fail "no answer from $FLEET_URL/api/fleet"
else
  ok "fleet console answered"
  if printf '%s' "$FLEET_JSON" | grep -q '"members"'; then ok "response has members"; else fail "no \"members\" in response"; fi
  if printf '%s' "$FLEET_JSON" | grep -q '"ip" *:' && printf '%s' "$FLEET_JSON" | grep -q '"rpc_port" *:'; then
    ok "members carry ip and rpc_port fields (fleet-scheduler-fields version)"
  else
    fail "members have no ip/rpc_port fields: fleet manager is not on branch fleet-scheduler-fields"
  fi
  if printf '%s' "$FLEET_JSON" | grep -q '"ip" *: *null'; then
    warn "some members show ip: null (relayed nodes until hub patch hub-member-ip is live; node_overrides covers the Nvidia laptop)"
  fi
fi

echo "== 4. Host firewall: $SUBNET -> 8090"
RULE_CMD="sudo ufw allow from $SUBNET to any port 8090 proto tcp comment 'llm-scheduler to fleet console'"
UFW_OUT=""
if have ufw; then UFW_OUT="$(ufw status 2>/dev/null || sudo -n ufw status 2>/dev/null || true)"; fi
if [ -n "$UFW_OUT" ]; then
  if printf '%s\n' "$UFW_OUT" | grep -E '^8090(/tcp)? ' | grep -q '172\.30\.0\.0/24'; then
    ok "ufw rule for $SUBNET -> 8090 present"
  else
    fail "no ufw rule for $SUBNET -> 8090. Add it yourself with:"
    echo "      $RULE_CMD"
  fi
else
  warn "cannot read ufw status without sudo. Run: sudo ufw status | grep 8090"
  echo "      If no line mentions 172.30.0.0/24, add it with:"
  echo "      $RULE_CMD"
fi

echo "== 5. Remote nodes answer a TCP connect on their RPC port"
TARGETS="$(sed -n 's/.*rpc: *"\([0-9.]*:[0-9]*\)".*/\1/p' "$CONFIG")"
if have jq && [ -n "$FLEET_JSON" ]; then
  FT="$(printf '%s' "$FLEET_JSON" | jq -r '.members[]? | select(.online==true and .ip!=null and .rpc_port!=null) | "\(.ip):\(.rpc_port)"' 2>/dev/null || true)"
  TARGETS="$(printf '%s\n%s\n' "$TARGETS" "$FT")"
else
  warn "jq not found: only node_overrides are checked, not fleet members"
fi
TARGETS="$(printf '%s\n' "$TARGETS" | sed '/^$/d' | sort -u)"
if [ -z "$TARGETS" ]; then
  warn "no remote RPC targets found"
else
  for t in $TARGETS; do
    h="${t%:*}"; p="${t##*:}"
    # Connect-and-close only, like the hub's probe. It succeeds even if a head holds the worker.
    if have timeout && have bash && timeout 3 bash -c "exec 3<>/dev/tcp/$h/$p" 2>/dev/null; then
      ok "$t accepts connections"
    else
      fail "$t did not accept a TCP connect within 3 s (node off, or its firewall does not admit the host machine)"
    fi
  done
fi

echo
if [ "$FAILS" -eq 0 ]; then echo "PRE-FLIGHT PASSED"; else echo "PRE-FLIGHT FAILED: $FAILS check(s)"; exit 1; fi
