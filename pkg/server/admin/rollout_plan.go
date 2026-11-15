package admin

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/rollout"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

var tierStates = map[upgrade.TierState]cloudv1.RolloutTierState{
	upgrade.TierDone:       cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_DONE,
	upgrade.TierRolling:    cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_ROLLING,
	upgrade.TierSoaking:    cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_SOAKING,
	upgrade.TierNotStarted: cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_NOT_STARTED,
	upgrade.TierHalted:     cloudv1.RolloutTierState_ROLLOUT_TIER_STATE_HALTED,
}

// RolloutPlanFromResult is the plan of a computed rollout (res.Plan), its
// clusters named from res.Rows. Not paged: every cluster of the channel.
func RolloutPlanFromResult(res rollout.Result) *cloudv1.RolloutPlan {
	rows := make(map[string]rollout.Row, len(res.Rows))
	for _, r := range res.Rows {
		rows[r.Key()] = r
	}
	p := res.Plan
	out := &cloudv1.RolloutPlan{Rolling: p.Rolling, Batch: int32(p.Batch), Batches: int32(p.Batches), //nolint:gosec // small counts
		Done: p.Done, Halted: p.Halted}
	if p.Rolling {
		out.RollingTier = RolloutTierFromAPI(p.Tier)
	}
	for _, t := range p.Tiers {
		tp := &cloudv1.RolloutTierPlan{Tier: RolloutTierFromAPI(t.Tier), State: tierStates[t.State],
			Total: rollout.Count32(t.Total), Done: rollout.Count32(t.Done), InProgress: rollout.Count32(t.InProgress),
			Failed: rollout.Count32(t.Failed), Blocked: rollout.Count32(t.Blocked), MaxFailed: rollout.Count32(t.MaxFailed),
			BatchSize: rollout.Count32(t.BatchSize), FinishedTime: stamp(t.FinishedAt), SoakUntil: stamp(t.SoakUntil),
			Current: int32(t.Current)} //nolint:gosec // an index of a few batches
		for _, b := range t.Batches {
			batch := &cloudv1.RolloutBatch{}
			for _, key := range b.Members {
				batch.Clusters = append(batch.Clusters, planCluster(rows[key]))
			}
			tp.Batches = append(tp.Batches, batch)
		}
		for _, key := range t.Waits {
			tp.Waits = append(tp.Waits, planCluster(rows[key]))
		}
		out.Tiers = append(out.Tiers, tp)
	}
	return out
}

func planCluster(r rollout.Row) *cloudv1.RolloutPlanCluster {
	out := &cloudv1.RolloutPlanCluster{TenantId: r.Tenant, ClusterName: r.Name, ClusterId: r.ID,
		State: rolloutStates[r.State], Phase: r.Phase, Since: stamp(r.Since)}
	if r.Gate != nil {
		out.Reason = r.Gate.Message
	}
	return out
}

func stamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
