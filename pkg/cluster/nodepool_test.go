package cluster

import (
	"reflect"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

var zonesAB = []string{"use1-az1", "use1-az2"}

// systemPlacement is the system group's fixed label and taint.
func assertSystemPlacement(t *testing.T, pool crossplaneaws.NodePoolPlan) {
	t.Helper()
	if !reflect.DeepEqual(pool.Labels, map[string]string{"node.cluster-vending.io/pool": "system"}) {
		t.Errorf("labels = %v", pool.Labels)
	}
	if !reflect.DeepEqual(pool.Taints, []crossplaneaws.TaintPlan{{Key: "node.cluster-vending.io/pool", Value: "system", Effect: "NO_SCHEDULE"}}) {
		t.Errorf("taints = %v", pool.Taints)
	}
}

// A spec has no node group: a cluster the platform created gets exactly the
// platform's system group (the defaults when the admin sets nothing) and
// Karpenter's setup, the platform pool's prerequisite.
func TestRenderPlanGivesAPlatformClusterTheDefaultSystemGroup(t *testing.T) {
	spec := mustResolve(t, minimalSpec(), zonesAB)

	plan := RenderPlan(spec, []string{"subnet-a"}, crossplaneawsBase())

	want := crossplaneaws.NodePoolPlan{InstanceTypes: []string{"t3.medium"}, CapacityType: "ON_DEMAND", AMIType: "AL2023_x86_64_STANDARD",
		MinSize: 2, MaxSize: 2, Desired: 2,
		Labels: map[string]string{"node.cluster-vending.io/pool": "system"},
		Taints: []crossplaneaws.TaintPlan{{Key: "node.cluster-vending.io/pool", Value: "system", Effect: "NO_SCHEDULE"}}}
	if !reflect.DeepEqual(plan.NodePool, want) {
		t.Errorf("node pool = %+v\nwant %+v", plan.NodePool, want)
	}
	if !plan.Karpenter {
		t.Error("a platform cluster has Karpenter's setup")
	}
}

// Every setting of the system group is the admin's (nodeGroups.system).
func TestRenderPlanTakesTheAdminsSystemGroup(t *testing.T) {
	spec := mustResolve(t, minimalSpec(), zonesAB)
	base := crossplaneawsBase()
	base.System = crossplaneaws.SystemNodeGroup{Size: 4, InstanceTypes: []string{"m7g.large", "m6g.large"}, CapacityType: "SPOT",
		AMIType: "AL2023_ARM_64_STANDARD", DiskSizeGiB: 50}

	plan := RenderPlan(spec, []string{"subnet-a"}, base)

	p := plan.NodePool
	if !reflect.DeepEqual(p.InstanceTypes, []string{"m7g.large", "m6g.large"}) || p.CapacityType != "SPOT" ||
		p.AMIType != "AL2023_ARM_64_STANDARD" || p.DiskSizeGB != 50 || p.MinSize != 4 || p.MaxSize != 4 || p.Desired != 4 || p.LaunchTemplate != nil {
		t.Errorf("node pool = %+v", p)
	}
	assertSystemPlacement(t, p)
}

// A cluster built with the defaults before the platform owned the group
// stored t3.medium, on-demand and AL2023 x86: its render now is the same
// node group (same replacement name), so nothing is replaced. A change of
// the admin's size stays in place; one of the instance types replaces the
// group.
func TestSystemGroupChangesReplaceOnlyWhatEKSCannotChange(t *testing.T) {
	spec := mustResolve(t, minimalSpec(), zonesAB)
	base := crossplaneawsBase()
	stored := crossplaneaws.NodePoolPlan{InstanceTypes: []string{"t3.medium"}, CapacityType: "ON_DEMAND", AMIType: "AL2023_x86_64_STANDARD"}
	before := RenderPlan(spec, []string{"subnet-a"}, base).NodePool
	if crossplaneaws.ReplacementNodeGroupName("p", stored) != crossplaneaws.ReplacementNodeGroupName("p", before) {
		t.Error("a default cluster's node group would be replaced")
	}

	base.System.Size = 5
	resized := RenderPlan(spec, []string{"subnet-a"}, base).NodePool
	if resized.Desired != 5 || crossplaneaws.ReplacementNodeGroupName("p", resized) != crossplaneaws.ReplacementNodeGroupName("p", before) {
		t.Errorf("a size change must stay in place: %+v", resized)
	}

	base.System.InstanceTypes = []string{"m7i.large"}
	retyped := RenderPlan(spec, []string{"subnet-a"}, base).NodePool
	if crossplaneaws.ReplacementNodeGroupName("p", retyped) == crossplaneaws.ReplacementNodeGroupName("p", before) {
		t.Error("an instance type change must replace the node group")
	}
}

// An imported cluster renders the node group it adopted, as it is: its
// labels, taints, size and launch template; no system group, no Karpenter.
func TestRenderPlanOfAnImportedClusterIsItsAdoptedNodeGroup(t *testing.T) {
	spec := mustResolve(t, minimalSpec(), zonesAB)
	base := crossplaneawsBase()
	adopted := &crossplaneaws.NodePoolPlan{InstanceTypes: []string{"m6i.large"}, CapacityType: "ON_DEMAND", AMIType: "AL2023_x86_64_STANDARD",
		MinSize: 2, MaxSize: 4, Desired: 3, Labels: map[string]string{"role": "system"},
		LaunchTemplate: &crossplaneaws.LaunchTemplatePlan{ID: "lt-0123456789abcdef0", Version: "3"}}
	base.ExternalNames = map[string]string{"prod-12345678-default": "ng-1"}
	base.NodeGroup = adopted

	plan := RenderPlan(spec, []string{"subnet-a"}, base)

	if plan.Karpenter || !reflect.DeepEqual(plan.NodePool, *adopted) {
		t.Errorf("imported plan = karpenter %v, pool %+v", plan.Karpenter, plan.NodePool)
	}
}
