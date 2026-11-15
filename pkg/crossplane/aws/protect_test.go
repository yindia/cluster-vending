package aws

import (
	"context"
	"slices"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// Stateful objects are managed without Delete, those a Build makes too;
// others, and objects with policies of their own (imports), are not
// changed.
func TestProtected(t *testing.T) {
	vpc, route := &awsec2.VPC{}, &awsec2.Route{}
	observed := &awsec2.Subnet{}
	observed.Spec.ManagementPolicies = PolicyObserve
	built := &awseks.NodeGroup{}
	set := protected(sequence.Set{{Object: vpc}, {Object: route}, {Object: observed},
		{Object: &awseks.NodeGroup{}, Build: func(context.Context, sequence.Observed) (client.Object, string, error) { return built, "", nil }}})

	if _, _, err := set[3].Build(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vpc.Spec.ManagementPolicies, PolicyNoDelete) || !slices.Equal(built.Spec.ManagementPolicies, PolicyNoDelete) {
		t.Errorf("vpc %v, built node group %v: want no Delete", vpc.Spec.ManagementPolicies, built.Spec.ManagementPolicies)
	}
	if len(route.Spec.ManagementPolicies) != 0 || !slices.Equal(observed.Spec.ManagementPolicies, PolicyObserve) {
		t.Errorf("route %v, observed subnet %v: want unchanged", route.Spec.ManagementPolicies, observed.Spec.ManagementPolicies)
	}
}
