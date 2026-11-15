package admin

import (
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A channel's release moves reach the API as its status reports them:
// the release, who may start, why the rest waits, the counts per tier and
// the clusters kept back; none planned is unset.
func TestChangeRolloutFromAPI(t *testing.T) {
	if ChangeRolloutFromAPI(nil) != nil {
		t.Error("no release moves: want nil")
	}
	m := &cloudv1alpha1.ChannelReleaseMoves{Release: shipped, Admitted: []string{"tenant-acme/dev"}, Waiting: "tier 1 soaks",
		Members: []cloudv1alpha1.RolloutTierCount{{Tier: cloudv1alpha1.ClusterTier2, Succeeded: 1, Pending: 2}},
		Blocked: []cloudv1alpha1.BlockedCluster{{Cluster: "tenant-acme/prod", Gate: "FROZEN", Message: "the platform is frozen"}}}

	got := ChangeRolloutFromAPI(m)

	if got.GetTarget() != shipped || len(got.GetAdmitted()) != 1 || got.GetWaiting() != "tier 1 soaks" ||
		len(got.GetTiers()) != 1 || got.GetTiers()[0].GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_2 || got.GetTiers()[0].GetPending() != 2 {
		t.Errorf("release moves = %v", got)
	}
	if b := got.GetBlocked(); len(b) != 1 || b[0].GetGate() != "FROZEN" || b[0].GetReason() != "the platform is frozen" {
		t.Errorf("blocked = %v", b)
	}
	got.Admitted[0] = "changed"
	if m.Admitted[0] != "tenant-acme/dev" {
		t.Error("the status's admitted list was changed through the copy")
	}
}
