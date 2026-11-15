package aws

import (
	"context"
	"slices"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// policied is a managed resource whose management policies can be read.
type policied interface {
	managed
	GetManagementPolicies() xpv2.ManagementPolicies
}

// stateful reports whether obj is of a kind whose cloud resource holds
// state that is costly or impossible to get back: VPC, subnets, NAT and
// internet gateways, EIPs, route tables, the EKS cluster and its node
// groups, KMS keys, IAM roles and policies, the OIDC provider, log groups.
func stateful(obj client.Object) bool {
	switch obj.(type) {
	case *awsec2.VPC, *awsec2.Subnet, *awsec2.NATGateway, *awsec2.EIP, *awsec2.InternetGateway, *awsec2.RouteTable,
		*awseks.Cluster, *awseks.NodeGroup, *awskms.Key, *awsiam.Role, *awsiam.Policy, *awsiam.OpenIDConnectProvider,
		*awscwl.Group:
		return true
	}
	return false
}

// protect makes a stateful object the platform fully manages (no
// policies, or "*") managed without Delete (PolicyNoDelete): deleting the
// object by anything but its owner's deletion (managed.Client.Delete, which
// switches it back first) leaves its cloud resource. Imported objects keep
// their own policies.
func protect(obj client.Object) {
	m, ok := obj.(policied)
	if !ok || !stateful(obj) {
		return
	}
	p := m.GetManagementPolicies()
	if len(p) == 0 || slices.Equal(p, PolicyAll) {
		m.SetManagementPolicies(slices.Clone(PolicyNoDelete))
	}
}

// protected is set with its stateful objects protected (protect), those
// a Build makes too.
func protected(set sequence.Set) sequence.Set {
	out := make(sequence.Set, 0, len(set))
	for _, item := range set {
		protect(item.Object)
		if build := item.Build; build != nil {
			item.Build = func(ctx context.Context, seen sequence.Observed) (client.Object, string, error) {
				obj, waits, err := build(ctx, seen)
				if obj != nil {
					protect(obj)
				}
				return obj, waits, err
			}
		}
		out = append(out, item)
	}
	return out
}
