package convert_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/controller/api/v1alpha1"
	"github.com/yindia/cluster-vending/controller/convert"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The upgrade in progress is carried onto the kind's status, each phase
// under the kind's spelling, and cleared when the contract has none.
func TestClusterStatusIntoCarriesTheUpgrade(t *testing.T) {
	started := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for phase, want := range map[cloudv1.ClusterUpgradePhase]cloudv1alpha1.ClusterUpgradePhase{
		cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE:    cloudv1alpha1.ClusterUpgradeControlPlane,
		cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS: cloudv1alpha1.ClusterUpgradeNodesAndAddons,
	} {
		var status cloudv1alpha1.ClusterStatus
		convert.ClusterStatusInto(&cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_UPDATING,
			Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.34", ToVersion: "1.35", Phase: phase,
				StartTime: timestamppb.New(started), Message: "Cluster prod: refused"},
		}, "", metav1.Now(), &status)
		u := status.Upgrade
		if u == nil || u.Phase != want || u.FromVersion != "1.34" || u.ToVersion != "1.35" || u.Message != "Cluster prod: refused" ||
			u.StartTime == nil || !u.StartTime.Time.Equal(started) {
			t.Errorf("%s: upgrade = %+v, want phase %s from 1.34 to 1.35 since %s", phase, u, want, started)
		}
		// The next status has no upgrade: it is gone from the kind too.
		convert.ClusterStatusInto(&cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING}, "", metav1.Now(), &status)
		if status.Upgrade != nil {
			t.Errorf("%s: upgrade after it finished = %+v, want none", phase, status.Upgrade)
		}
	}
}

// Every phase the contract has is one the kind spells out.
func TestEveryUpgradePhaseHasAKindSpelling(t *testing.T) {
	for number, name := range cloudv1.ClusterUpgradePhase_name {
		phase := cloudv1.ClusterUpgradePhase(number)
		if phase == cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_UNSPECIFIED {
			continue
		}
		var status cloudv1alpha1.ClusterStatus
		convert.ClusterStatusInto(&cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{Phase: phase}}, "", metav1.Now(), &status)
		if status.Upgrade == nil || status.Upgrade.Phase == "" {
			t.Errorf("%s has no spelling on the kind", name)
		}
	}
}
