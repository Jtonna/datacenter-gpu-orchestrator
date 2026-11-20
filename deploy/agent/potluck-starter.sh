#!/bin/sh
# potluck-starter.sh - fetch and start potluck-agent on a RAM-only node
# (docs/agent-contract.md section 5.4).
#
# Usage:  potluck-starter.sh          (no arguments)
#
# Settings, each read from the environment first, else from the file
# /etc/potluck/starter.env (lines NAME=value; blank lines and lines starting
# with # are ignored; the file is read, never sourced):
#   POTLUCK_URL                  required, for example http://10.0.0.1:3122
#   POTLUCK_MODEL_SOURCE         default share:<host part of POTLUCK_URL>:/
#   POTLUCK_WATCHDOG_FLOOR_MIB   default 1000
#
# Runs as root, with no TTY and possibly no HOME. Needs only a POSIX shell,
# curl, sha256sum, sed, head, uname, mkdir, chmod, mv, rm, sleep and
# systemd-run/systemctl. No jq or python: the two values it needs from
# GET /api/agents/release are read with sed; the layout that answer must keep
# is documented on agentproto.ReleaseInfo.
#
# Exit status:
#   0  potluck-agent.service is already active (or on its way up or down),
#      /etc/potluck/disabled exists, or the agent was started.
#   1  a local fault that waiting cannot fix: curl, sha256sum or systemd-run
#      missing, unsupported architecture, /dev/shm/potluck not writable,
#      systemd-run failed.
# While Potluck is unreachable, the settings are missing or a download or
# checksum fails, it waits (5 s growing to 60 s) and tries again, forever.
# It never stops a running agent or launch.

#
# TEST HOOKS, used ONLY by deploy/agent/starter_test.go; never set them on a
# node:
#   POTLUCK_STARTER_TEST_PATH  replaces PATH (the directory of fake tools)
#   POTLUCK_STARTER_TEST_ETC   replaces /etc/potluck
#   POTLUCK_STARTER_TEST_SHM   replaces /dev/shm/potluck

PATH=${POTLUCK_STARTER_TEST_PATH:-/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin}
export PATH
umask 022
set -u
# Nothing here wants pathname expansion.
set -f

ETC_DIR=${POTLUCK_STARTER_TEST_ETC:-/etc/potluck}
MARKER=$ETC_DIR/disabled
ENV_FILE=$ETC_DIR/starter.env
SHM_DIR=${POTLUCK_STARTER_TEST_SHM:-/dev/shm/potluck}
UNIT=potluck-agent.service
CR=$(printf '\r')

log() {
	printf 'potluck-starter: %s\n' "$*" >&2
}

# fatal: a local fault waiting cannot fix.
fatal() {
	log "error: $*"
	exit 1
}

# file_value NAME: prints the value of NAME from ENV_FILE (last one wins),
# without surrounding blanks or one pair of surrounding quotes.
file_value() {
	[ -r "$ENV_FILE" ] || return 0
	fv_out=""
	while IFS= read -r fv_line || [ -n "$fv_line" ]; do
		fv_line=${fv_line%"$CR"}
		case $fv_line in
			"$1="*) fv_out=${fv_line#"$1="} ;;
		esac
	done < "$ENV_FILE"
	# trim blanks
	while :; do
		case $fv_out in
			" "*) fv_out=${fv_out# } ;;
			*" ") fv_out=${fv_out% } ;;
			*) break ;;
		esac
	done
	case $fv_out in
		\"*\") fv_out=${fv_out#\"}; fv_out=${fv_out%\"} ;;
		\'*\') fv_out=${fv_out#\'}; fv_out=${fv_out%\'} ;;
	esac
	printf '%s' "$fv_out"
}

# setting NAME: the environment's value, else the file's.
setting() {
	eval "s_val=\${$1:-}"
	if [ -z "$s_val" ]; then
		s_val=$(file_value "$1")
	fi
	printf '%s' "$s_val"
}

