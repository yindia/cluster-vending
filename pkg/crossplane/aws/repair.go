package aws

import (
	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// recreatable are the kinds of a network's or cluster's objects that may
// be created again when they were deleted from the management cluster
// behind the platform's back (Recreatable).
//
// Listed is what cannot become a second cloud resource next to one that
// survived the object:
//
//   - objects that are no cloud resource: the Helm ProviderConfig, Usages,
//     the ClusterAuth (it only writes the kubeconfig Secret);
//   - cloud resources AWS identifies by a name the renderer chooses: IAM
//     roles and policies and their attachments, log groups, key aliases,
//     node groups, add-ons, access entries and their policy associations,
//     the cluster's OIDC identity provider configuration. Created again,
//     the provider finds a survivor by that name and takes it over, or AWS
//     refuses the duplicate and the object reports it.
//
// Not listed, and so never created again blindly, is every kind whose
// cloud ID AWS assigns, where a new object makes a new resource whatever
// is left of the old one: the encryption key (a second key, while the EKS
// cluster cannot change its key), VPCs, subnets, gateways, addresses,
// route tables, network ACLs, flow logs, security groups and their rules,
// launch templates, pod identity associations, the IAM OIDC provider. The
// EKS cluster itself is not listed either: nothing of a cluster is
// applied while its EKS cluster is not ACTIVE.
var recreatable = map[schema.GroupKind]bool{
	HelmProviderConfigGVK.GroupKind():                                      true,
	protectionv1beta1.SchemeGroupVersion.WithKind("Usage").GroupKind():     true,
	awseks.CRDGroupVersion.WithKind("ClusterAuth").GroupKind():             true,
	awseks.CRDGroupVersion.WithKind("NodeGroup").GroupKind():               true,
	awseks.CRDGroupVersion.WithKind("Addon").GroupKind():                   true,
	awseks.CRDGroupVersion.WithKind("AccessEntry").GroupKind():             true,
	awseks.CRDGroupVersion.WithKind("AccessPolicyAssociation").GroupKind(): true,
	awseks.CRDGroupVersion.WithKind("IdentityProviderConfig").GroupKind():  true,
	awsiam.CRDGroupVersion.WithKind("Role").GroupKind():                    true,
	awsiam.CRDGroupVersion.WithKind("Policy").GroupKind():                  true,
	awsiam.CRDGroupVersion.WithKind("RolePolicyAttachment").GroupKind():    true,
	awscwl.CRDGroupVersion.WithKind("Group").GroupKind():                   true,
	awskms.CRDGroupVersion.WithKind("Alias").GroupKind():                   true,
}

// Recreatable reports whether an object of this API version and kind may
// be created again after it was deleted from the management cluster
// (recreatable has the reasons). Everything else stays missing, and its
// owner reports it.
func Recreatable(apiVersion, kind string) bool {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false
	}
	return recreatable[schema.GroupKind{Group: gv.Group, Kind: kind}]
}

// notByName are the kinds that are never created again even with their
// recorded cloud identity: the EKS cluster, since nothing of a cluster is
// applied while its EKS cluster is not ACTIVE, so its object would not
// come back in a repair and the cluster would wait for it for ever.
var notByName = map[schema.GroupKind]bool{
	awseks.CRDGroupVersion.WithKind("Cluster").GroupKind(): true,
}

// RecreatableByName reports whether an object of this API version and kind
// may be created again with its recorded cloud identity (its
// external name), which makes it take the existing resource over: every
// kind but notByName.
func RecreatableByName(apiVersion, kind string) bool {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false
	}
	return !notByName[schema.GroupKind{Group: gv.Group, Kind: kind}]
}
