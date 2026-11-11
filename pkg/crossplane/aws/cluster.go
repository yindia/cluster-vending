package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// Tags the platform puts on a cluster's AWS resources.
const (
	TagCluster     = "cluster-vending.io/cluster"
	TagClusterName = "cluster-vending.io/cluster-name"
)

// AWS managed policies the cluster's roles use.
const (
	policyEKSCluster = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
	policyWorkerNode = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
	policyCNI        = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
	policyECRRead    = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
	policyEBSCSI     = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
	policyAdmin      = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
)

// Trust policies of the cluster's roles.
const (
	trustEKS         = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"eks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	trustEC2         = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	trustPodIdentity = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"pods.eks.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`
)

// ClusterPlan is a cluster resolved to concrete values: defaults applied,
// the network's subnets chosen. ClusterResources only renders it.
type ClusterPlan struct {
	Namespace, ProviderConfigName, TenantID string
	// ClusterID is the cloud.v1 Cluster.id (labels, tags). ClusterName, the
	// client's name, is the EKS cluster's name (its external name).
	// Prefix ("<name>-<hash8>") starts every Kubernetes object and IAM
	// role name: IAM names are global to the account, so the same cluster
	// name in two regions must not share roles.
	ClusterID, ClusterName, Prefix, Region string

	// KubernetesVersion is "1.x"; empty takes the EKS default.
	KubernetesVersion string
	// SubnetIDs are the network's private subnets in the cluster's zones.
	SubnetIDs []string
	// PublicAccessCIDRs is never empty (["0.0.0.0/0"] for unrestricted).
	PublicAccessCIDRs  []string
	AuthenticationMode string
	LogTypes           []string
	// SupportType is the upgrade policy ("STANDARD", "EXTENDED"); empty
	// takes the EKS default.
	SupportType string
	ServiceCIDR string
	Tags        map[string]string

	NodePool        NodePoolPlan
	AdminPrincipals []string
	// Addons are the cluster's add-ons; empty renders DefaultAddonPlans.
	Addons []AddonPlan
	// NodeGroupName names the default node group object (and EKS node
	// group); empty is DefaultNodeGroupName.
	NodeGroupName string
	// NodeKubernetesVersion, when set, is the node groups' version instead
	// of KubernetesVersion: during an upgrade they follow the control
	// plane, not the target (EKS refuses nodes newer than the cluster).
	NodeKubernetesVersion string
	// SubnetObjects are the network's Subnet objects of SubnetIDs: the
	// cluster's Usages keep them while the EKS cluster exists.
	SubnetObjects []string
}

// NodePoolPlan is the default node group.
type NodePoolPlan struct {
	InstanceTypes             []string
	CapacityType, AMIType     string
	DiskSizeGB                int
	MinSize, MaxSize, Desired int
	Labels                    map[string]string
	Taints                    []TaintPlan
}

// TaintPlan is a node taint; Effect is the EKS spelling (NO_SCHEDULE, ...).
type TaintPlan struct{ Key, Value, Effect string }

// ClusterPrefix is "<name>-<hash8>", like NetworkPrefix.
func ClusterPrefix(tenantID, clusterID, name string) string {
	return NetworkPrefix(tenantID, clusterID, name)
}

// ClusterResources renders the plan, in creation order: the cluster and
// node roles with their policies, the EKS cluster, the default node group,
// the add-ons, the EBS CSI driver's role and pod identity, and one access
// entry with cluster-admin per admin principal.
func ClusterResources(p ClusterPlan) []client.Object {
	b := clusterBuilder{p: p}
	addons := b.addons()
	out := make([]client.Object, 0, 8+len(addons)+2*len(p.AdminPrincipals))
	out = append(out,
		b.role("cluster-role", trustEKS), b.attach("cluster-policy", "cluster-role", policyEKSCluster),
		b.role("node-role", trustEC2),
		b.attach("node-worker", "node-role", policyWorkerNode), b.attach("node-cni", "node-role", policyCNI),
		b.attach("node-ecr", "node-role", policyECRRead),
		b.cluster(), b.nodeGroup(),
	)
	out = append(out, addons...)
	out = append(out, b.clusterAuth())
	for i, principal := range p.AdminPrincipals {
		entry := fmt.Sprintf("admin-%d", i+1)
		out = append(out, b.accessEntry(entry, principal), b.accessPolicy(entry, principal))
	}
	// Last: Usages refer to the objects above.
	for _, u := range Usages(p.Namespace, p.Prefix, crossplane.ClusterLabels(p.TenantID, p.ClusterID), b.usagePairs()) {
		out = append(out, u)
	}
	return out
}

type clusterBuilder struct{ p ClusterPlan }

func (b clusterBuilder) name(suffix string) string { return b.p.Prefix + "-" + suffix }

func (b clusterBuilder) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: b.p.Namespace, Labels: crossplane.ClusterLabels(b.p.TenantID, b.p.ClusterID)}
}

func (b clusterBuilder) spec() xpv2.ManagedResourceSpec {
	return xpv2.ManagedResourceSpec{ProviderConfigReference: ProviderConfigRef(b.p.ProviderConfigName)}
}

func (b clusterBuilder) tags(name string) map[string]*string {
	out := map[string]*string{}
	for k, v := range b.p.Tags {
		out[k] = ptr.To(v)
	}
	maps.Copy(out, map[string]*string{
		TagName: ptr.To(name), TagTenant: ptr.To(b.p.TenantID),
		TagCluster: ptr.To(b.p.ClusterID), TagClusterName: ptr.To(b.p.ClusterName),
		// The EKS cluster name: finds the cluster's own resources (node
		// role, ...) when Karpenter is set up.
		TagKarpenterDiscovery: ptr.To(b.p.ClusterName),
	})
	return out
}

func (b clusterBuilder) ref(name string) *xpv2.NamespacedReference {
	return &xpv2.NamespacedReference{Name: name}
}

func (b clusterBuilder) role(suffix, trust string) *awsiam.Role {
	name := b.name(suffix)
	return &awsiam.Role{
		ObjectMeta: b.meta(name),
		Spec: awsiam.RoleSpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.RoleParameters{
			AssumeRolePolicy: ptr.To(trust), Tags: b.tags(name),
		}},
	}
}

func (b clusterBuilder) attach(suffix, role, policy string) *awsiam.RolePolicyAttachment {
	return &awsiam.RolePolicyAttachment{
		ObjectMeta: b.meta(b.name(suffix)),
		Spec: awsiam.RolePolicyAttachmentSpec{ManagedResourceSpec: b.spec(), ForProvider: awsiam.RolePolicyAttachmentParameters{
			PolicyArn: ptr.To(policy), RoleRef: b.ref(b.name(role)),
		}},
	}
}

func (b clusterBuilder) cluster() *awseks.Cluster {
	p := b.p
	params := awseks.ClusterParameters{
		Region:     ptr.To(p.Region),
		RoleArnRef: b.ref(b.name("cluster-role")),
		VPCConfig: &awseks.VPCConfigParameters{
			SubnetIds:             ptrs(p.SubnetIDs),
			EndpointPrivateAccess: ptr.To(true),
			EndpointPublicAccess:  ptr.To(true),
			PublicAccessCidrs:     ptrs(p.PublicAccessCIDRs),
		},
		AccessConfig: &awseks.AccessConfigParameters{
			AuthenticationMode:                      ptr.To(p.AuthenticationMode),
			BootstrapClusterCreatorAdminPermissions: ptr.To(true),
		},
		EnabledClusterLogTypes: ptrs(p.LogTypes),
		Tags:                   b.tags(p.ClusterName),
	}
	if p.KubernetesVersion != "" {
		params.Version = ptr.To(p.KubernetesVersion)
	}
	if p.SupportType != "" {
		params.UpgradePolicy = &awseks.UpgradePolicyParameters{SupportType: ptr.To(p.SupportType)}
	}
	if p.ServiceCIDR != "" {
		params.KubernetesNetworkConfig = &awseks.KubernetesNetworkConfigParameters{ServiceIPv4Cidr: ptr.To(p.ServiceCIDR)}
	}
	meta := b.meta(p.Prefix)
	// The object is named like the others; the EKS cluster is named by the
	// client. References to it (node group, add-ons, ...) resolve to this
	// external name.
	meta.Annotations = map[string]string{ExternalNameAnnotation: p.ClusterName}
	return &awseks.Cluster{ObjectMeta: meta, Spec: awseks.ClusterSpec{ManagedResourceSpec: b.spec(), ForProvider: params}}
}

// nodeGroup is the default node group. The desired size is set once, at
// creation (initProvider): the cluster autoscaler owns it afterwards.
// KubeconfigSecretKey is the key of the kubeconfig in the ClusterAuth
// connection Secret.
const KubeconfigSecretKey = "kubeconfig"

// KubeconfigSecretName is the Secret, in the tenant namespace, the
// cluster's ClusterAuth writes its kubeconfig to.
func KubeconfigSecretName(prefix string) string { return prefix + "-kubeconfig" }

// clusterAuth publishes the cluster's kubeconfig (token refreshed every 10
// minutes, as the ProviderConfig's identity) for in-cluster providers
// such as provider-helm.
func (b clusterBuilder) clusterAuth() *awseks.ClusterAuth {
	spec := b.spec()
	spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: KubeconfigSecretName(b.p.Prefix)}
	return &awseks.ClusterAuth{
		ObjectMeta: b.meta(b.name("auth")),
		Spec: awseks.ClusterAuthSpec{ManagedResourceSpec: spec, ForProvider: awseks.ClusterAuthParameters{
			Region: b.p.Region, ClusterNameRef: b.ref(b.p.Prefix),
		}},
	}
}

// DefaultNodeGroupName is the default node group of a cluster created with
// these settings.
func DefaultNodeGroupName(prefix string) string { return prefix + "-default" }

// ReplacementNodeGroupName names the node group that replaces the default
// one when settings EKS cannot change in place (instance types, capacity
// type, AMI type, disk size) change: "<prefix>-default-<hash6>".
func ReplacementNodeGroupName(prefix string, n NodePoolPlan) string {
	types := slices.Clone(n.InstanceTypes)
	slices.Sort(types)
	sum := sha256.Sum256(fmt.Appendf(nil, "%v|%s|%s|%d", types, n.CapacityType, n.AMIType, n.DiskSizeGB))
	return DefaultNodeGroupName(prefix) + "-" + hex.EncodeToString(sum[:])[:6]
}

// SameNodeGroup reports whether a node group's settings EKS cannot change
// in place equal n's.
func SameNodeGroup(ng *awseks.NodeGroup, n NodePoolPlan) bool {
	fp := ng.Spec.ForProvider
	have := make([]string, 0, len(fp.InstanceTypes))
	for _, t := range fp.InstanceTypes {
		have = append(have, ptr.Deref(t, ""))
	}
	want := slices.Clone(n.InstanceTypes)
	slices.Sort(have)
	slices.Sort(want)
	disk := 0
	if fp.DiskSize != nil {
		disk = int(*fp.DiskSize)
	}
	return slices.Equal(have, want) && ptr.Deref(fp.CapacityType, "") == n.CapacityType &&
		ptr.Deref(fp.AMIType, "") == n.AMIType && disk == n.DiskSizeGB
}

func (b clusterBuilder) nodeGroup() *awseks.NodeGroup {
	n := b.p.NodePool
	name := b.nodeGroupName()
	params := awseks.NodeGroupParameters{
		Region:         ptr.To(b.p.Region),
		ClusterNameRef: b.ref(b.p.Prefix),
		NodeRoleArnRef: b.ref(b.name("node-role")),
		SubnetIds:      ptrs(b.p.SubnetIDs),
		InstanceTypes:  ptrs(n.InstanceTypes),
		CapacityType:   ptr.To(n.CapacityType),
		AMIType:        ptr.To(n.AMIType),
		// desiredSize has no omitempty in the provider types and the CRD
		// requires a number: it is always set, from desired_size or
		// min_size (the API's default).
		ScalingConfig: &awseks.ScalingConfigParameters{MinSize: ptr.To(float64(n.MinSize)),
			MaxSize: ptr.To(float64(n.MaxSize)), DesiredSize: ptr.To(float64(n.Desired))},
		Labels: strPtrs(n.Labels),
		Tags:   b.tags(name),
	}
	if v := b.nodeVersion(); v != "" {
		params.Version = ptr.To(v)
	}
	if n.DiskSizeGB > 0 {
		params.DiskSize = ptr.To(float64(n.DiskSizeGB))
	}
	for _, t := range n.Taints {
		params.Taint = append(params.Taint, awseks.TaintParameters{Key: ptr.To(t.Key), Value: ptr.To(t.Value), Effect: ptr.To(t.Effect)})
	}
	return &awseks.NodeGroup{
		ObjectMeta: b.meta(name),
		Spec:       awseks.NodeGroupSpec{ManagedResourceSpec: b.spec(), ForProvider: params},
	}
}

func (b clusterBuilder) accessEntry(suffix, principal string) *awseks.AccessEntry {
	name := b.name(suffix)
	return &awseks.AccessEntry{
		ObjectMeta: b.meta(name),
		Spec: awseks.AccessEntrySpec{ManagedResourceSpec: b.spec(), ForProvider: awseks.AccessEntryParameters{
			Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix), PrincipalArn: ptr.To(principal),
			Type: ptr.To("STANDARD"), Tags: b.tags(name),
		}},
	}
}

func (b clusterBuilder) accessPolicy(suffix, principal string) *awseks.AccessPolicyAssociation {
	return &awseks.AccessPolicyAssociation{
		ObjectMeta: b.meta(b.name(suffix + "-policy")),
		Spec: awseks.AccessPolicyAssociationSpec{ManagedResourceSpec: b.spec(), ForProvider: awseks.AccessPolicyAssociationParameters{
			Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix), PrincipalArn: ptr.To(principal),
			PolicyArn: ptr.To(policyAdmin), AccessScope: &awseks.AccessScopeParameters{Type: ptr.To("cluster")},
		}},
	}
}

func ptrs(in []string) []*string {
	if len(in) == 0 {
		return nil
	}
	out := make([]*string, len(in))
	for i := range in {
		out[i] = ptr.To(in[i])
	}
	return out
}

func strPtrs(in map[string]string) map[string]*string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*string, len(in))
	for k, v := range in {
		out[k] = ptr.To(v)
	}
	return out
}

// ClusterLists are empty lists of every kind ClusterResources creates, to
// find a cluster's objects by label.
func ClusterLists() []client.ObjectList {
	return []client.ObjectList{
		&awseks.AccessPolicyAssociationList{}, &awseks.AccessEntryList{}, &awseks.PodIdentityAssociationList{},
		&awseks.AddonList{}, &awseks.ClusterAuthList{}, &awseks.NodeGroupList{}, &awseks.ClusterList{},
		&awsiam.RolePolicyAttachmentList{}, &awsiam.PolicyList{}, &awsiam.RoleList{},
	}
}

// DesiredSizeAtCreateOnly moves a NodeGroup's desired size (an apply body,
// see kube.ApplyObject) from forProvider to initProvider: the provider's
// Go type always serializes forProvider.scalingConfig.desiredSize (no
// omitempty), so applying it as is makes Crossplane reset the node count
// to it on every poll, against any autoscaler. In initProvider it is set
// at creation only.
func DesiredSizeAtCreateOnly(u *unstructured.Unstructured) {
	desired, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "forProvider", "scalingConfig", "desiredSize")
	unstructured.RemoveNestedField(u.Object, "spec", "forProvider", "scalingConfig", "desiredSize")
	if found && desired != nil {
		_ = unstructured.SetNestedField(u.Object, desired, "spec", "initProvider", "scalingConfig", "desiredSize")
	}
}

func (b clusterBuilder) nodeVersion() string {
	if b.p.NodeKubernetesVersion != "" {
		return b.p.NodeKubernetesVersion
	}
	return b.p.KubernetesVersion
}