# load_settings: sets URL, ADDR (host:port), HOST, MODEL_SOURCE, FLOOR.
# Returns 1 (and logs why) when they are missing or malformed; the caller
# waits and reads them again, so a fixed file is picked up without a reboot.
load_settings() {
	URL=$(setting POTLUCK_URL)
	if [ -z "$URL" ]; then
		log "POTLUCK_URL is not set (environment or $ENV_FILE)"
		return 1
	fi
	case $URL in
		http://*) ;;
		*) log "POTLUCK_URL must start with http://, got '$URL'"; return 1 ;;
	esac
	ls_rest=${URL#http://}
	ADDR=${ls_rest%%/*}
	case $ADDR in
		*[!A-Za-z0-9.:_-]* | "") log "POTLUCK_URL has an odd host part: '$URL'"; return 1 ;;
		*:*) ;;
		*) log "POTLUCK_URL needs a port, for example http://10.0.0.1:3122"; return 1 ;;
	esac
	HOST=${ADDR%:*}
	case $HOST in
		*[!0-9.]* | "") log "POTLUCK_URL host must be an IPv4 address (the agent takes <ip>:<port>), got '$HOST'"; return 1 ;;
	esac
	URL=http://$ADDR
	MODEL_SOURCE=$(setting POTLUCK_MODEL_SOURCE)
	[ -n "$MODEL_SOURCE" ] || MODEL_SOURCE="share:$HOST:/"
	FLOOR=$(setting POTLUCK_WATCHDOG_FLOOR_MIB)
	[ -n "$FLOOR" ] || FLOOR=1000
	case $FLOOR in
		*[!0-9]*) log "POTLUCK_WATCHDOG_FLOOR_MIB must be a number, got '$FLOOR'"; return 1 ;;
	esac
	case $MODEL_SOURCE in
		*[!A-Za-z0-9.:/_-]*) log "POTLUCK_MODEL_SOURCE has odd characters: '$MODEL_SOURCE'"; return 1 ;;
	esac
	return 0
}

check_tools() {
	for ct_tool in curl sha256sum sed head uname systemd-run systemctl; do
		command -v "$ct_tool" >/dev/null 2>&1 || fatal "$ct_tool is missing"
	done
}

resolve_platform() {
	case $(uname -s) in
		Linux) ;;
		*) fatal "unsupported operating system $(uname -s)" ;;
	esac
	case $(uname -m) in
		x86_64 | amd64) PLATFORM=linux-amd64 ;;
		aarch64 | arm64) PLATFORM=linux-arm64 ;;
		*) fatal "unsupported architecture $(uname -m)" ;;
	esac
}

check_disabled() {
	if [ -e "$MARKER" ]; then
		log "$MARKER exists, doing nothing"
		exit 0
	fi
}

# True when the agent unit is running or on its way up or down. A unit with
# Restart=always that is restarting reports "activating"; never interfere.
agent_running() {
	ar_state=$(systemctl is-active "$UNIT" 2>/dev/null)
	case $ar_state in
		active | activating | reloading | deactivating) return 0 ;;
	esac
	return 1
}

prepare_dir() {
	mkdir -p "$SHM_DIR" 2>/dev/null || fatal "cannot create $SHM_DIR"
	: > "$SHM_DIR/.write-test" 2>/dev/null || fatal "cannot write $SHM_DIR"
	rm -f "$SHM_DIR/.write-test"
}

# release_field JSON FIELD: the FIELD string of this platform's entry
# (layout rule on agentproto.ReleaseInfo).
release_field() {
	case $2 in
		sha256) rf_val='[0-9a-f]\{64\}' ;;
		*) rf_val='[^"]*' ;;
	esac
	printf '%s\n' "$1" |
		sed -n "s/.*\"$PLATFORM\"[[:space:]]*:[[:space:]]*{[^}]*\"$2\"[[:space:]]*:[[:space:]]*\"\\($rf_val\\)\".*/\\1/p" |
		head -n 1
}

# One attempt to download and verify the agent. Returns 0 when
# $SHM_DIR/potluck-agent is the verified binary, 1 on a fault that waiting
# may fix (Potluck down, bad answer, checksum mismatch).
fetch() {
	f_json=$(curl -fsS --connect-timeout 5 --max-time 20 "$URL/api/agents/release") || {
		log "Potluck at $URL not reachable (or GET /api/agents/release failed)"
		return 1
	}
	f_path=$(release_field "$f_json" path)
	f_want=$(release_field "$f_json" sha256)
	if [ -z "$f_path" ] || [ -z "$f_want" ]; then
		log "release answer has no agent (path and sha256) for $PLATFORM"
		return 1
	fi
	case $f_path in
		/*) ;;
		*) log "agent path is not absolute: $f_path"; return 1 ;;
	esac
	f_part=$SHM_DIR/potluck-agent.part
	rm -f "$f_part"
	if ! curl -fsS --connect-timeout 5 --max-time 600 -o "$f_part" "$URL$f_path"; then
		log "download of $f_path failed"
		rm -f "$f_part"
		return 1
	fi
	f_line=$(sha256sum "$f_part") || fatal "sha256sum failed on $f_part"
	set -- $f_line
	if [ "${1:-}" != "$f_want" ]; then
		log "checksum mismatch for $f_path (got ${1:-none}, want $f_want)"
		rm -f "$f_part"
		return 1
	fi
	chmod 755 "$f_part" || fatal "cannot chmod $f_part"
	mv -f "$f_part" "$SHM_DIR/potluck-agent" || fatal "cannot write $SHM_DIR/potluck-agent"
	return 0
}

start_agent() {
	# Another run may have started the unit while this one was downloading.
	check_disabled
	if agent_running; then
		log "$UNIT started meanwhile, doing nothing"
		exit 0
	fi
	# A failed transient unit of the same name blocks systemd-run.
	systemctl reset-failed "$UNIT" >/dev/null 2>&1
	log "starting $UNIT ($PLATFORM, Potluck $ADDR, model source $MODEL_SOURCE)"
	# A system-level transient unit of its own: not part of the unit (or
	# payload) that ran this script, so it outlives it.
	systemd-run --unit="$UNIT" --description="Potluck agent" \
		-p Restart=always -p RestartSec=5 \
		"$SHM_DIR/potluck-agent" --ram-only --potluck "$ADDR" \
		--model-source "$MODEL_SOURCE" --watchdog-floor-mib "$FLOOR" ||
		fatal "systemd-run failed"
	log "agent started"
	exit 0
}

main() {
	if [ $# -gt 0 ]; then
		log "arguments are ignored; settings come from the environment or $ENV_FILE"
	fi
	check_disabled
	if command -v systemctl >/dev/null 2>&1 && agent_running; then
		log "$UNIT is running, nothing to do"
		exit 0
	fi
	check_tools
	resolve_platform
	prepare_dir
	wait_s=5
	while :; do
		check_disabled
		if agent_running; then
			log "$UNIT is running, nothing to do"
			exit 0
		fi
		if load_settings && fetch; then
			start_agent
		fi
		log "retrying in ${wait_s}s"
		sleep "$wait_s"
		wait_s=$((wait_s * 2))
		[ "$wait_s" -le 60 ] || wait_s=60
	done
}

main "$@"
