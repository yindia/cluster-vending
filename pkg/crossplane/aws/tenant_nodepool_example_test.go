package aws_test

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// examples/tenant-nodepool.yaml is what a tenant copies to make a pool of
// its own: its EC2NodeClass selects the subnets and security group the
// platform pool's class selects (the tags the platform's subnets and EKS
// carry) and names Karpenter's node role, the only one its controller may
// pass. It carries no platform label, so the platform leaves it alone.
func TestExampleTenantNodePoolMatchesThePlatformsSelectors(t *testing.T) {
	raw, err := os.ReadFile("../../../examples/tenant-nodepool.yaml")
	if err != nil {
		t.Fatal(err)
	}
	example := map[string]*unstructured.Unstructured{}
	for _, doc := range bytes.Split(raw, []byte("\n---\n")) {
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(doc, &u.Object); err != nil {
			t.Fatal(err)
		}
		if _, ok := u.GetLabels()[crossplaneaws.LabelManagedBy]; ok {
			t.Errorf("%s %s carries the platform's label", u.GetKind(), u.GetName())
		}
		if name := u.GetName(); name == crossplaneaws.PoolPlatform || name == "platform" {
			t.Errorf("%s is named like a platform pool", u.GetKind())
		}
		example[u.GetKind()] = u
	}
	class, pool := example["EC2NodeClass"], example["NodePool"]
	if class == nil || pool == nil {
		t.Fatalf("the example has %v, want an EC2NodeClass and a NodePool", example)
	}
	in := platformInput(defaultPlatformPool())
	in.ClusterName, in.Discovery = "prod-eks", true
	objects, err := crossplaneaws.KarpenterNodeGroupObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	platform := asJSONMap(t, objects[0])["spec"].(map[string]any)
	spec := class.Object["spec"].(map[string]any)
	for _, key := range []string{"subnetSelectorTerms", "securityGroupSelectorTerms"} {
		if !reflect.DeepEqual(spec[key], platform[key]) {
			t.Errorf("%s = %v, the platform pool's class has %v", key, spec[key], platform[key])
		}
	}
	if role, _ := spec["role"].(string); !strings.HasSuffix(role, crossplaneaws.KarpenterNodeRoleName("")) {
		t.Errorf("role = %q, want Karpenter's node role (%s)", role, crossplaneaws.KarpenterNodeRoleName("<prefix>"))
	}
	if _, ok := spec["instanceProfile"]; ok {
		t.Error("the example sets an instance profile: Karpenter makes it from the role")
	}
	if ref, _, _ := unstructured.NestedString(pool.Object, "spec", "template", "spec", "nodeClassRef", "name"); ref != class.GetName() {
		t.Errorf("the pool refers to class %q, want %q", ref, class.GetName())
	}
}
