#!/bin/sh
# potluck-starter.sh - fetch and start potluck-agent on a RAM-only node
# (docs/agent-contract.md section 5.4).
#
# Usage:  potluck-starter.sh [addr:port]
#
# The Potluck address comes from, in this order:
#   1. the first argument,
#   2. the environment variable POTLUCK_ADDR,
#   3. the file "potluck.addr" in the directory of this script (one line).
#
# Extra agent options can be given in the environment variable
# POTLUCK_AGENT_ARGS. It is split into words on purpose (no quoting is
# possible inside it) and appended to the agent's command line, for example:
#   POTLUCK_AGENT_ARGS="--watchdog-floor-mib 1500 --name console-5"
#
# Needs only: a POSIX shell, curl, sha256sum, systemctl/systemd-run and
# mkdir, chmod, mv, rm, sleep. No jq, python, awk, sed or grep.
#
# Safe to run on every payload apply: it does nothing while the marker
# /etc/potluck/disabled exists or while potluck-agent.service is running.
# It never stops a running agent or launch.

set -eu
# Pathname expansion is never wanted here; POTLUCK_AGENT_ARGS is split into
# words below and must not be globbed.
set -f

MARKER=/etc/potluck/disabled
SHM_DIR=/dev/shm/potluck
UNIT=potluck-agent.service

NL='
'
TAB=$(printf '\t')
CR=$(printf '\r')

log() {
	printf 'potluck-starter: %s\n' "$*" >&2
}

die() {
	log "error: $*"
	exit 2
}

