package cli

import (
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Mid-upgrade, cluster get shows every resource the upgrade changes as
// Updating, with from and to, and the Upgrade line names the node group
// and the add-ons still moving.
func TestPrintClusterMidUpgrade(t *testing.T) {
	withClock(t)
	const (
		ready    = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
		updating = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING
		yes      = cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	)
	c := testCluster()
	c.Status.State = cloudv1.ClusterState_CLUSTER_STATE_UPDATING
	c.Status.ManagedResources = []*cloudv1.ManagedResource{
		{Kind: "Cluster", Name: "prod-eks-d9cd021e", ExternalName: "prod-eks", State: ready, Synced: yes, Ready: yes},
		{Kind: "NodeGroup", Name: "prod-eks-d9cd021e-default", ExternalName: "prod-eks-d9cd021e-default", State: updating,
			Synced: yes, Ready: yes, Message: "updating: 1.36 → 1.37"},
		{Kind: "Addon", Name: "prod-eks-d9cd021e-addon-coredns", ExternalName: "prod-eks:coredns", State: updating,
			Synced: yes, Ready: yes, Message: "updating: v1.14.7-eksbuild.35 → v1.15.0-eksbuild.2"},
	}
	c.Status.Upgrade = &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37",
		Phase:        cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS,
		NodeGroups:   []*cloudv1.ClusterUpgradeNodeGroup{{Name: "prod-eks-d9cd021e-default", FromVersion: "1.36", ToVersion: "1.37", State: updating}},
		AddonsBehind: []string{"coredns"}}

	got := printed(t, &cloudv1.GetClusterResponse{Cluster: c}, OutputTable)

	for _, want := range []string{
		"Phase:         Updating\n",
		"Upgrade:       1.36 to 1.37 (NodesAndAddons): node group prod-eks-d9cd021e-default 1.36 → 1.37 updating; add-ons updating: coredns\n",
		"  Cluster     prod-eks-d9cd021e                 prod-eks                    Ready      True     True    <none>\n",
		"  NodeGroup   prod-eks-d9cd021e-default         prod-eks-d9cd021e-default   Updating   True     True    updating: 1.36 → 1.37\n",
		"  Addon       prod-eks-d9cd021e-addon-coredns   prod-eks:coredns            Updating   True     True    updating: v1.14.7-eksbuild.35 → v1.15.0-eksbuild.2\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cluster get does not show %q:\n%s", want, got)
		}
	}
}
