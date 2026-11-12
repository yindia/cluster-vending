package idl_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The phases are numbered in the order an upgrade goes through them, and
// status.upgrade survives the JSON a status is stored and returned as.
func TestClusterUpgradeStatus(t *testing.T) {
	if cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_UNSPECIFIED != 0 ||
		cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE >= cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODES_AND_ADDONS {
		t.Error("the upgrade phases are not numbered control plane first, then nodes and add-ons")
	}
	in := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_UPDATING, Upgrade: &cloudv1.ClusterUpgrade{
		FromVersion: "1.34", ToVersion: "1.35", Phase: cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_CONTROL_PLANE,
		StartTime: timestamppb.New(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)), Message: "upgrading the control plane",
	}}
	raw, err := protojson.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out := &cloudv1.ClusterStatus{}
	if err := protojson.Unmarshal(raw, out); err != nil || !proto.Equal(in, out) {
		t.Errorf("status after a JSON round trip = %v (%v), want %v", out, err, in)
	}
	if (&cloudv1.ClusterStatus{}).GetUpgrade() != nil {
		t.Error("a status without an upgrade reports one")
	}
}
