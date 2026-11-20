package aws_test

import (
	"reflect"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func TestSystemPoolIsFixedByThePlatform(t *testing.T) {
	old := clusterPlan().NodePool // a tenant-chosen pool: other taints, scaling 1..3

	got := crossplaneaws.FixedSystemNodePool(old)

	if !reflect.DeepEqual(got.Labels, map[string]string{"node.cluster-vending.io/pool": "system"}) {
		t.Errorf("labels = %v", got.Labels)
	}
	if want := []crossplaneaws.TaintPlan{{Key: "node.cluster-vending.io/pool", Value: "system", Effect: "NO_SCHEDULE"}}; !reflect.DeepEqual(got.Taints, want) {
		t.Errorf("taints = %v, want %v", got.Taints, want)
	}
	if got.MinSize != 2 || got.MaxSize != 2 || got.Desired != 2 {
		t.Errorf("size = %d/%d/%d, want 2/2/2", got.MinSize, got.MaxSize, got.Desired)
	}
	if len(old.Taints) != 1 || old.Taints[0].Key != "gpu" || old.MaxSize != 3 {
		t.Error("the input plan was changed")
	}
}

// Moving an existing cluster to the fixed pool is in place: nothing EKS
// cannot change on a node group changes, so the node group keeps its name
// and its object.
func TestFixingTheSystemPoolDoesNotReplaceTheNodeGroup(t *testing.T) {
	before := clusterPlan()
	after := before
	after.NodePool = crossplaneaws.FixedSystemNodePool(before.NodePool)

	if a, b := crossplaneaws.ReplacementNodeGroupName(before.Prefix, before.NodePool), crossplaneaws.ReplacementNodeGroupName(after.Prefix, after.NodePool); a != b {
		t.Errorf("replacement name changed: %s -> %s", a, b)
	}
	var old, now *awseks.NodeGroup
	for _, o := range crossplaneaws.ClusterResources(before) {
		if ng, ok := o.(*awseks.NodeGroup); ok {
			old = ng
		}
	}
	for _, o := range crossplaneaws.ClusterResources(after) {
		if ng, ok := o.(*awseks.NodeGroup); ok {
			now = ng
		}
	}
	if old.Name != now.Name || !crossplaneaws.SameNodeGroup(old, after.NodePool) {
		t.Errorf("node group %s -> %s, same settings = %v", old.Name, now.Name, crossplaneaws.SameNodeGroup(old, after.NodePool))
	}
	fp := now.Spec.ForProvider
	if *fp.ScalingConfig.MinSize != 2 || *fp.ScalingConfig.MaxSize != 2 || *fp.ScalingConfig.DesiredSize != 2 ||
		*fp.Labels["node.cluster-vending.io/pool"] != "system" || len(fp.Taint) != 1 || *fp.Taint[0].Value != "system" || *fp.Taint[0].Effect != "NO_SCHEDULE" {
		t.Errorf("node group = %+v", fp)
	}
}

func TestPoolPlacement(t *testing.T) {
	if got := crossplaneaws.PoolLabels("platform"); !reflect.DeepEqual(got, map[string]string{"node.cluster-vending.io/pool": "platform"}) {
		t.Errorf("labels = %v", got)
	}
	want := []map[string]any{{"key": "node.cluster-vending.io/pool", "operator": "Equal", "value": "platform", "effect": "NoSchedule"}}
	if got := crossplaneaws.PoolTolerations("platform"); !reflect.DeepEqual(got, want) {
		t.Errorf("tolerations = %v, want %v", got, want)
	}
}
