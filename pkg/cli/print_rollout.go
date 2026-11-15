package cli

import (
	"fmt"
	"strconv"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// manualMessage starts the message of a cluster kept back because it
// upgrades by hand: it is the whole line (no hop the rollout would make).
const manualMessage = "manual upgrades:"

// rolloutLine is cluster get's Rollout line from the record the
// controller keeps (status.rollout): where the cluster is in its
// channel's rollout before its upgrade starts, then how the upgrade goes.
// "" when there is no record. A record the rollout has not started whose
// target the cluster already runs (running) says the cluster is up to
// date, not what it once waited for.
func rolloutLine(r *cloudv1.ClusterRollout, running string) string {
	if r == nil {
		return ""
	}
	switch r.GetResult() {
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING, cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED:
		if !minorBefore(running, r.GetToVersion()) && running != "" {
			return fmt.Sprintf("up to date with channel %s (%s)", r.GetChannel(), running)
		}
	}
	if r.GetResult() == cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED && strings.HasPrefix(r.GetMessage(), manualMessage) {
		return r.GetMessage()
	}
	hop := fmt.Sprintf("%s → %s via channel %s (tier %d)", r.GetFromVersion(), r.GetToVersion(), r.GetChannel(), r.GetTier())
	switch r.GetResult() {
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS:
		return hop + ": in progress since " + age(r.GetStartTime())
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_SUCCEEDED:
		return hop + ": succeeded " + age(r.GetFinishTime()) + " ago"
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_FAILED:
		return hop + ": failed: " + r.GetMessage()
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT:
		return hop + ": timed out: " + r.GetMessage()
	case cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED:
		return hop + ": blocked: " + r.GetMessage()
	default: // PENDING: what it waits for
		return hop + ": " + r.GetMessage()
	}
}

// minorBefore reports whether version a is of an older minor than b; false
// when either does not read as "1.<minor>".
func minorBefore(a, b string) bool {
	ma, okA := minorNumber(a)
	mb, okB := minorNumber(b)
	return okA && okB && ma < mb
}

func minorNumber(v string) (int, bool) {
	major, rest, ok := strings.Cut(v, ".")
	if !ok || major != "1" {
		return 0, false
	}
	minor, _, _ := strings.Cut(rest, ".")
	n, err := strconv.Atoi(minor)
	return n, err == nil
}
