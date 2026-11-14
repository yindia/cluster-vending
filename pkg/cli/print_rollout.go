package cli

import (
	"fmt"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// manualMessage starts the message of a cluster kept back because it
// upgrades by hand: it is the whole line (no hop the rollout would make).
const manualMessage = "manual upgrades:"

// rolloutLine is cluster get's Rollout line from the record the
// controller keeps (status.rollout): where the cluster is in its
// channel's rollout before its upgrade starts, then how the upgrade goes.
// "" when there is no record.
func rolloutLine(r *cloudv1.ClusterRollout) string {
	if r == nil {
		return ""
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
