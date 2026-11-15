# Helpers of the demo's phase tapes (hack/demo/0*.tape, via setup.tape)
# and hack/demo/run.sh (bash).
#
# Each step's visible command runs, then a wait_* helper follows it and
# prints __step_<n>_done__, or, when the command failed, the resource is
# FAILED or does not appear, or the wait times out, __step_<n>_failed__
# with the reason, also written to bin/demo/<run>-<phase>-failed.txt. The
# tape then runs `check`: it prints __continue__ unless a step failed, so a
# failure stops the recording (vhs gives up waiting for __continue__) and
# run.sh prints the reason and the phase to run again.
#
# A phase run again resumes: what an earlier attempt made is found by its
# name and its id kept in the run's state (state.sh); a create the platform
# refuses because the object exists already is not a failure.

# shellcheck source=hack/demo/state.sh
. "$(dirname "${BASH_SOURCE[0]}")/state.sh"

# demo_init sets up the run (demo_run) for a phase: its state, the names
# derived from its id, and a scratch directory D. A value in the state
# wins over the environment, so every phase of a run uses the same.
demo_init() {
	RUN="$(demo_run)"
	export RUN DEMO_RUN="$RUN"
	state_load "$RUN"
	state_set DEMO_RUN "$RUN"
	state_set T "${T:-${DEMO_TENANT:-demo-$RUN}}"
	state_set DEMO_REGION "${DEMO_REGION:-us-west-1}"
	state_set DEMO_ZONE_A "${DEMO_ZONE_A:-usw1-az1}"
	state_set DEMO_ZONE_B "${DEMO_ZONE_B:-usw1-az3}"
	if [ -n "${DEMO_ACCOUNT_ID:-}" ]; then state_set DEMO_ACCOUNT_ID "$DEMO_ACCOUNT_ID"; fi
	export ROLE="cluster-vending-demo-$RUN-provisioner"
	D="$(mktemp -d)"
	export D
}

# demo_finish marks the run's cleanup done: a later phase starts a new run.
demo_finish() { state_set FINISHED 1; }

# failed_file is where the phase's failure is written.
failed_file() { printf '%s/%s-%s-failed.txt\n' "$DEMO_STATE_DIR" "$RUN" "${DEMO_PHASE:-demo}"; }

# fill name: hack/demo/<name> with the run's values, into $D/<name>.
fill() {
	sed -e "s/__RUN__/$RUN/g" -e "s/__TENANT__/$T/g" -e "s/__ACCOUNT_ID__/${DEMO_ACCOUNT_ID:-}/g" \
		-e "s/__BOOTSTRAP_ID__/${ACCOUNT:-}/g" -e "s/__NETWORK_ID__/${NETWORK:-}/g" -e "s/__REGION__/$DEMO_REGION/g" \
		-e "s/__ZONE_A__/$DEMO_ZONE_A/g" -e "s/__ZONE_B__/$DEMO_ZONE_B/g" "hack/demo/$1" >"$D/$1"
}

json() { cluster-vending "$@" --tenant "$T" -o json; }

fail() {
	local n=$1
	shift
	printf 'demo %s, %s step %s failed: %s\n' "$RUN" "${DEMO_PHASE:-demo}" "$n" "$*" >"$D/failed"
	mkdir -p "$DEMO_STATE_DIR"
	cp "$D/failed" "$(failed_file)"
	cat "$D/failed"
	echo "__step_${n}_failed__"
	return 1
}

done_() { echo "__step_$1_done__"; }

check() {
	if [ -f "$D/failed" ]; then
		cat "$D/failed"
	else
		echo "__continue__"
	fi
}

# until_ n seconds what cmd...: runs cmd every DEMO_POLL seconds (15) until
# it succeeds (0), fails the step when it reports failure (2, its output
# the reason), or after seconds. cmd runs in this shell (not $(...)): the
# found_* checks set ACCOUNT, NETWORK and CLUSTER.
until_() {
	local n=$1 limit=$2 what=$3 out rc
	shift 3
	local end=$((SECONDS + limit))
	while :; do
		"$@" >"$D/out" 2>&1
		rc=$?
		out="$(cat "$D/out")"
		case $rc in
		0) return 0 ;;
		2)
			fail "$n" "$what: $out"
			return 1
			;;
		esac
		if [ "$SECONDS" -ge "$end" ]; then
			fail "$n" "$what: not done after ${limit}s${out:+ ($out)}"
			return 1
		fi
		sleep "${DEMO_POLL:-15}"
	done
}

