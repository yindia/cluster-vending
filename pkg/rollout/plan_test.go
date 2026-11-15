package rollout

import (
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// A rollout with nothing left to upgrade is at no tier: the status says
// none, not Tier0.
func TestComputeDoneIsAtNoTier(t *testing.T) {
	res := Compute(input(cluster("up-to-date", cloudv1alpha1.ClusterTier2, "1.36")))

	if res.Tier != "" || !res.Rollout.Done || !res.Plan.Done || res.Plan.Rolling {
		t.Fatalf("tier %q, done %v, plan %+v", res.Tier, res.Rollout.Done, res.Plan)
	}
}

// The result carries the projection of the same decision: the clusters
// it starts are the pending ones of the rolling tier's current batch.
func TestComputeCarriesThePlan(t *testing.T) {
	res := Compute(input(
		cluster("a", cloudv1alpha1.ClusterTier2, "1.35"),
		cluster("b", cloudv1alpha1.ClusterTier2, "1.35"),
		cluster("c", cloudv1alpha1.ClusterTier2, "1.35"),
		cluster("prod", cloudv1alpha1.ClusterTier0, "1.35"),
	))

	if !res.Plan.Rolling || res.Plan.Tier != upgrade.Tier2 || res.Plan.Batch != 1 || res.Plan.Batches != 2 {
		t.Fatalf("plan = %+v", res.Plan)
	}
	current := res.Plan.Tiers[0].Batches[res.Plan.Tiers[0].Current].Members
	if len(res.Start) != 2 || current[0] != res.Start[0] || current[1] != res.Start[1] {
		t.Errorf("current batch %v, starts %v", current, res.Start)
	}
	if res.Plan.Tiers[1].State != upgrade.TierNotStarted {
		t.Errorf("tier 0 = %+v", res.Plan.Tiers[1])
	}
}
