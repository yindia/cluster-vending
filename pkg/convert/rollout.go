package convert

import (
	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// rolloutResults are the kind's spellings of an automatic upgrade's
// results.
var rolloutResults = map[cloudv1.ClusterRolloutResult]cloudv1alpha1.ClusterRolloutResult{
	cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS: cloudv1alpha1.ClusterRolloutInProgress,
	cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_SUCCEEDED:   cloudv1alpha1.ClusterRolloutSucceeded,
	cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_FAILED:      cloudv1alpha1.ClusterRolloutFailed,
	cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT:   cloudv1alpha1.ClusterRolloutTimedOut,
	cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING:     cloudv1alpha1.ClusterRolloutPending,
	cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED:     cloudv1alpha1.ClusterRolloutBlocked,
}

// RolloutToAPI is the kind's view of a cluster's automatic upgrade; nil
// for none.
func RolloutToAPI(p *cloudv1.ClusterRollout) *cloudv1alpha1.ClusterRolloutRecord {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.ClusterRolloutRecord{Channel: p.GetChannel(), FromVersion: p.GetFromVersion(),
		ToVersion: p.GetToVersion(), FinishTime: timeOf(p.GetFinishTime()), Result: rolloutResults[p.GetResult()],
		Message: p.GetMessage(), Tier: p.GetTier()}
	if start := timeOf(p.GetStartTime()); start != nil {
		out.StartTime = *start
	}
	return out
}

// RolloutFromAPI is the contract's view of what the kind holds; nil for
// none.
func RolloutFromAPI(a *cloudv1alpha1.ClusterRolloutRecord) *cloudv1.ClusterRollout {
	if a == nil {
		return nil
	}
	out := &cloudv1.ClusterRollout{Channel: a.Channel, FromVersion: a.FromVersion, ToVersion: a.ToVersion,
		StartTime: timestampOf(&a.StartTime), FinishTime: timestampOf(a.FinishTime), Message: a.Message, Tier: a.Tier}
	for p, k := range rolloutResults {
		if k == a.Result {
			out.Result = p
		}
	}
	return out
}