# id_of list jq-array name field: the id of the object whose field is
# name, or nothing.
id_of() { json "$1" list | jq -r --arg n "$3" ".$2[]? | select(.$4==\$n) | .id"; }

# account_verified: the account's access is verified, in DEMO_REGION too
# (where the network goes). A check that never covered the region asks
# for it once (account verify --region) and keeps waiting.
account_verified() {
	local a
	a="$(json account get --id "$ACCOUNT")" || return 1
	if jq -e --arg r "$DEMO_REGION" '.account.status.access.verified and
		any(.account.status.access.regions[]?; .region == $r and .ok)' <<<"$a" >/dev/null; then
		return 0
	fi
	if jq -e '.account.status.access.checkedTime and (.account.status.access.verified | not)' <<<"$a" >/dev/null; then
		jq -r '.account.status.access | "\(.reason): \(.message)"' <<<"$a"
		return 2
	fi
	if [ -z "${_DEMO_REGION_ASKED:-}" ] && jq -e '.account.status.access.verified' <<<"$a" >/dev/null; then
		_DEMO_REGION_ASKED=1
		cluster-vending account verify --id "$ACCOUNT" --tenant "$T" --region "$DEMO_REGION" >/dev/null 2>&1 || true
	fi
	return 1
}

# ensure_region n: the account is verified in DEMO_REGION before anything
# is created there; prints __region_ok__ or fails step n.
ensure_region() {
	until_ "$1" 900 "the access check of the account in $DEMO_REGION" account_verified && echo __region_ok__
}

found_account() {
	local id
	id="$(id_of account accounts "demo-$RUN" accountName)"
	[ -n "$id" ] && state_set ACCOUNT "$id"
}

# wait_account n status: account create (status: its exit code) onboarded
# demo-<run> and its access is verified. A create that failed is fine when
# the account exists: an earlier attempt of the phase made it.
wait_account() {
	if [ "$2" -ne 0 ] && ! found_account; then
		fail "$1" "account create exited $2 (its output is above)"
		return
	fi
	until_ "$1" 60 "the account demo-$RUN does not exist" found_account || return
	until_ "$1" 1200 "the access check of the account" account_verified || return
	done_ "$1"
}

network_ready() {
	local n
	n="$(json network get --id "$NETWORK" --live)" || return 1
	case "$(jq -r '.network.status.state' <<<"$n")" in
	NETWORK_STATE_READY) return 0 ;;
	NETWORK_STATE_FAILED)
		jq -r '.network.status.message // "FAILED"' <<<"$n"
		return 2
		;;
	esac
	return 1
}

found_network() {
	local id
	id="$(id_of network networks "demo-$RUN-net" name)"
	[ -n "$id" ] && state_set NETWORK "$id"
}

# wait_network n status: network create made demo-<run>-net and it is
# READY (or an earlier attempt made it).
wait_network() {
	if [ "$2" -ne 0 ] && ! found_network; then
		fail "$1" "network create exited $2 (its output is above)"
		return
	fi
	until_ "$1" 120 "the network demo-$RUN-net does not exist" found_network || return
	until_ "$1" 2400 "the network demo-$RUN-net" network_ready || return
	done_ "$1"
}

cluster_running() {
	local c
	c="$(json cluster get --id "$CLUSTER" --live)" || return 1
	case "$(jq -r '.cluster.status.state' <<<"$c")" in
	CLUSTER_STATE_RUNNING) return 0 ;;
	CLUSTER_STATE_FAILED)
		jq -r '.cluster.status.message // "FAILED"' <<<"$c"
		return 2
		;;
	esac
	return 1
}

found_cluster() {
	local id
	id="$(id_of cluster clusters "demo-$RUN-eks" name)"
	[ -n "$id" ] && state_set CLUSTER "$id"
}

# wait_cluster n status: cluster create made demo-<run>-eks and it is
# RUNNING (or an earlier attempt made it).
wait_cluster() {
	if [ "$2" -ne 0 ] && ! found_cluster; then
		fail "$1" "cluster create exited $2 (its output is above)"
		return
	fi
	until_ "$1" 120 "the cluster demo-$RUN-eks does not exist" found_cluster || return
	until_ "$1" 4500 "the cluster demo-$RUN-eks" cluster_running || return
	done_ "$1"
}

