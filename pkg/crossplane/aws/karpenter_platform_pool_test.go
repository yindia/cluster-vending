package aws_test

import (
	"slices"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The platform pool runs small, general burstable nodes: the family and
// sizes are the platform's own named constants.
func TestPlatformNodePoolIsT3MediumToXlarge(t *testing.T) {
	objs, err := crossplaneaws.KarpenterNodeGroupObjects(nodeGroupsInput())
	if err != nil {
		t.Fatal(err)
	}
	pool := objectNamed(t, objs, "NodePool", "platform")

	if got := reqValues(t, pool, "karpenter.k8s.aws/instance-family"); !slices.Equal(got, crossplaneaws.PlatformInstanceFamilies) ||
		!slices.Equal(got, []string{"t3"}) {
		t.Errorf("families %v", got)
	}
	if got := reqValues(t, pool, "karpenter.k8s.aws/instance-size"); !slices.Equal(got, crossplaneaws.PlatformInstanceSizes) ||
		!slices.Equal(got, []string{"medium", "large", "xlarge"}) {
		t.Errorf("sizes %v", got)
	}
	for _, key := range []string{"karpenter.k8s.aws/instance-category", "karpenter.k8s.aws/instance-generation"} {
		if got := reqValues(t, pool, key); got != nil {
			t.Errorf("%s %v must not be set next to the family", key, got)
		}
	}
	if got := reqValues(t, pool, "kubernetes.io/arch"); !slices.Equal(got, []string{"amd64"}) {
		t.Errorf("arch %v", got)
	}
	if got := reqValues(t, pool, "karpenter.sh/capacity-type"); !slices.Equal(got, []string{"on-demand"}) {
		t.Errorf("capacity %v", got)
	}
}

// User groups keep the default categories (the admin allowlist is checked
// by the server for them); the platform pool is not an allowlist's concern.
func TestUserNodePoolKeepsDefaultCategories(t *testing.T) {
	in := nodeGroupsInput(&cloudv1.NodeGroupSpec{Name: "batch"})
	objs, _ := crossplaneaws.KarpenterNodeGroupObjects(in)
	pool := objectNamed(t, objs, "NodePool", "batch")
	if got := reqValues(t, pool, "karpenter.k8s.aws/instance-category"); !slices.Equal(got, []string{"c", "m", "r"}) {
		t.Errorf("categories %v", got)
	}
	if got := reqValues(t, pool, "karpenter.k8s.aws/instance-size"); got != nil {
		t.Errorf("sizes %v leaked from the platform pool", got)
	}
}
