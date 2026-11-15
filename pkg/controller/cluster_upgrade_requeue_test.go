package controller

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// A RUNNING cluster with an upgrade in progress is looked at again within
// upgradeDelay: each step is read as the cloud reports it, not left to the
// next resync.
func TestAnUpgradingClusterIsLookedAtSoon(t *testing.T) {
	running := func(st *cloudv1.ClusterStatus) *model.Cluster {
		raw, err := reconcilecluster.Encode(st)
		if err != nil {
			t.Fatal(err)
		}
		return &model.Cluster{Resource: model.Resource{State: reconcilecluster.StateRunning, Generation: 2,
			ObservedGeneration: 2, AppliedGeneration: 2, Status: raw}}
	}
	upgradingRow := running(&cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37",
		Phase: cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS}})

	if got := nextCluster(upgradingRow, nil).RequeueAfter; got > upgradeDelay || got == 0 {
		t.Errorf("an upgrading cluster: requeue after %s, want at most %s", got, upgradeDelay)
	}
	if got := nextCluster(running(&cloudv1.ClusterStatus{}), nil).RequeueAfter; got != resyncDelay {
		t.Errorf("a settled cluster: requeue after %s, want %s", got, resyncDelay)
	}
}
