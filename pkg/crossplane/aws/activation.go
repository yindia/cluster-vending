package aws

import (
	"reflect"

	apiextv1alpha1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1alpha1"
	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awsevents "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchevents/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	awssqs "github.com/upbound/provider-aws/v2/apis/namespaced/sqs/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	// Network flow logs (also roles, policies and their attachments, below).
	"flowlogs.ec2.aws.m.upbound.io", "groups.cloudwatchlogs.aws.m.upbound.io",
	// Network ACLs of imported networks.
	"defaultnetworkacls.ec2.aws.m.upbound.io", "networkacls.ec2.aws.m.upbound.io",
	"networkaclrules.ec2.aws.m.upbound.io",
	// Clusters (ClusterResources).
	"roles.iam.aws.m.upbound.io", "rolepolicyattachments.iam.aws.m.upbound.io", "policies.iam.aws.m.upbound.io",
	"clusters.eks.aws.m.upbound.io", "nodegroups.eks.aws.m.upbound.io", "addons.eks.aws.m.upbound.io",
	"podidentityassociations.eks.aws.m.upbound.io", "accessentries.eks.aws.m.upbound.io",
	"accesspolicyassociations.eks.aws.m.upbound.io", "clusterauths.eks.aws.m.upbound.io",
	// Cluster options: secrets encryption, IRSA, a rendered launch template
	// (the control plane's log group is groups.cloudwatchlogs, above).
	"keys.kms.aws.m.upbound.io", "aliases.kms.aws.m.upbound.io",
	"openidconnectproviders.iam.aws.m.upbound.io", "launchtemplates.ec2.aws.m.upbound.io",
	// Carried over from an imported cluster.
	"identityproviderconfigs.eks.aws.m.upbound.io", "securitygroups.ec2.aws.m.upbound.io",
	"securitygroupingressrules.ec2.aws.m.upbound.io", "securitygroupegressrules.ec2.aws.m.upbound.io",
	// Karpenter's interruption queue and the EventBridge rules feeding it
	// (components; nothing renders them yet).
	"queues.sqs.aws.m.upbound.io", "queuepolicies.sqs.aws.m.upbound.io",
	"rules.cloudwatchevents.aws.m.upbound.io", "targets.cloudwatchevents.aws.m.upbound.io",
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

// ManagedObjects are empty objects of every kind the platform creates in
// tenant namespaces (networks' and clusters' managed resources, and the
// Usages ordering their deletion), for watches and caches. Usage is a
// Crossplane core kind, not an MRD: it needs no activation.
func ManagedObjects() []client.Object {
	all := append(NetworkObjects(), &protectionv1beta1.Usage{},
		&awsiam.Role{}, &awsiam.RolePolicyAttachment{}, &awsiam.Policy{},
		&awseks.Cluster{}, &awseks.NodeGroup{}, &awseks.Addon{}, &awseks.PodIdentityAssociation{},
		&awseks.AccessEntry{}, &awseks.AccessPolicyAssociation{}, &awseks.ClusterAuth{},
		&awscwl.Group{}, &awskms.Key{}, &awskms.Alias{}, &awsiam.OpenIDConnectProvider{}, &awsec2.LaunchTemplate{},
		&awseks.IdentityProviderConfig{}, &awsec2.SecurityGroup{}, &awsec2.SecurityGroupIngressRule{},
		&awsec2.SecurityGroupEgressRule{},
		&awssqs.Queue{}, &awssqs.QueuePolicy{}, &awsevents.Rule{}, &awsevents.Target{},
	)
	// Networks and clusters share kinds (IAM): each once.
	out := make([]client.Object, 0, len(all))
	seen := map[reflect.Type]bool{}
	for _, obj := range all {
		if t := reflect.TypeOf(obj); !seen[t] {
			seen[t] = true
			out = append(out, obj)
		}
	}
	return out
}
