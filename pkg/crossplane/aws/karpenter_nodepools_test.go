package aws_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func nodeGroupsInput() crossplaneaws.KarpenterNodeGroupsInput {
	return crossplaneaws.KarpenterNodeGroupsInput{ClusterName: "prod", ClusterPrefix: "prod-1a2b3c4d", AMIPin: "v20261107", Discovery: true}
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

// The platform pool is the only Karpenter group: nothing without it, and a
// cluster name and prefix are required.
func TestKarpenterNodeGroupObjectsAreThePlatformPoolOnly(t *testing.T) {
	objs, err := crossplaneaws.KarpenterNodeGroupObjects(nodeGroupsInput())
	if err != nil || len(objs) != 0 {
		t.Fatalf("%d objects (%v), want none without a platform pool", len(objs), err)
	}
	if _, err := crossplaneaws.KarpenterNodeGroupObjects(crossplaneaws.KarpenterNodeGroupsInput{PlatformPool: defaultPlatformPool()}); err == nil {
		t.Error("want an error without the cluster's name and prefix")
	}
}

// A node class follows the platform's values: role, AMI pin, selectors
// by discovery tag or, where another cluster holds it, the cluster tag.
func TestRenderNodeClass(t *testing.T) {
	in := platformInput(defaultPlatformPool())
	objs, _ := crossplaneaws.KarpenterNodeGroupObjects(in)
	class := objectNamed(t, objs, "EC2NodeClass", crossplaneaws.PoolPlatform)

	role, _, _ := unstructured.NestedString(class.Object, "spec", "role")
	subnets, _, _ := unstructured.NestedSlice(class.Object, "spec", "subnetSelectorTerms")
	if role != "prod-1a2b3c4d-karpenter-node" || !reflect.DeepEqual(subnets[0], map[string]any{"tags": map[string]any{"karpenter.sh/discovery": "prod"}}) {
		t.Errorf("role %q subnets %v", role, subnets)
	}
	if class.GetLabels()[crossplaneaws.LabelManagedBy] != crossplaneaws.ManagedByPlatform {
		t.Errorf("labels %v: the class must carry the managed-by label", class.GetLabels())
	}
	in.Discovery, in.AMIPin = false, ""
	objs, _ = crossplaneaws.KarpenterNodeGroupObjects(in)
	class = objectNamed(t, objs, "EC2NodeClass", crossplaneaws.PoolPlatform)
	subnets, _, _ = unstructured.NestedSlice(class.Object, "spec", "subnetSelectorTerms")
	terms, _, _ := unstructured.NestedSlice(class.Object, "spec", "amiSelectorTerms")
	if !reflect.DeepEqual(subnets[0], map[string]any{"tags": map[string]any{"kubernetes.io/cluster/prod": "*"}}) ||
		terms[0].(map[string]any)["alias"] != "al2023@latest" {
		t.Errorf("fallback subnets %v ami %v", subnets, terms)
	}
}