upgrade_failed() {
	local c=$1
	if jq -e '.cluster.status.rollout.result | IN("CLUSTER_ROLLOUT_RESULT_FAILED", "CLUSTER_ROLLOUT_RESULT_TIMED_OUT")' <<<"$c" >/dev/null; then
		jq -r '.cluster.status.rollout | "\(.result): \(.message // "")"' <<<"$c"
		return 0
	fi
	return 1
}

upgrade_started() {
	local c
	c="$(json cluster get --id "$CLUSTER")" || return 1
	upgrade_failed "$c" && return 2
	jq -e '.cluster.status.upgrade.phase or (.cluster.status.currentKubernetesVersion | startswith("1.37"))' <<<"$c" >/dev/null
}

upgraded() {
	local c
	c="$(json cluster get --id "$CLUSTER")" || return 1
	upgrade_failed "$c" && return 2
	jq -e '(.cluster.status.currentKubernetesVersion | startswith("1.37")) and (.cluster.status.upgrade == null)' <<<"$c" >/dev/null
}

# wait_upgrade_started n: the rollout started the cluster's upgrade (it
# waits for the cluster's tier and checks first), or it is done already.
wait_upgrade_started() {
	until_ "$1" 1800 "the automatic upgrade of demo-$RUN-eks did not start (admin rollout get --channel alpha says why)" upgrade_started || return
	done_ "$1"
}

# wait_upgraded n: the cluster runs 1.37, control plane, node groups and
# add-ons.
wait_upgraded() {
	until_ "$1" 7200 "the upgrade of demo-$RUN-eks to 1.37" upgraded || return
	done_ "$1"
}

# gone kind id: the object does not exist (no id: it was never made).
gone() { [ -z "$2" ] || ! json "$1" get --id "$2" >/dev/null 2>&1; }

# wait_gone n status kind id seconds: kind delete (status: its exit code)
# removed the object. One gone already (an earlier attempt, or never made)
# is done.
wait_gone() {
	if gone "$3" "$4"; then
		done_ "$1"
		return
	fi
	[ "$2" -eq 0 ] || { fail "$1" "$3 delete exited $2 (its output is above)"; return; }
	until_ "$1" "$5" "the $3 $4 is still there" gone "$3" "$4" || return
	done_ "$1"
}

stack_gone() {
	! aws cloudformation describe-stacks --stack-name "$ROLE" --region "$DEMO_REGION" >/dev/null 2>&1
}

# wait_account_gone n status: account delete --delete-role removed the
# account and its role's stack (or they are gone already).
wait_account_gone() {
	if gone account "${ACCOUNT:-}" && stack_gone; then
		done_ "$1"
		return
	fi
	if [ "$2" -ne 0 ] && ! gone account "${ACCOUNT:-}"; then
		fail "$1" "account delete exited $2 (its output is above)"
		return
	fi
	until_ "$1" 600 "the account $ACCOUNT is still there" gone account "${ACCOUNT:-}" || return
	until_ "$1" 1200 "the stack $ROLE is still there" stack_gone || return
	done_ "$1"
}

# phase_done phase: the phase's result is there already, so run.sh skips
# it: the account verified, the network READY, the cluster RUNNING, the
# cluster upgraded to 1.37, the run's cleanup finished.
phase_done() {
	case $1 in
	account) [ -n "${ACCOUNT:-}" ] && account_verified >/dev/null 2>&1 ;;
	network) [ -n "${NETWORK:-}" ] && network_ready >/dev/null 2>&1 ;;
	cluster) [ -n "${CLUSTER:-}" ] && cluster_running >/dev/null 2>&1 ;;
	upgrade) [ -n "${CLUSTER:-}" ] && upgraded >/dev/null 2>&1 ;;
	cleanup) [ "${FINISHED:-}" = 1 ] ;;
	*) return 1 ;;
	esac
}

# phase_needs phase: the make target to run first when the phase lacks
# what an earlier one learns; nothing when it can run.
phase_needs() {
	case $1 in
	network) [ -n "${ACCOUNT:-}" ] || echo demo-account ;;
	cluster) [ -n "${NETWORK:-}" ] || echo demo-network ;;
	upgrade) [ -n "${CLUSTER:-}" ] || echo demo-cluster ;;
	esac
	return 0
}
