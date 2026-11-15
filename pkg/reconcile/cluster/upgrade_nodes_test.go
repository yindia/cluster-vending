package cluster

import (
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

const (
	updatingState = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING
	readyState    = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
)

func named(v *cloudv1.ManagedResource, external string) *cloudv1.ManagedResource {
	v.ExternalName = external
	return v
}

// In NODE_GROUPS the upgrade names each node group with the version the
// provider last saw and how it stands; in ADDONS, the add-ons still
// moving.
func TestUpgradeStatusNamesTheNodeGroups(t *testing.T) {
	yes := cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	row := &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1", UpdatedAt: time.Now()},
		State: StateUpdating, Generation: 4, AppliedGeneration: 4}}
	u := upgrade{target: "1.37", controlPlane: "1.37", nodes: []string{"1.36"}, nodeGroups: map[string]string{"prod-default": "1.36"}}
	views := []*cloudv1.ManagedResource{
		upgradeView("Cluster", "prod", readyState, yes, ""),
		named(upgradeView(KindNodeGroup, "prod-default", updatingState, yes, "updating: 1.36 → 1.37"), "prod-eks-default"),
		named(upgradeView("Addon", "prod-addon-coredns", updatingState, yes, ""), "prod-eks:coredns"),
		named(upgradeView("Addon", "prod-addon-vpc-cni", readyState, yes, ""), "prod-eks:vpc-cni"),
	}

	got := u.status(row, views, nil, componentsState{})

	if got.GetPhase() != stepNodes {
		t.Fatalf("phase = %s, want NODE_GROUPS", got.GetPhase())
	}
	groups := got.GetNodeGroups()
	if len(groups) != 1 || groups[0].GetName() != "prod-eks-default" || groups[0].GetFromVersion() != "1.36" ||
		groups[0].GetToVersion() != "1.37" || groups[0].GetState() != updatingState {
		t.Errorf("node groups = %v", groups)
	}
	if want := "node group prod-eks-default 1.36 → 1.37 (updating)"; !strings.Contains(got.GetMessage(), want) {
		t.Errorf("message = %q, want it to say %q", got.GetMessage(), want)
	}

	settledNodes := upgrade{target: "1.37", controlPlane: "1.37", nodes: []string{"1.37"},
		nodeGroups: map[string]string{"prod-default": "1.37"}, nodeSpecs: map[string]string{"prod-default": "1.37"}}
	views[1] = named(upgradeView(KindNodeGroup, "prod-default", readyState, yes, ""), "prod-eks-default")
	got = settledNodes.status(row, views, &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37", Phase: stepAddons, Applied: true},
		componentsState{})
	if behind := got.GetAddonsBehind(); got.GetPhase() != stepAddons || len(behind) != 1 || behind[0] != "coredns" {
		t.Errorf("phase %s, add-ons behind = %v, want ADDONS with [coredns]", got.GetPhase(), behind)
	}
	if want := "add-ons updating: coredns"; !strings.Contains(got.GetMessage(), want) {
		t.Errorf("message = %q, want it to say %q", got.GetMessage(), want)
	}
}

// A node group the provider still sees updating keeps the upgrade in
// NODE_GROUPS even though it reports the version: done is the version it
// reports and READY.
func TestALaggingNodeGroupKeepsTheUpgradeOpen(t *testing.T) {
	yes := cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
	row := &model.Cluster{Resource: model.Resource{Base: model.Base{ID: "c-1", UpdatedAt: time.Now()},
		State: StateUpdating, Generation: 4, AppliedGeneration: 4}}
	stored := &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37", Phase: nodesAndAddons, Applied: true}
	u := upgrade{target: "1.37", controlPlane: "1.37", nodes: []string{"1.37"}, nodeGroups: map[string]string{"prod-default": "1.37"}}
	views := []*cloudv1.ManagedResource{
		upgradeView("Cluster", "prod", readyState, yes, ""),
		upgradeView(KindNodeGroup, "prod-default", updatingState, yes, "updating"),
	}

	if got := u.status(row, views, stored, componentsState{}); got.GetPhase() != stepNodes {
		t.Errorf("phase = %s, want NODE_GROUPS while the node group updates", got.GetPhase())
	}
}

// While an upgrade runs, the cluster is UPDATING and its Ready condition
// says what the upgrade waits for, not "N of N resources ready".
func TestAnUpgradingClusterIsNotRunning(t *testing.T) {
	st := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING,
		Conditions: []*cloudv1.Condition{{Type: conditionReady, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
			Reason: "AllResourcesReady", Message: "51 of 51 resources ready"}},
		Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37", Phase: stepNodes,
			Message: "the control plane runs 1.37; node group prod-eks-default 1.36 → 1.37 (updating)"}}

	upgrading(st)

	ready := st.GetConditions()[0]
	if st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_UPDATING || ready.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_FALSE ||
		ready.GetReason() != "Upgrading" || !strings.HasPrefix(ready.GetMessage(), "upgrading 1.36 to 1.37: ") ||
		!strings.HasPrefix(st.GetMessage(), "upgrading 1.36 to 1.37: ") {
		t.Errorf("status = %v", st)
	}
}

// The upgrade's events: started, the node groups' turn, finished, each
// once.
func TestUpgradeEventsPerStep(t *testing.T) {
	cp := &cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37", Phase: controlPlane}}
	nodes := &cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37", Phase: stepNodes,
		NodeGroups: []*cloudv1.ClusterUpgradeNodeGroup{{Name: "prod-eks-default", FromVersion: "1.36", ToVersion: "1.37"}}}}
	done := &cloudv1.ClusterStatus{State: cloudv1.ClusterState_CLUSTER_STATE_RUNNING}
	step := func(p cloudv1.ClusterUpgradePhase) *cloudv1.ClusterStatus {
		return &cloudv1.ClusterStatus{Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37", Phase: p}}
	}

	for _, tt := range []struct {
		name          string
		before, after *cloudv1.ClusterStatus
		reason        string
		message       string
	}{
		{"started", done, cp, reconcile.ReasonUpgradeStarted, "upgrading Kubernetes from 1.36 to 1.37"},
		{"the node groups' turn", cp, nodes, reconcile.ReasonUpgradeNodes, "upgrading node group prod-eks-default to 1.37"},
		{"still on the node groups", nodes, nodes, "", ""},
		{"the add-ons' turn", nodes, step(stepAddons), reconcile.ReasonUpgradeStep, "upgrading the add-ons, then the platform components"},
		{"the platform components' turn", step(stepAddons), step(stepPlatform), reconcile.ReasonUpgradeStep, "moving the platform components to 1.37"},
		{"the tenant components' turn", step(stepPlatform), step(stepTenant), reconcile.ReasonUpgradeStep, "moving the tenant components"},
		{"finished", step(stepTenant), done, reconcile.ReasonUpgradeFinished, "Kubernetes 1.37: control plane, node groups, add-ons and components upgraded"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := upgradeEvents(tt.before, tt.after)

			if tt.reason == "" {
				if len(got) != 0 {
					t.Errorf("events = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Reason != tt.reason || !strings.Contains(got[0].Message, tt.message) {
				t.Errorf("events = %v, want %s %q", got, tt.reason, tt.message)
			}
		})
	}
}
