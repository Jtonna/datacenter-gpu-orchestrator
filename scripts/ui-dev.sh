#!/usr/bin/env bash
# Start the UI development harness (cmd/devcluster): the real scheduler server
# against the fake fleet and fake llama helpers, plus the dev-only mock of the
# endpoints that are not built yet.
#
#   scripts/ui-dev.sh [scenario] [listen]
#
# Then open http://127.0.0.1:8790/ui/ . Scenarios: GET /dev/scenarios, switch
# with POST /dev/scenario?name=<name> (or pass one here). Stop with Ctrl-C.
set -euo pipefail
cd "$(dirname "$0")/.."
exec go run ./cmd/devcluster --listen "${2:-127.0.0.1:8790}" --scenario "${1:-demo}"