# json_obj TEXT KEY
# Sets JOBJ to the text between the first "{" after "KEY" and the next "}".
# Fine for a flat object such as {"path":"...","sha256":"..."}.
json_obj() {
	jo_pat="*\"$2\""
	jo_rest=${1#$jo_pat}
	[ "$jo_rest" != "$1" ] || return 1
	jo_open='{'
	jo_rest=${jo_rest#*"$jo_open"}
	jo_close='}'
	JOBJ=${jo_rest%%"$jo_close"*}
	[ "$JOBJ" != "$jo_rest" ] || return 1
	return 0
}

# json_str TEXT KEY
# Sets JVAL to the string value of "KEY": "..." (first occurrence in TEXT),
# whatever the whitespace around the colon. Does not handle escaped quotes
# inside the value; paths and hex digests have none.
json_str() {
	js_pat="*\"$2\""
	js_rest=${1#$js_pat}
	[ "$js_rest" != "$1" ] || return 1
	js_rest=${js_rest#*:}
	while :; do
		case $js_rest in
			" "*|"$TAB"*|"$NL"*|"$CR"*) js_rest=${js_rest#?} ;;
			*) break ;;
		esac
	done
	case $js_rest in
		\"*) ;;
		*) return 1 ;;
	esac
	js_rest=${js_rest#\"}
	JVAL=${js_rest%%\"*}
	[ -n "$JVAL" ] || return 1
	return 0
}

resolve_addr() {
	ADDR=""
	if [ $# -ge 1 ] && [ -n "$1" ]; then
		ADDR=$1
	elif [ -n "${POTLUCK_ADDR:-}" ]; then
		ADDR=$POTLUCK_ADDR
	else
		case $0 in
			*/*) ra_dir=${0%/*} ;;
			*) ra_dir=. ;;
		esac
		if [ -r "$ra_dir/potluck.addr" ]; then
			ra_line=""
			read -r ra_line < "$ra_dir/potluck.addr" || true
			ADDR=$ra_line
		fi
	fi
	# strip a trailing CR and surrounding blanks
	ADDR=${ADDR%"$CR"}
	while :; do
		case $ADDR in
			" "*|"$TAB"*) ADDR=${ADDR#?} ;;
			*" "|*"$TAB") ADDR=${ADDR%?} ;;
			*) break ;;
		esac
	done
	[ -n "$ADDR" ] || die "no Potluck address (argument, POTLUCK_ADDR or potluck.addr)"
	case $ADDR in
		*:*) ;;
		*) die "Potluck address must be addr:port, got '$ADDR'" ;;
	esac
	case $ADDR in
		*[!A-Za-z0-9.:_-]*) die "Potluck address has odd characters: '$ADDR'" ;;
	esac
	POTLUCK_HOST=${ADDR%:*}
}

resolve_platform() {
	rp_os=$(uname -s)
	case $rp_os in
		Linux) rp_os=linux ;;
		Darwin) rp_os=darwin ;;
	esac
	rp_machine=$(uname -m)
	case $rp_machine in
		x86_64|amd64) rp_arch=amd64 ;;
		aarch64|arm64) rp_arch=arm64 ;;
		*) rp_arch=$rp_machine ;;
	esac
	PLATFORM="$rp_os-$rp_arch"
}

# Exits 0 when the marker exists.
check_disabled() {
	if [ -e "$MARKER" ]; then
		log "$MARKER exists, doing nothing"
		exit 0
	fi
}

# True when the agent unit is running or on its way up or down. A unit with
# Restart=always that is restarting reports "activating"; systemd-run would
# fail on it, and we must never interfere.
agent_running() {
	ar_state=$(systemctl is-active "$UNIT" 2>/dev/null || true)
	case $ar_state in
		active|activating|reloading|deactivating) return 0 ;;
	esac
	return 1
}

# One attempt. Returns 0 when the agent unit was started, 1 on any failure.
attempt() {
	at_json=$(curl -fsS --connect-timeout 5 --max-time 20 "http://$ADDR/api/agents/release") || {
		log "Potluck at $ADDR not reachable (or release request failed)"
		return 1
	}
	json_obj "$at_json" "$PLATFORM" || {
		log "release answer has no agent for $PLATFORM"
		return 1
	}
	at_obj=$JOBJ
	json_str "$at_obj" path || {
		log "release answer has no path for $PLATFORM"
		return 1
	}
	at_path=$JVAL
	json_str "$at_obj" sha256 || {
		log "release answer has no sha256 for $PLATFORM"
		return 1
	}
	at_want=$JVAL
	case $at_path in
		/*) ;;
		*) log "agent path is not absolute: $at_path"; return 1 ;;
	esac

	mkdir -p "$SHM_DIR" || { log "cannot create $SHM_DIR"; return 1; }
	at_part=$SHM_DIR/potluck-agent.part
	rm -f "$at_part"
	curl -fsS --connect-timeout 5 --max-time 600 -o "$at_part" "http://$ADDR$at_path" || {
		log "download of $at_path failed"
		rm -f "$at_part"
		return 1
	}
	at_line=$(sha256sum "$at_part") || {
		log "sha256sum failed"
		rm -f "$at_part"
		return 1
	}
	# first word of the output is the digest
	set -- $at_line
	at_got=$1
	if [ "$at_got" != "$at_want" ]; then
		log "checksum mismatch (got $at_got, want $at_want)"
		rm -f "$at_part"
		return 1
	fi
	chmod +x "$at_part" || { log "chmod failed"; rm -f "$at_part"; return 1; }
	mv -f "$at_part" "$SHM_DIR/potluck-agent" || { log "rename failed"; return 1; }

	# Re-check just before launching: another run may have started the unit
	# while we were downloading.
	check_disabled
	if agent_running; then
		log "$UNIT started meanwhile, doing nothing"
		exit 0
	fi

	systemctl reset-failed "$UNIT" >/dev/null 2>&1 || true
	log "starting $UNIT ($PLATFORM, Potluck $ADDR)"
	# POTLUCK_AGENT_ARGS is intentionally unquoted: split into words, no globbing.
	# shellcheck disable=SC2086
	systemd-run --unit=potluck-agent --description="Potluck agent" \
		-p Restart=always -p RestartSec=5 \
		"$SHM_DIR/potluck-agent" --potluck "$ADDR" --ram-only \
		--model-source "share:$POTLUCK_HOST:/" ${POTLUCK_AGENT_ARGS:-} || {
		log "systemd-run failed"
		return 1
	}
	return 0
}

main() {
	resolve_addr "$@"
	resolve_platform
	wait_s=5
	while :; do
		check_disabled
		if agent_running; then
			log "$UNIT is running, nothing to do"
			exit 0
		fi
		if attempt; then
			log "agent started"
			exit 0
		fi
		log "retrying in ${wait_s}s"
		sleep "$wait_s"
		wait_s=$((wait_s * 2))
		[ "$wait_s" -le 60 ] || wait_s=60
	done
}

main "$@"
