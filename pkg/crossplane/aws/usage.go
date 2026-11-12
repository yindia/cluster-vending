package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// maxUsageName bounds Usage names (a DNS label: they appear in labels and
// events).
const maxUsageName = 63

// UsageRef names one object a Usage refers to, in the Usage's namespace.
type UsageRef struct {
	APIVersion, Kind, Name string
}

// UsagePair is one deletion dependency: Of cannot be deleted while By
// exists. Crossplane's Usage webhook refuses Of's deletion (409) and,
// with replayDeletion, deletes Of again once By is gone.
type UsagePair struct {
	Of, By UsageRef
}

// Usages renders a protection.crossplane.io Usage per pair, in namespace,
// labelled with labels and named deterministically
// "<prefix>-usage-<of>-by-<by>" (of and by without prefix; truncated and
// hashed to 63 characters). Crossplane owner-references each Usage to its
// By object, so Kubernetes garbage-collects it with By. Reusable for any
// owner: clusters, node pools.
func Usages(namespace, prefix string, labels map[string]string, pairs []UsagePair) []*protectionv1beta1.Usage {
	out := make([]*protectionv1beta1.Usage, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, &protectionv1beta1.Usage{
			TypeMeta:   metav1.TypeMeta{APIVersion: protectionv1beta1.SchemeGroupVersion.String(), Kind: protectionv1beta1.UsageKind},
			ObjectMeta: metav1.ObjectMeta{Name: UsageName(prefix, p), Namespace: namespace, Labels: labels},
			Spec: protectionv1beta1.UsageSpec{
				Of: protectionv1beta1.NamespacedResource{APIVersion: p.Of.APIVersion, Kind: p.Of.Kind,
					ResourceRef: &protectionv1beta1.NamespacedResourceRef{Name: p.Of.Name}},
				By: &protectionv1beta1.Resource{APIVersion: p.By.APIVersion, Kind: p.By.Kind,
					ResourceRef: &protectionv1beta1.ResourceRef{Name: p.By.Name}},
				ReplayDeletion: ptr.To(true),
			},
		})
	}
	return out
}

// UsageName is a pair's Usage name: "<prefix>-usage-<of>-by-<by>", the
// object names without prefix (an object named the prefix itself, the EKS
// cluster, is its kind in lower case); longer than 63 characters it is cut
// and suffixed with a hash of the full name, so it stays unique.
func UsageName(prefix string, p UsagePair) string {
	short := func(r UsageRef) string {
		if r.Name == prefix {
			return strings.ToLower(r.Kind)
		}
		return strings.TrimPrefix(strings.TrimPrefix(r.Name, prefix), "-")
	}
	name := prefix + "-usage-" + short(p.Of) + "-by-" + short(p.By)
	if len(name) <= maxUsageName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:maxUsageName-9], "-.") + "-" + hex.EncodeToString(sum[:])[:8]
}

// Object kinds the cluster's Usages refer to.
var (
	clusterKind     = UsageRef{APIVersion: awseks.CRDGroupVersion.String(), Kind: awseks.Cluster_Kind}
	nodeGroupKind   = UsageRef{APIVersion: awseks.CRDGroupVersion.String(), Kind: awseks.NodeGroup_Kind}
	addonKind       = UsageRef{APIVersion: awseks.CRDGroupVersion.String(), Kind: awseks.Addon_Kind}
	podIdentityKind = UsageRef{APIVersion: awseks.CRDGroupVersion.String(), Kind: awseks.PodIdentityAssociation_Kind}
	clusterAuthKind = UsageRef{APIVersion: awseks.CRDGroupVersion.String(), Kind: awseks.ClusterAuth_Kind}
	roleKind        = UsageRef{APIVersion: awsiam.CRDGroupVersion.String(), Kind: awsiam.Role_Kind}
	attachmentKind  = UsageRef{APIVersion: awsiam.CRDGroupVersion.String(), Kind: awsiam.RolePolicyAttachment_Kind}
	subnetKind      = UsageRef{APIVersion: awsec2.CRDGroupVersion.String(), Kind: awsec2.Subnet_Kind}
	flowLogKind     = UsageRef{APIVersion: awsec2.CRDGroupVersion.String(), Kind: awsec2.FlowLog_Kind}
	kmsKeyKind      = UsageRef{APIVersion: awskms.CRDGroupVersion.String(), Kind: awskms.Key_Kind}
	oidcKind        = UsageRef{APIVersion: awsiam.CRDGroupVersion.String(), Kind: awsiam.OpenIDConnectProvider_Kind}
	launchTmplKind  = UsageRef{APIVersion: awsec2.CRDGroupVersion.String(), Kind: awsec2.LaunchTemplate_Kind}
	logGroupKind    = UsageRef{APIVersion: awscwl.CRDGroupVersion.String(), Kind: awscwl.Group_Kind}
)

func (r UsageRef) named(name string) UsageRef { r.Name = name; return r }

// usagePairs is the cluster's deletion order as Usages (Of cannot go while
// By exists): node groups before the EKS cluster and before their IAM role
// and the kubeconfig the cleanup Job needs; the EKS cluster before its IAM
// role and the network subnets its interfaces sit in; the EBS CSI driver
// add-on before its IAM.
func (b clusterBuilder) usagePairs() []UsagePair {
	p := b.p
	nodeGroup := nodeGroupKind.named(b.nodeGroupName())
	eks := clusterKind.named(p.Prefix)
	pairs := []UsagePair{
		{Of: eks, By: nodeGroup},
		{Of: roleKind.named(b.name("cluster-role")), By: eks},
		{Of: attachmentKind.named(b.name("cluster-policy")), By: eks},
		{Of: roleKind.named(b.name("node-role")), By: nodeGroup},
		{Of: clusterAuthKind.named(b.name("auth")), By: nodeGroup},
		// The kubeconfig Secret the ClusterAuth writes is what the helm
		// ProviderConfig reaches the cluster with: it stays while the
		// ProviderConfig (and, through its in-use finalizer, any Release)
		// exists.
		{Of: clusterAuthKind.named(b.name("auth")), By: helmProviderConfigKind.named(HelmProviderConfigName(p.Prefix))},
	}
	for _, a := range []string{"node-worker", "node-cni", "node-ecr"} {
		pairs = append(pairs, UsagePair{Of: attachmentKind.named(b.name(a)), By: nodeGroup})
	}
	if b.hasAddon(AddonEBSCSI) {
		ebs := addonKind.named(b.name("addon-" + AddonEBSCSI))
		pairs = append(pairs,
			UsagePair{Of: roleKind.named(b.name("ebs-csi-role")), By: ebs},
			UsagePair{Of: attachmentKind.named(b.name("ebs-csi-policy")), By: ebs},
			UsagePair{Of: podIdentityKind.named(b.name("ebs-csi-identity")), By: ebs})
	}
	for _, subnet := range p.SubnetObjects {
		pairs = append(pairs, UsagePair{Of: subnetKind.named(subnet), By: eks})
	}
	return append(pairs, b.optionUsagePairs(eks, nodeGroup)...)
}

func (b clusterBuilder) nodeGroupName() string {
	if b.p.NodeGroupName != "" {
		return b.p.NodeGroupName
	}
	return DefaultNodeGroupName(b.p.Prefix)
}

func (b clusterBuilder) hasAddon(name string) bool {
	if len(b.p.Addons) == 0 {
		return slices.Contains(DefaultAddons, name)
	}
	return slices.ContainsFunc(b.p.Addons, func(a AddonPlan) bool { return a.Name == name })
}
