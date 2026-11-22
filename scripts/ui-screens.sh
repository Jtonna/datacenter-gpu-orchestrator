#!/usr/bin/env bash
# Take one screenshot of the web UI with a headless browser. It starts nothing:
# the dev harness (scripts/ui-dev.sh) must be running.
#
#   scripts/ui-screens.sh <base-url> <route> <scenario> <size> <out.png> [wait-ms]
#
#   base-url  http://127.0.0.1:8790
#   route     the hash route, for example models or benchmarks/runs/r-12?tab=steps
#   scenario  a dev harness scenario, or - to leave the current one
#   size      desktop (1440x1100), phone (390x844), or WIDTHxHEIGHT (a tall
#             height captures a long page: 1440x2200, 390x2600)
#   wait-ms   how long the page may run before the picture is taken (default 5000)
#
# Example:
#   scripts/ui-screens.sh http://127.0.0.1:8790 models loading desktop models-loading-desktop.png
#
# Look at the picture afterwards. A picture that shows only grey placeholder
# bars was taken before the data arrived: take it again.
set -euo pipefail
base="${1:?base url}"; route="${2:?route}"; scenario="${3:?scenario or -}"; size="${4:?size}"; out="${5:?output png}"; wait="${6:-5000}"
case "$size" in
  desktop) size=1440x1100 ;;
  phone) size=390x844 ;;
esac
w="${size%x*}"; h="${size#*x}"
if [ "$scenario" != "-" ]; then
  curl -fsS -X POST "$base/dev/scenario?name=$scenario" >/dev/null
  sleep 1 # let the next status frame carry the new state
fi
browser="${UI_BROWSER:-}"
if [ -z "$browser" ]; then
  for c in "/c/Program Files/Google/Chrome/Application/chrome.exe" "/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe" \
           "$(command -v google-chrome || true)" "$(command -v chromium || true)" "$(command -v chromium-browser || true)"; do
    if [ -n "$c" ] && [ -x "$c" ]; then browser="$c"; break; fi
  done
fi
[ -n "$browser" ] || { echo "no Chrome or Edge found; set UI_BROWSER" >&2; exit 1; }
mkdir -p "$(dirname "$out")"
case "$out" in /*|?:*) abs="$out" ;; *) abs="$PWD/$out" ;; esac
if command -v cygpath >/dev/null 2>&1; then abs="$(cygpath -w "$abs")"; fi
url="$base/ui/#/$route"
# Desktop Chrome will not lay a window out narrower than about 500 px, so a
# phone-width picture is taken through the harness's frame page: the UI inside
# it sees a real viewport of the asked width.
if [ "$w" -lt 510 ]; then
  enc="$(printf '%s' "$route" | sed -e 's/%/%25/g' -e 's/?/%3F/g' -e 's/&/%26/g' -e 's/=/%3D/g' -e 's/#/%23/g')"
  url="$base/dev/frame?w=$w&h=$h&route=$enc"
fi
profile="$(mktemp -d)"
"$browser" --headless=new --disable-gpu --hide-scrollbars --no-first-run --no-default-browser-check \
  --user-data-dir="$profile" --window-size="$w,$h" --virtual-time-budget="$wait" --screenshot="$abs" \
  "$url" >/dev/null 2>&1 || true
rm -rf "$profile" 2>/dev/null || true
[ -s "$out" ] && echo "$out" || { echo "no screenshot written: $out" >&2; exit 1; }
