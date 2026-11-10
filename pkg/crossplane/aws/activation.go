package aws

import (
	apiextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// ActivationPolicyName is the platform's ManagedResourceActivationPolicy.
const ActivationPolicyName = "cluster-vending"

// managedMRDs are the managed resource definitions (MRDs) of every kind
// the platform creates, named as the providers publish them (kubectl get
// managedresourcedefinitions), plus provider-helm's namespaced Release.
// Kept in step with the builders by TestActivationPolicy.
var managedMRDs = []string{
	// Networks (NetworkResources, ImportedNetworkResources).
	"vpcs.ec2.aws.m.upbound.io", "internetgateways.ec2.aws.m.upbound.io", "subnets.ec2.aws.m.upbound.io",
	"eips.ec2.aws.m.upbound.io", "natgateways.ec2.aws.m.upbound.io", "routetables.ec2.aws.m.upbound.io",
	"routes.ec2.aws.m.upbound.io", "routetableassociations.ec2.aws.m.upbound.io",
	// Clusters (ClusterResources).
	"roles.iam.aws.m.upbound.io", "rolepolicyattachments.iam.aws.m.upbound.io", "policies.iam.aws.m.upbound.io",
	"clusters.eks.aws.m.upbound.io", "nodegroups.eks.aws.m.upbound.io", "addons.eks.aws.m.upbound.io",
	"podidentityassociations.eks.aws.m.upbound.io", "accessentries.eks.aws.m.upbound.io",
	"accesspolicyassociations.eks.aws.m.upbound.io", "clusterauths.eks.aws.m.upbound.io",
	// provider-helm.
	"releases.helm.m.crossplane.io",
}

// ActivationPolicy activates managedMRDs and nothing else: the AWS
// providers define ~280 kinds, and every active one is a CRD the API
// server holds and a controller the provider runs. Crossplane's chart
// activates all of them ("*") unless its default policy is disabled
// (provider.defaultActivations=null, see hack/start.sh).
func ActivationPolicy() *apiextv1alpha1.ManagedResourceActivationPolicy {
	activations := make([]apiextv1alpha1.ActivationPolicy, 0, len(managedMRDs))
	for _, n := range managedMRDs {
		activations = append(activations, apiextv1alpha1.ActivationPolicy(n))
	}
	return &apiextv1alpha1.ManagedResourceActivationPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: apiextv1alpha1.SchemeGroupVersion.String(),
			Kind: apiextv1alpha1.ManagedResourceActivationPolicyKind},
		ObjectMeta: metav1.ObjectMeta{Name: ActivationPolicyName, Labels: crossplane.Labels("")},
		Spec:       apiextv1alpha1.ManagedResourceActivationPolicySpec{Activations: activations},
	}
}
