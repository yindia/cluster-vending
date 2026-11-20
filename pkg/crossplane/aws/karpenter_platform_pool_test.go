package aws_test

import (
	"slices"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// User groups keep the default categories (the admin allowlist is checked
// by the server for them).
func TestUserNodePoolKeepsDefaultCategories(t *testing.T) {
	in := nodeGroupsInput(&cloudv1.NodeGroupSpec{Name: "batch"})
	objs, _ := crossplaneaws.KarpenterNodeGroupObjects(in)
	pool := objectNamed(t, objs, "NodePool", "batch")
	if got := reqValues(t, pool, "karpenter.k8s.aws/instance-category"); !slices.Equal(got, []string{"c", "m", "r"}) {
		t.Errorf("categories %v", got)
	}
	if got := reqValues(t, pool, "karpenter.k8s.aws/instance-size"); got != nil {
		t.Errorf("sizes %v set on a user pool", got)
	}
}
