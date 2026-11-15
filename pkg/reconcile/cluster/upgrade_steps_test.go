package cluster

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Steps, short.
const (
	stepNodes    = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_NODE_GROUPS
	stepAddons   = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_ADDONS
	stepPlatform = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_PLATFORM_COMPONENTS
	stepTenant   = cloudv1.ClusterUpgradePhase_CLUSTER_UPGRADE_PHASE_TENANT_COMPONENTS
)

// An upgrade moves one step at a time, in the order a cluster is created;
// a step is left only once it is settled and its changes were applied in
// an earlier pass (a pass that applies a change may still read the old,
// settled state).
func TestNextStep(t *testing.T) {
	all := settledSteps{controlPlane: true, nodes: true, addons: true, platform: true, tenant: true}
	at := func(p cloudv1.ClusterUpgradePhase, applied bool) *cloudv1.ClusterUpgrade {
		return &cloudv1.ClusterUpgrade{ToVersion: "1.37", Phase: p, Applied: applied}
	}
	tests := []struct {
		name        string
		prev        *cloudv1.ClusterUpgrade
		s           settledSteps
		nodesBehind bool
		want        cloudv1.ClusterUpgradePhase
		applied     bool
	}{
		{name: "no upgrade", s: all, want: noUpgrade},
		{name: "the control plane is behind", s: settledSteps{}, want: controlPlane, applied: true},
		{name: "first seen with the nodes behind", s: settledSteps{controlPlane: true}, nodesBehind: true, want: stepNodes},
		{name: "the control plane is done: the node groups' turn", prev: at(controlPlane, true), s: all, want: stepNodes},
		{name: "a recorded NODES_AND_ADDONS is the node groups' step", prev: at(nodesAndAddons, false), s: all, want: stepNodes, applied: true},
		{name: "node groups just entered: applied this pass", prev: at(stepNodes, false), s: all, want: stepNodes, applied: true},
		{name: "node groups settled and applied: add-ons", prev: at(stepNodes, true), s: all, want: stepAddons},
		{name: "node groups not settled", prev: at(stepNodes, true), s: settledSteps{controlPlane: true, addons: true}, want: stepNodes, applied: true},
		{name: "add-ons settled: platform components", prev: at(stepAddons, true), s: all, want: stepPlatform},
		{name: "add-ons not settled", prev: at(stepAddons, true), s: settledSteps{controlPlane: true, nodes: true}, want: stepAddons, applied: true},
		{name: "platform settled: tenant components", prev: at(stepPlatform, true), s: all, want: stepTenant},
		{name: "platform not applied yet", prev: at(stepPlatform, false), s: all, want: stepPlatform, applied: true},
		{name: "tenant settled: done", prev: at(stepTenant, true), s: all, want: noUpgrade},
		{name: "a control plane behind again (another upgrade) wins", prev: at(stepTenant, true), s: settledSteps{}, want: controlPlane, applied: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, applied := nextStep(tt.prev, tt.s, tt.nodesBehind)
			if got != tt.want || applied != tt.applied {
				t.Errorf("nextStep = %s (applied %t), want %s (applied %t)", got, applied, tt.want, tt.applied)
			}
		})
	}
}
