package aws_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func nodeGroupsInput(groups ...*cloudv1.NodeGroupSpec) crossplaneaws.KarpenterNodeGroupsInput {
	return crossplaneaws.KarpenterNodeGroupsInput{ClusterName: "prod", ClusterPrefix: "prod-1a2b3c4d", AMIPin: "v20261107",
		Discovery: true, NodeGroups: groups}
}

func objectNamed(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no %s %s in %d objects", kind, name, len(objs))
	return nil
}

func reqValues(t *testing.T, pool *unstructured.Unstructured, key string) []string {
	t.Helper()
	reqs, _, _ := unstructured.NestedSlice(pool.Object, "spec", "template", "spec", "requirements")
	for _, r := range reqs {
		m := r.(map[string]any)
		if m["key"] == key {
			var out []string
			for _, v := range m["values"].([]any) {
				out = append(out, v.(string))
			}
			return out
		}
	}
	return nil
}

// The objects are the platform's class and pool, then each group's, a class
// before its pool, every one labelled managed; the same input always gives
// the same bytes.
func TestKarpenterNodeGroupObjectsOrderAndDeterminism(t *testing.T) {
	in := nodeGroupsInput(&cloudv1.NodeGroupSpec{Name: "gpu", InstanceFamilies: []string{"g5"}}, &cloudv1.NodeGroupSpec{Name: "batch"})

	objs, err := crossplaneaws.KarpenterNodeGroupObjects(in)

	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, o := range objs {
		order = append(order, o.GetKind()+"/"+o.GetName())
		if o.GetLabels()[crossplaneaws.LabelManagedBy] != crossplaneaws.ManagedByPlatform {
			t.Errorf("%s is not labelled managed", o.GetName())
		}
	}
	want := []string{"EC2NodeClass/platform", "NodePool/platform", "EC2NodeClass/gpu", "NodePool/gpu", "EC2NodeClass/batch", "NodePool/batch"}
	if !slices.Equal(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
	again, _ := crossplaneaws.KarpenterNodeGroupObjects(in)
	a, _ := json.Marshal(objs)
	b, _ := json.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Error("the same input rendered other objects")
	}
}

