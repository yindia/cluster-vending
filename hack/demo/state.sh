# The state of a demo run (bash), shared by the phase tapes
# (hack/demo/0*.tape) and hack/demo/run.sh: bin/demo/<run>.env holds what
# the phases have learned (the run id, tenant, region and zones, the
# account, network and cluster ids) as KEY=value lines, written by the
# phase that learns a value and read by the next. FINISHED=1 marks a run
# whose cleanup is done.

DEMO_STATE_DIR="${DEMO_STATE_DIR:-bin/demo}"

state_file() { printf '%s/%s.env\n' "$DEMO_STATE_DIR" "$1"; }

# demo_run prints the run to act on: DEMO_RUN when set; else the newest
# run that is not finished (so a phase re-run after a failure continues
# it); else a new id from the time (MMDDHHMM).
demo_run() {
	if [ -n "${DEMO_RUN:-}" ]; then
		printf '%s\n' "$DEMO_RUN"
		return
	fi
	local f
	# shellcheck disable=SC2045 # run ids have no spaces
	for f in $(ls -t "$DEMO_STATE_DIR"/*.env 2>/dev/null); do
		grep -qx 'FINISHED=1' "$f" && continue
		basename "$f" .env
		return
	done
	date +%m%d%H%M
}

# state_load run: the run's values into this shell (nothing when the run
# has no state yet).
state_load() {
	local f
	f="$(state_file "$1")"
	# shellcheck disable=SC1090
	[ -f "$f" ] && . "$f"
	return 0
}

# state_set key value: stores key=value in the run's ($RUN) state and sets
# it in this shell.
state_set() {
	local f tmp
	f="$(state_file "$RUN")"
	mkdir -p "$DEMO_STATE_DIR"
	tmp="$f.tmp"
	{ [ -f "$f" ] && grep -v "^$1=" "$f"; printf '%s=%q\n' "$1" "$2"; } >"$tmp"
	mv "$tmp" "$f"
	printf -v "$1" '%s' "$2"
	export "${1?}"
}
