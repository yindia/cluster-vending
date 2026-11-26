package aws_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// The nodes carry the cluster's labels and the platform's tenant and
// cluster tags; keys Karpenter or AWS reserve are left out (Karpenter
// refuses an EC2NodeClass with them), and a label cannot change the
// platform's own tags.
func TestKarpenterNodeTags(t *testing.T) {
	labels := map[string]string{
		"team": "payments", "cost-center": "42",
		"Name": "x", "aws:created": "x", "kubernetes.io/cluster/prod": "owned", "eks:eks-cluster-name": "x",
		"karpenter.sh/nodepool": "x", "karpenter.sh/nodeclaim": "x", "karpenter.k8s.aws/ec2nodeclass": "x",
		crossplaneaws.TagTenant: "someone-else",
	}

	got := crossplaneaws.KarpenterNodeTags("t-1", "c-1", "prod", labels)

	want := map[string]string{"team": "payments", "cost-center": "42",
		crossplaneaws.TagTenant: "t-1", crossplaneaws.TagCluster: "c-1", crossplaneaws.TagClusterName: "prod"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tags %v, want %v", got, want)
	}
	if _, ok := labels["team"]; !ok || labels[crossplaneaws.TagTenant] != "someone-else" {
		t.Fatal("the labels were changed")
	}
}

// The platform pool's EC2NodeClass carries the tags in spec.tags; without
// tags it has none (the golden class).
func TestPlatformPoolNodeClassTags(t *testing.T) {
	in := platformInput(defaultPlatformPool())
	in.Tags = crossplaneaws.KarpenterNodeTags("t-1", "c-1", "prod", map[string]string{"team": "payments"})
	objs, err := crossplaneaws.KarpenterNodeGroupObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	class := objectNamed(t, objs, "EC2NodeClass", crossplaneaws.PoolPlatform)
	tags, _, _ := unstructured.NestedStringMap(class.Object, "spec", "tags")
	if !reflect.DeepEqual(tags, in.Tags) {
		t.Errorf("spec.tags %v, want %v", tags, in.Tags)
	}

	in.Tags = nil
	objs, _ = crossplaneaws.KarpenterNodeGroupObjects(in)
	class = objectNamed(t, objs, "EC2NodeClass", crossplaneaws.PoolPlatform)
	if _, found, _ := unstructured.NestedFieldNoCopy(class.Object, "spec", "tags"); found {
		t.Error("spec.tags without tags")
	}
}
