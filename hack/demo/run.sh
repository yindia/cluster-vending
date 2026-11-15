#!/usr/bin/env bash
# Runs the demo's phases (make demo-<phase>, make demo):
#
#   hack/demo/run.sh account|network|cluster|upgrade|cleanup   one phase
#   hack/demo/run.sh all                                       every phase, in order
#   hack/demo/run.sh status                                    the run's state, and each piece now
#   hack/demo/run.sh video                                     the run's phase videos, joined
#
# A phase whose result is there already is skipped. A phase that fails
# stops (all stops at it), says why and which `make demo-<phase>` to run
# again: running it again resumes the same run (DEMO_RUN, or the newest
# unfinished run).
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
export PATH="$PWD/bin:$PATH"
# shellcheck source=hack/demo/lib.sh
. hack/demo/lib.sh

PHASES=(account network cluster upgrade cleanup)

tape_of() {
	case $1 in
	account) echo 01-account ;;
	network) echo 02-network ;;
	cluster) echo 03-cluster ;;
	upgrade) echo 04-upgrade ;;
	cleanup) echo 05-cleanup ;;
	*) return 1 ;;
	esac
}

# need_aws: credentials of DEMO_ACCOUNT_ID (the account and cleanup phases
# create and delete the role's CloudFormation stack with them).
need_aws() {
	if [ -z "${DEMO_ACCOUNT_ID:-}" ]; then
		echo "export DEMO_ACCOUNT_ID=<12-digit account> first" >&2
		return 1
	fi
	local got
	got="$(aws sts get-caller-identity --query Account --output text 2>/dev/null)"
	if [ "$got" != "$DEMO_ACCOUNT_ID" ]; then
		echo "the AWS credentials are not of account $DEMO_ACCOUNT_ID (export them, or AWS_PROFILE)" >&2
		return 1
	fi
}

# show phase: what a skipped phase has (not recorded).
show() {
	case $1 in
	account) cluster-vending account get --id "$ACCOUNT" --tenant "$T" ;;
	network) cluster-vending network get --id "$NETWORK" --tenant "$T" | head -8 ;;
	cluster | upgrade) cluster-vending cluster get --id "$CLUSTER" --tenant "$T" | grep -E '^(Name|Phase|Version|Rollout|Upgrade):' ;;
	cleanup) echo "demo $RUN: cleaned up" ;;
	esac
}

run_phase() {
	local p=$1 tape need rc failed
	tape="$(tape_of "$p")" || {
		echo "unknown phase $p: one of ${PHASES[*]}" >&2
		return 2
	}
	demo_init
	export DEMO_PHASE="$p"
	failed="$(failed_file)"
	if phase_done "$p"; then
		echo "demo $RUN: $p is done already; skipped"
		show "$p"
		return 0
	fi
	need="$(phase_needs "$p")"
	if [ -n "$need" ]; then
		echo "demo $RUN: $p needs what make $need makes; run it first (DEMO_RUN=$RUN)" >&2
		return 1
	fi
	case $p in account | cleanup) need_aws || return 1 ;; esac
	rm -f "$failed"
	mkdir -p "$DEMO_STATE_DIR"
	echo "demo $RUN: recording $p (hack/demo/$tape.tape)"
	vhs "hack/demo/$tape.tape"
	rc=$?
	local ext
	for ext in gif mp4; do
		if [ -f "$DEMO_STATE_DIR/$tape.$ext" ]; then mv "$DEMO_STATE_DIR/$tape.$ext" "$DEMO_STATE_DIR/$RUN-$p.$ext"; fi
	done
	if [ "$rc" -ne 0 ] || [ -f "$failed" ]; then
		[ -f "$failed" ] && cat "$failed" >&2
		echo "demo $RUN: $p stopped; run it again with: make demo-$p DEMO_RUN=$RUN" >&2
		return 1
	fi
	echo "demo $RUN: $p done: $DEMO_STATE_DIR/$RUN-$p.gif"
}

status() {
	RUN="$(demo_run)"
	if [ ! -f "$(state_file "$RUN")" ]; then
		echo "no demo run yet (bin/demo is empty)"
		return 0
	fi
	echo "demo $RUN ($(state_file "$RUN")):"
	sed 's/^/  /' "$(state_file "$RUN")"
	state_load "$RUN"
	local p
	for p in "${PHASES[@]}"; do
		if phase_done "$p" 2>/dev/null; then echo "  $p: done"; else echo "  $p: not done"; fi
	done
	[ -n "${ACCOUNT:-}" ] && cluster-vending account get --id "$ACCOUNT" --tenant "$T" 2>&1 | head -6
	[ -n "${NETWORK:-}" ] && cluster-vending network get --id "$NETWORK" --tenant "$T" 2>&1 | grep -E '^(Name|Phase):'
	[ -n "${CLUSTER:-}" ] && cluster-vending cluster get --id "$CLUSTER" --tenant "$T" 2>&1 | grep -E '^(Name|Phase|Version|Rollout|Upgrade):'
	return 0
}

video() {
	RUN="$(demo_run)"
	if ! command -v ffmpeg >/dev/null; then
		echo "ffmpeg is not installed: the phase videos stay apart ($DEMO_STATE_DIR/$RUN-<phase>.mp4)"
		return 0
	fi
	local list="$DEMO_STATE_DIR/$RUN-videos.txt" p n=0
	: >"$list"
	for p in "${PHASES[@]}"; do
		if [ -f "$DEMO_STATE_DIR/$RUN-$p.mp4" ]; then
			echo "file '$RUN-$p.mp4'" >>"$list"
			n=$((n + 1))
		fi
	done
	if [ "$n" -eq 0 ]; then
		echo "demo $RUN has no phase videos yet" >&2
		return 1
	fi
	ffmpeg -loglevel error -y -f concat -safe 0 -i "$list" -c copy "$DEMO_STATE_DIR/$RUN.mp4" &&
		echo "demo $RUN: $DEMO_STATE_DIR/$RUN.mp4 ($n phases)"
}

case "${1:-}" in
all)
	for p in "${PHASES[@]}"; do
		run_phase "$p" || exit 1
		export DEMO_RUN="$RUN"
	done
	;;
status) status ;;
video) video ;;
account | network | cluster | upgrade | cleanup) run_phase "$1" ;;
*)
	echo "usage: $0 account|network|cluster|upgrade|cleanup|all|status|video" >&2
	exit 2
	;;
esac
