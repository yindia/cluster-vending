# Helpers of hack/demo/demo.tape, sourced by its hidden setup (bash).
#
# Each step's visible command runs, then a wait_* helper follows it and
# prints __step_<n>_done__, or, when the command failed, the resource is
# FAILED or does not appear, or the wait times out, __step_<n>_failed__
# with the reason, also written to bin/demo-failed.txt. The tape then runs
# `check`: it prints __continue__ unless a step failed, so a failure stops
# the recording (vhs gives up waiting for __continue__) and `make demo`
# prints the reason.
#
# Needs: T (tenant), RUN, D (scratch directory), DEMO_REGION.

json() { cluster-vending "$@" --tenant "$T" -o json; }

fail() {
	local n=$1
	shift
	printf 'demo step %s failed: %s\n' "$n" "$*" >"$D/failed"
	cp "$D/failed" bin/demo-failed.txt
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

# until_ n seconds what cmd...: runs cmd every 15s until it succeeds (0),
# fails the step when it reports failure (2, its output the reason), or
# after seconds. cmd runs in this shell (not $(...)): the found_* checks
# set ACCOUNT, NETWORK and CLUSTER.
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
		sleep 15
	done
}

# id_of list jq-array name: the id of the object named name, or nothing.
id_of() { json "$1" list | jq -r --arg n "$3" ".$2[]? | select(.$4==\$n) | .id"; }

account_verified() {
	local a
	a="$(json account get --id "$ACCOUNT")" || return 1
	if jq -e '.account.status.access.verified' <<<"$a" >/dev/null; then return 0; fi
	if jq -e '.account.status.access.checkedTime and (.account.status.access.verified | not)' <<<"$a" >/dev/null; then
		jq -r '.account.status.access | "\(.reason): \(.message)"' <<<"$a"
		return 2
	fi
	return 1
}

# wait_account n status: account create (status: its exit code) onboarded
# demo-<run> and its access is verified.
wait_account() {
	[ "$2" -eq 0 ] || { fail "$1" "account create exited $2 (its output is above)"; return; }
	until_ "$1" 60 "the account demo-$RUN does not exist" found_account || return
	until_ "$1" 1200 "the access check of the account" account_verified || return
	done_ "$1"
}

found_account() {
	ACCOUNT="$(id_of account accounts "demo-$RUN" accountName)"
	[ -n "$ACCOUNT" ]
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
	NETWORK="$(id_of network networks "demo-$RUN-net" name)"
	[ -n "$NETWORK" ]
}

# wait_network n status: network create made demo-<run>-net and it is READY.
wait_network() {
	[ "$2" -eq 0 ] || { fail "$1" "network create exited $2 (its output is above)"; return; }
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
	CLUSTER="$(id_of cluster clusters "demo-$RUN-eks" name)"
	[ -n "$CLUSTER" ]
}

# wait_cluster n status: cluster create made demo-<run>-eks and it is RUNNING.
wait_cluster() {
	[ "$2" -eq 0 ] || { fail "$1" "cluster create exited $2 (its output is above)"; return; }
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
	jq -e '.cluster.status.upgrade.phase' <<<"$c" >/dev/null
}

upgraded() {
	local c
	c="$(json cluster get --id "$CLUSTER")" || return 1
	upgrade_failed "$c" && return 2
	jq -e '(.cluster.status.currentKubernetesVersion | startswith("1.37")) and (.cluster.status.upgrade == null)' <<<"$c" >/dev/null
}

# wait_upgrade_started n: the rollout started the cluster's upgrade (it
# waits for the cluster's tier and checks first).
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

gone() { ! json "$1" get --id "$2" >/dev/null 2>&1; }

# wait_gone n status kind id seconds: kind delete (status: its exit code)
# removed the object.
wait_gone() {
	[ "$2" -eq 0 ] || { fail "$1" "$3 delete exited $2 (its output is above)"; return; }
	until_ "$1" "$5" "the $3 $4 is still there" gone "$3" "$4" || return
	done_ "$1"
}

stack_gone() {
	! aws cloudformation describe-stacks --stack-name "$ROLE" --region "$DEMO_REGION" >/dev/null 2>&1
}

# wait_account_gone n status: account delete --delete-role removed the
# account and its role's stack.
wait_account_gone() {
	[ "$2" -eq 0 ] || { fail "$1" "account delete exited $2 (its output is above)"; return; }
	until_ "$1" 600 "the account $ACCOUNT is still there" gone account "$ACCOUNT" || return
	until_ "$1" 1200 "the stack $ROLE is still there" stack_gone || return
	done_ "$1"
}
