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

// The system group is the admin's settings with the platform's placement:
// the system label and taint, one static size as min, max and desired;
// zero values are the defaults.
func TestSystemNodePool(t *testing.T) {
	got := crossplaneaws.SystemNodePool(crossplaneaws.SystemNodeGroup{Size: 3, InstanceTypes: []string{"m7i.large"}, CapacityType: "SPOT",
		AMIType: "BOTTLEROCKET_x86_64", DiskSizeGiB: 40})

	want := crossplaneaws.NodePoolPlan{InstanceTypes: []string{"m7i.large"}, CapacityType: "SPOT", AMIType: "BOTTLEROCKET_x86_64", DiskSizeGB: 40,
		MinSize: 3, MaxSize: 3, Desired: 3, Labels: map[string]string{"node.cluster-vending.io/pool": "system"},
		Taints: []crossplaneaws.TaintPlan{{Key: "node.cluster-vending.io/pool", Value: "system", Effect: "NO_SCHEDULE"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("system pool = %+v\nwant %+v", got, want)
	}
	defaults := crossplaneaws.SystemNodePool(crossplaneaws.SystemNodeGroup{})
	if !reflect.DeepEqual(defaults.InstanceTypes, []string{"t3.medium"}) || defaults.CapacityType != "ON_DEMAND" ||
		defaults.AMIType != "AL2023_x86_64_STANDARD" || defaults.DiskSizeGB != 0 || defaults.LaunchTemplate != nil {
		t.Errorf("defaults = %+v", defaults)
	}
	for _, c := range []struct{ size, want int }{
		{0, 2}, {1, 2}, {-3, 2}, // unset or below the least: the default
		{2, 2}, {9, 9},
	} {
		if p := crossplaneaws.SystemNodePool(crossplaneaws.SystemNodeGroup{Size: c.size}); p.MinSize != c.want || p.MaxSize != c.want || p.Desired != c.want {
			t.Errorf("size %d gave %d/%d/%d, want %d", c.size, p.MinSize, p.MaxSize, p.Desired, c.want)
		}
	}
}

// Moving an existing cluster to the static size, or changing the size
// later, is in place: nothing EKS cannot change on a node group changes, so
// the node group keeps its name and its object. The apply sends the size as
// desired, min and max in forProvider: an existing group that grew (under
// the former autoscaler) goes back to it, and Crossplane holds it there.
func TestFixingTheSystemPoolDoesNotReplaceTheNodeGroup(t *testing.T) {
	before := clusterPlan()
	before.NodePool = crossplaneaws.SystemNodePool(crossplaneaws.SystemNodeGroup{Size: 2})
	after := before
	after.NodePool = crossplaneaws.SystemNodePool(crossplaneaws.SystemNodeGroup{Size: 3})

	if a, b := crossplaneaws.ReplacementNodeGroupName(before.Prefix, before.NodePool), crossplaneaws.ReplacementNodeGroupName(after.Prefix, after.NodePool); a != b {
		t.Errorf("replacement name changed: %s -> %s", a, b)
	}
	old, now := nodeGroupOf(t, before), nodeGroupOf(t, after)
	if old.Name != now.Name || !crossplaneaws.SameNodeGroup(old, after.NodePool) {
		t.Errorf("node group %s -> %s, same settings = %v", old.Name, now.Name, crossplaneaws.SameNodeGroup(old, after.NodePool))
	}
	fp := now.Spec.ForProvider
	if *fp.ScalingConfig.MinSize != 3 || *fp.ScalingConfig.MaxSize != 3 || *fp.ScalingConfig.DesiredSize != 3 ||
		*fp.Labels["node.cluster-vending.io/pool"] != "system" || len(fp.Taint) != 1 || *fp.Taint[0].Value != "system" || *fp.Taint[0].Effect != "NO_SCHEDULE" {
		t.Errorf("node group = %+v", fp)
	}
	scaling, init := applyBodyScaling(t, after)
	if scaling["desiredSize"] != 3.0 || scaling["minSize"] != 3.0 || scaling["maxSize"] != 3.0 {
		t.Errorf("forProvider.scalingConfig = %v, want 3/3/3", scaling)
	}
	if init != nil {
		t.Errorf("initProvider.scalingConfig = %v, want none: the size is applied in place", init)
	}
}

// An imported cluster's own node group keeps its count: whatever scales it
// there does, so its desired size is sent at creation only.
func TestImportedNodeGroupDesiredSizeAtCreateOnly(t *testing.T) {
	p := clusterPlan()
	p.Adoption = crossplaneaws.Adoption{NodeRoleName: "their-node-role"}
	if !p.Imported() {
		t.Fatal("plan is not imported")
	}
	scaling, init := applyBodyScaling(t, p)
	if _, has := scaling["desiredSize"]; has {
		t.Errorf("forProvider.scalingConfig = %v, want no desiredSize", scaling)
	}
	if init["desiredSize"] == nil {
		t.Errorf("initProvider.scalingConfig = %v, want the desired size", init)
	}
}

// applyBodyScaling is the node group's forProvider and initProvider
// scalingConfig as ClusterSet applies them.
func applyBodyScaling(t *testing.T, p crossplaneaws.ClusterPlan) (forProvider, initProvider map[string]any) {
	t.Helper()
	for _, item := range crossplaneaws.ClusterSet(p) {
		if _, ok := item.Object.(*awseks.NodeGroup); !ok {
			continue
		}
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(item.Object)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: raw}
		if item.Adjust != nil {
			item.Adjust(u)
		}
		forProvider, _, _ = unstructured.NestedMap(u.Object, "spec", "forProvider", "scalingConfig")
		initProvider, _, _ = unstructured.NestedMap(u.Object, "spec", "initProvider", "scalingConfig")
		return forProvider, initProvider
	}
	t.Fatal("no node group in the set")
	return nil, nil
}

// Nothing autoscales the system group: no node group carries the
// cluster-autoscaler discovery tags.
func TestNoNodeGroupIsTaggedForClusterAutoscaler(t *testing.T) {
	p := clusterPlan()
	p.NodePool = crossplaneaws.SystemNodePool(crossplaneaws.SystemNodeGroup{})
	for _, plan := range []crossplaneaws.ClusterPlan{p, clusterPlan()} {
		for key := range nodeGroupOf(t, plan).Spec.ForProvider.Tags {
			if strings.HasPrefix(key, "k8s.io/cluster-autoscaler/") {
				t.Errorf("node group carries %s", key)
			}
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
