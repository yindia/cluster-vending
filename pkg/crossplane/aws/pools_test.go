package aws_test

import (
	"reflect"
	"strings"
	"testing"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func TestSystemPoolIsFixedByThePlatform(t *testing.T) {
	old := clusterPlan().NodePool // a tenant-chosen pool: other taints, scaling 1..3

	got := crossplaneaws.FixedSystemNodePool(old, 3, 8)

	if !reflect.DeepEqual(got.Labels, map[string]string{"node.cluster-vending.io/pool": "system"}) {
		t.Errorf("labels = %v", got.Labels)
	}
	if want := []crossplaneaws.TaintPlan{{Key: "node.cluster-vending.io/pool", Value: "system", Effect: "NO_SCHEDULE"}}; !reflect.DeepEqual(got.Taints, want) {
		t.Errorf("taints = %v, want %v", got.Taints, want)
	}
	// Desired is the size at creation (the minimum); afterwards it is the
	// autoscaler's (DesiredSizeAtCreateOnly).
	if got.MinSize != 3 || got.MaxSize != 8 || got.Desired != 3 {
		t.Errorf("size = %d/%d/%d, want min 3, max 8, desired 3", got.MinSize, got.MaxSize, got.Desired)
	}
	for _, c := range []struct{ min, max, wantMin, wantMax int }{
		{0, 0, 2, 6}, {1, 0, 2, 6}, {-3, 4, 2, 4}, // unset or below the minimum: the defaults
		{2, 0, 2, 6}, {2, 1, 2, 2}, {9, 0, 9, 9}, // a maximum below the minimum is the minimum
	} {
		if p := crossplaneaws.FixedSystemNodePool(old, c.min, c.max); p.MinSize != c.wantMin || p.MaxSize != c.wantMax || p.Desired != c.wantMin {
			t.Errorf("%d..%d gave %d/%d/%d, want min %d max %d", c.min, c.max, p.MinSize, p.MaxSize, p.Desired, c.wantMin, c.wantMax)
		}
	}
	if len(old.Taints) != 1 || old.Taints[0].Key != "gpu" || old.MaxSize != 3 {
		t.Error("the input plan was changed")
	}
}

// Moving an existing cluster to the autoscaled pool, or changing its min or
// max later, is in place: nothing EKS cannot change on a node group changes,
// so the node group keeps its name and its object; the apply never sends
// the desired size (the autoscaler's) except at creation.
func TestFixingTheSystemPoolDoesNotReplaceTheNodeGroup(t *testing.T) {
	before := clusterPlan()
	before.NodePool = crossplaneaws.FixedSystemNodePool(before.NodePool, 2, 2) // the former fixed size
	after := before
	after.NodePool = crossplaneaws.FixedSystemNodePool(before.NodePool, 3, 8)

	if a, b := crossplaneaws.ReplacementNodeGroupName(before.Prefix, before.NodePool), crossplaneaws.ReplacementNodeGroupName(after.Prefix, after.NodePool); a != b {
		t.Errorf("replacement name changed: %s -> %s", a, b)
	}
	var old *awseks.NodeGroup
	var now *awseks.NodeGroup
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
	if *fp.ScalingConfig.MinSize != 3 || *fp.ScalingConfig.MaxSize != 8 ||
		*fp.Labels["node.cluster-vending.io/pool"] != "system" || len(fp.Taint) != 1 || *fp.Taint[0].Value != "system" || *fp.Taint[0].Effect != "NO_SCHEDULE" {
		t.Errorf("node group = %+v", fp)
	}
	// The apply body: min and max in forProvider, desired only in initProvider.
	for _, item := range crossplaneaws.ClusterSet(after) {
		if _, ok := item.Object.(*awseks.NodeGroup); !ok {
			continue
		}
		u := &unstructured.Unstructured{}
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(item.Object)
		if err != nil {
			t.Fatal(err)
		}
		u.Object = raw
		item.Adjust(u)
		scaling, _, _ := unstructured.NestedMap(u.Object, "spec", "forProvider", "scalingConfig")
		if _, has := scaling["desiredSize"]; has || scaling["minSize"] != 3.0 || scaling["maxSize"] != 8.0 {
			t.Errorf("forProvider.scalingConfig = %v, want min 3 and max 8 without desiredSize", scaling)
		}
		if d, _, _ := unstructured.NestedFloat64(u.Object, "spec", "initProvider", "scalingConfig", "desiredSize"); d != 3 {
			t.Errorf("initProvider desiredSize = %v, want the minimum, 3", d)
		}
	}
}

// cluster-autoscaler finds the system group by the tags EKS copies to its
// Auto Scaling group; a user's (imported) group is not tagged.
func TestSystemNodeGroupIsTaggedForClusterAutoscaler(t *testing.T) {
	p := clusterPlan()
	p.NodePool = crossplaneaws.FixedSystemNodePool(p.NodePool, 2, 6)
	tags := nodeGroupOf(t, p).Spec.ForProvider.Tags
	for key, want := range map[string]string{
		"k8s.io/cluster-autoscaler/enabled":          "true",
		"k8s.io/cluster-autoscaler/" + p.ClusterName: "owned",
	} {
		if tags[key] == nil || *tags[key] != want {
			t.Errorf("tag %s = %v, want %s", key, tags[key], want)
		}
	}
	// The pool a plan has when it is not the system group has neither.
	other := clusterPlan()
	for key := range nodeGroupOf(t, other).Spec.ForProvider.Tags {
		if strings.HasPrefix(key, "k8s.io/cluster-autoscaler/") {
			t.Errorf("a non-system node group carries %s", key)
		}
	}
}

func nodeGroupOf(t *testing.T, p crossplaneaws.ClusterPlan) *awseks.NodeGroup {
	t.Helper()
	for _, o := range crossplaneaws.ClusterResources(p) {
		if ng, ok := o.(*awseks.NodeGroup); ok {
			return ng
		}
	}
	t.Fatal("no node group")
	return nil
}

func TestPoolPlacement(t *testing.T) {
	if got := crossplaneaws.PoolLabels("system"); !reflect.DeepEqual(got, map[string]string{"node.cluster-vending.io/pool": "system"}) {
		t.Errorf("labels = %v", got)
	}
	want := []map[string]any{{"key": "node.cluster-vending.io/pool", "operator": "Equal", "value": "system", "effect": "NoSchedule"}}
	if got := crossplaneaws.PoolTolerations("system"); !reflect.DeepEqual(got, want) {
		t.Errorf("tolerations = %v, want %v", got, want)
	}
}