func TestKarpenterNodeGroupObjectsRefuseBadNames(t *testing.T) {
	for _, name := range []string{"", "platform", "default", "system", "Bad_Name", "dup"} {
		groups := []*cloudv1.NodeGroupSpec{{Name: name}}
		if name == "dup" {
			groups = append(groups, &cloudv1.NodeGroupSpec{Name: "dup"})
		}
		if _, err := crossplaneaws.KarpenterNodeGroupObjects(nodeGroupsInput(groups...)); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
	if _, err := crossplaneaws.KarpenterNodeGroupObjects(crossplaneaws.KarpenterNodeGroupsInput{}); err == nil {
		t.Error("an input without a cluster was accepted")
	}
}

// The platform pool is on-demand, amd64, tainted and labelled for the
// platform's pods, with generous limits.
func TestRenderPlatformNodePool(t *testing.T) {
	objs, _ := crossplaneaws.KarpenterNodeGroupObjects(nodeGroupsInput())
	pool := objectNamed(t, objs, "NodePool", "platform")

	labels, _, _ := unstructured.NestedMap(pool.Object, "spec", "template", "metadata", "labels")
	taints, _, _ := unstructured.NestedSlice(pool.Object, "spec", "template", "spec", "taints")
	if labels["node.cluster-vending.io/pool"] != "platform" || len(taints) != 1 ||
		taints[0].(map[string]any)["key"] != "node.cluster-vending.io/pool" || taints[0].(map[string]any)["effect"] != "NoSchedule" {
		t.Errorf("labels %v taints %v", labels, taints)
	}
	if got := reqValues(t, pool, "karpenter.sh/capacity-type"); !slices.Equal(got, []string{"on-demand"}) {
		t.Errorf("capacity types %v", got)
	}
	if cpu, _, _ := unstructured.NestedString(pool.Object, "spec", "limits", "cpu"); cpu == "" {
		t.Error("no limits")
	}
	if ref, _, _ := unstructured.NestedString(pool.Object, "spec", "template", "spec", "nodeClassRef", "name"); ref != "platform" {
		t.Errorf("nodeClassRef %q", ref)
	}
}

// A user group renders its spec; the pool label cannot be overridden.
func TestRenderUserNodePool(t *testing.T) {
	g := &cloudv1.NodeGroupSpec{
		Name: "batch", InstanceTypes: []string{"m7i.large", "c7g.large", "m7i.large"}, InstanceFamilies: []string{"m7i"},
		Architectures: []cloudv1.Architecture{cloudv1.Architecture_ARCHITECTURE_ARM64, cloudv1.Architecture_ARCHITECTURE_AMD64},
		CapacityTypes: []cloudv1.CapacityType{cloudv1.CapacityType_CAPACITY_TYPE_SPOT},
		Labels:        map[string]string{"team": "ml", "node.cluster-vending.io/pool": "evil"},
		Taints:        []*cloudv1.Taint{{Key: "dedicated", Value: "ml", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_EXECUTE}},
		Limits:        &cloudv1.NodeGroupLimits{Cpu: ptr.To(int32(64)), MemoryGib: ptr.To(int32(256))}}

	objs, _ := crossplaneaws.KarpenterNodeGroupObjects(nodeGroupsInput(g, &cloudv1.NodeGroupSpec{Name: "x"}))
	pool := objectNamed(t, objs, "NodePool", "batch")

	labels, _, _ := unstructured.NestedMap(pool.Object, "spec", "template", "metadata", "labels")
	if !reflect.DeepEqual(labels, map[string]any{"node.cluster-vending.io/pool": "batch", "team": "ml"}) {
		t.Errorf("labels %v", labels)
	}
	taints, _, _ := unstructured.NestedSlice(pool.Object, "spec", "template", "spec", "taints")
	if len(taints) != 1 || taints[0].(map[string]any)["effect"] != "NoExecute" {
		t.Errorf("taints %v", taints)
	}
	if got := reqValues(t, pool, "node.kubernetes.io/instance-type"); !slices.Equal(got, []string{"c7g.large", "m7i.large"}) {
		t.Errorf("types %v", got)
	}
	if got := reqValues(t, pool, "kubernetes.io/arch"); !slices.Equal(got, []string{"amd64", "arm64"}) {
		t.Errorf("arch %v", got)
	}
	if got := reqValues(t, pool, "karpenter.sh/capacity-type"); !slices.Equal(got, []string{"spot"}) {
		t.Errorf("capacity %v", got)
	}
	limits, _, _ := unstructured.NestedMap(pool.Object, "spec", "limits")
	if !reflect.DeepEqual(limits, map[string]any{"cpu": "64", "memory": "256Gi"}) {
		t.Errorf("limits %v", limits)
	}
	if reqValues(t, pool, "karpenter.k8s.aws/instance-category") != nil {
		t.Error("the default categories apply only without types or families")
	}
	empty := objectNamed(t, objs, "NodePool", "x")
	if got := reqValues(t, empty, "kubernetes.io/arch"); !slices.Equal(got, []string{"amd64"}) {
		t.Errorf("default arch %v", got)
	}
	if _, found, _ := unstructured.NestedMap(empty.Object, "spec", "limits"); found {
		t.Error("a group without limits has none")
	}
}

// The node class follows the platform's values: role, AMI pin, selectors
// by discovery tag or, where another cluster holds it, the cluster tag.
func TestRenderNodeClass(t *testing.T) {
	in := nodeGroupsInput()
	objs, _ := crossplaneaws.KarpenterNodeGroupObjects(in)
	class := objectNamed(t, objs, "EC2NodeClass", "platform")

	role, _, _ := unstructured.NestedString(class.Object, "spec", "role")
	subnets, _, _ := unstructured.NestedSlice(class.Object, "spec", "subnetSelectorTerms")
	if role != "prod-1a2b3c4d-karpenter-node" || !reflect.DeepEqual(subnets[0], map[string]any{"tags": map[string]any{"karpenter.sh/discovery": "prod"}}) {
		t.Errorf("role %q subnets %v", role, subnets)
	}
	in.Discovery, in.AMIPin = false, ""
	objs, _ = crossplaneaws.KarpenterNodeGroupObjects(in)
	class = objectNamed(t, objs, "EC2NodeClass", "platform")
	subnets, _, _ = unstructured.NestedSlice(class.Object, "spec", "subnetSelectorTerms")
	terms, _, _ := unstructured.NestedSlice(class.Object, "spec", "amiSelectorTerms")
	if !reflect.DeepEqual(subnets[0], map[string]any{"tags": map[string]any{"kubernetes.io/cluster/prod": "*"}}) ||
		terms[0].(map[string]any)["alias"] != "al2023@latest" {
		t.Errorf("fallback subnets %v ami %v", subnets, terms)
	}
}
