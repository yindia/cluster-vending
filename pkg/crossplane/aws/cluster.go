package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awscwl "github.com/upbound/provider-aws/v2/apis/namespaced/cloudwatchlogs/v1beta1"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	awskms "github.com/upbound/provider-aws/v2/apis/namespaced/kms/v1beta1"
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

	// Everything below is optional; the zero value renders what a cluster
	// rendered before these existed.

	// EndpointPrivateAccess and EndpointPublicAccess turn the API server's
	// endpoints on or off; nil is on.
	EndpointPrivateAccess, EndpointPublicAccess *bool
	// AdditionalSecurityGroupIDs are extra security groups on the control
	// plane's network interfaces.
	AdditionalSecurityGroupIDs []string
	// LogGroupRetentionDays, above 0, renders the control plane's log group
	// with that retention.
	LogGroupRetentionDays int
	// CreateEncryptionKey renders a KMS key and its alias for the secrets
	// encryption. EncryptionKeyARN is the key the EKS cluster encrypts
	// secrets with: an existing key's ARN, or the rendered key's once it
	// exists (the provider has no reference for it). Empty with
	// CreateEncryptionKey: the EKS cluster is not rendered yet (Pending).
	CreateEncryptionKey bool
	EncryptionKeyARN    string
	// ClusterHeld leaves the EKS cluster out until what must exist before
	// it does (the log group, the key).
	ClusterHeld bool
	// AccessEntries are the access entries beyond AdminPrincipals.
	AccessEntries []AccessEntryPlan
	// IRSA renders the IAM OIDC provider of the cluster's issuer, once
	// OIDCIssuer (the EKS cluster's, known when it is ACTIVE) is set.
	IRSA       bool
	OIDCIssuer string
	// ClusterSecurityGroupID is the security group EKS created for the
	// cluster (known once it is ACTIVE): a rendered launch template with
	// security groups lists it next to them.
	ClusterSecurityGroupID string
	// Adoption is what an imported cluster keeps of its IAM roles; zero for
	// a cluster the platform created.
	Adoption
}

// Adoption is what an imported cluster keeps of the resources it was
// imported from that its spec cannot say. Stored with the cluster.
type Adoption struct {
	// ClusterRoleName and NodeRoleName are the IAM names of the cluster and
	// node roles (their external names); empty is the object's name,
	// "<prefix>-cluster-role" and "<prefix>-node-role".
	ClusterRoleName string `json:"clusterRoleName,omitempty"`
	NodeRoleName    string `json:"nodeRoleName,omitempty"`
	// ClusterRoleTrust and NodeRoleTrust are the roles' trust policies;
	// empty is the platform's (EKS, EC2).
	ClusterRoleTrust string `json:"clusterRoleTrust,omitempty"`
	NodeRoleTrust    string `json:"nodeRoleTrust,omitempty"`
	// ExternalNames are the external names of the rendered objects that
	// adopted an existing cloud resource, by object name: the node group's
	// name, "<cluster>:<add-on>", ... Rendered with every apply, or the
	// object would lose its cloud resource.
	ExternalNames map[string]string `json:"externalNames,omitempty"`
	// BootstrapAdmin is the EKS cluster's
	// accessConfig.bootstrapClusterCreatorAdminPermissions as it was
	// created (EKS cannot change it); nil is true, what the platform
	// creates clusters with.
	BootstrapAdmin *bool `json:"bootstrapAdmin,omitempty"`
}

// AccessEntryPlan is one access entry with its policies.
type AccessEntryPlan struct {
	Principal string
	// Type is the EKS spelling: STANDARD, EC2_LINUX, ...
	Type     string
	Policies []AccessPolicyPlan
}

// AccessPolicyPlan associates an access policy with an entry. Scope is
// "cluster" or "namespace" (then with Namespaces).
type AccessPolicyPlan struct {
	PolicyARN, Scope string
	Namespaces       []string
}

// LaunchTemplatePlan is the node group's launch template: an existing one
// (ID or Name, and Version) or a rendered one (SecurityGroupIDs, ImageID
// with UserData).
type LaunchTemplatePlan struct {
	ID, Name, Version string
	SecurityGroupIDs  []string
	ImageID, UserData string
}

// Rendered reports whether the platform renders the template.
func (l *LaunchTemplatePlan) Rendered() bool { return l != nil && l.ID == "" && l.Name == "" }

// identity is what a node group cannot change in place about its launch
// template: which existing template, or the rendered one's content.
func (l *LaunchTemplatePlan) identity() string {
	switch {
	case l == nil:
		return ""
	case !l.Rendered():
		return "existing:" + l.ID + l.Name
	}
	groups := slices.Clone(l.SecurityGroupIDs)
	slices.Sort(groups)
	sum := sha256.Sum256(fmt.Appendf(nil, "%v|%s|%s", groups, l.ImageID, l.UserData))
	return "rendered:" + hex.EncodeToString(sum[:])[:12]
}

// NodePoolPlan is the default node group.
type NodePoolPlan struct {
	InstanceTypes             []string
	CapacityType, AMIType     string
	DiskSizeGB                int
	MinSize, MaxSize, Desired int
	Labels                    map[string]string
	Taints                    []TaintPlan
	// LaunchTemplate, when set, is the node group's launch template.
	LaunchTemplate *LaunchTemplatePlan
}

// TaintPlan is a node taint; Effect is the EKS spelling (NO_SCHEDULE, ...).
type TaintPlan struct{ Key, Value, Effect string }

// ClusterPrefix is "<name>-<hash8>", like NetworkPrefix.
func ClusterPrefix(tenantID, clusterID, name string) string {
	return NetworkPrefix(tenantID, clusterID, name)
}

// ClusterResources renders the plan, in creation order: what must exist
// before the EKS cluster (its log group, its encryption key), the cluster
// and node roles with their policies, the EKS cluster, the default node
// group (after its launch template), the add-ons, the EBS CSI driver's
// role and pod identity, one access entry with cluster-admin per admin
// principal, the other access entries and the IAM OIDC provider. Objects
// that cannot be rendered yet (Pending) are left out, with the Usages that
// refer to them.
func ClusterResources(p ClusterPlan) []client.Object {
	b := clusterBuilder{p: p}
	addons := b.addons()
	out := make([]client.Object, 0, 8+len(addons)+2*len(p.AdminPrincipals))
	out = append(out, b.beforeCluster()...)
	out = append(out,
		b.roleNamed("cluster-role", orDefault(p.ClusterRoleTrust, trustEKS), p.ClusterRoleName),
		b.attach("cluster-policy", "cluster-role", policyEKSCluster),
		b.roleNamed("node-role", orDefault(p.NodeRoleTrust, trustEC2), p.NodeRoleName),
		b.attach("node-worker", "node-role", policyWorkerNode), b.attach("node-cni", "node-role", policyCNI),
		b.attach("node-ecr", "node-role", policyECRRead),
		b.cluster(),
	)
	if lt := b.launchTemplate(); lt != nil {
		out = append(out, lt)
	}
	out = append(out, b.nodeGroup())
	out = append(out, addons...)
	out = append(out, b.clusterAuth(), b.helmProviderConfig())
	for i, principal := range p.AdminPrincipals {
		entry := fmt.Sprintf("admin-%d", i+1)
		out = append(out, b.accessEntry(entry, principal), b.accessPolicy(entry, principal))
	}
	out = append(out, b.accessEntries()...)
	if p.IRSA {
		out = append(out, b.oidcProvider())
	}
	out = b.withoutPending(out)
	for _, obj := range out {
		if name, ok := p.ExternalNames[obj.GetName()]; ok {
			annotations := obj.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[ExternalNameAnnotation] = name
			obj.SetAnnotations(annotations)
		}
	}
	// Last: Usages refer to the objects above.
	rendered := map[string]bool{}
	for _, obj := range out {
		rendered[obj.GetName()] = true
	}
	for _, u := range Usages(p.Namespace, p.Prefix, crossplane.ClusterLabels(p.TenantID, p.ClusterID), b.usagePairs()) {
		if rendered[u.Spec.Of.ResourceRef.Name] && rendered[u.Spec.By.ResourceRef.Name] || isSubnetUsage(u) && rendered[u.Spec.By.ResourceRef.Name] {
			out = append(out, u)
		}
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
			EndpointPrivateAccess: ptr.To(ptr.Deref(p.EndpointPrivateAccess, true)),
			EndpointPublicAccess:  ptr.To(ptr.Deref(p.EndpointPublicAccess, true)),
			SecurityGroupIds:      ptrs(p.AdditionalSecurityGroupIDs),
		},
		AccessConfig: &awseks.AccessConfigParameters{
			AuthenticationMode:                      ptr.To(p.AuthenticationMode),
			BootstrapClusterCreatorAdminPermissions: ptr.To(ptr.Deref(p.BootstrapAdmin, true)),
		},
		EnabledClusterLogTypes: ptrs(p.LogTypes),
		Tags:                   b.tags(p.ClusterName),
	}
	if ptr.Deref(p.EndpointPublicAccess, true) {
		params.VPCConfig.PublicAccessCidrs = ptrs(p.PublicAccessCIDRs)
	}
	if p.EncryptionKeyARN != "" {
		params.EncryptionConfig = &awseks.EncryptionConfigParameters{
			Provider: &awseks.ProviderParameters{KeyArn: ptr.To(p.EncryptionKeyARN)}, Resources: ptrs([]string{"secrets"})}
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
	settings := fmt.Appendf(nil, "%v|%s|%s|%d", types, n.CapacityType, n.AMIType, n.DiskSizeGB)
	if lt := n.LaunchTemplate.identity(); lt != "" {
		settings = fmt.Appendf(settings, "|%s", lt)
	}
	sum := sha256.Sum256(settings)
	return DefaultNodeGroupName(prefix) + "-" + hex.EncodeToString(sum[:])[:6]
}

// LaunchTemplateAnnotation, on a NodeGroup object, is the identity of the
// launch template it was rendered with (which existing template, or the
// rendered one's content): what tells a template change from the object.
const LaunchTemplateAnnotation = "cluster-vending.io/launch-template"

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
		ptr.Deref(fp.AMIType, "") == n.AMIType && disk == n.DiskSizeGB &&
		ng.Annotations[LaunchTemplateAnnotation] == n.LaunchTemplate.identity()
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
	// A custom AMI brings its own Kubernetes version: EKS refuses one here.
	if v := b.nodeVersion(); v != "" && n.AMIType != amiTypeCustom {
		params.Version = ptr.To(v)
	}
	meta := b.meta(name)
	if lt := n.LaunchTemplate; lt != nil {
		meta.Annotations = map[string]string{LaunchTemplateAnnotation: lt.identity()}
		params.LaunchTemplate = &awseks.LaunchTemplateParameters{ID: optional(lt.ID), Name: optional(lt.Name), Version: ptr.To(lt.Version)}
		if lt.Rendered() {
			// A rendered template is never changed (a change renders a new
			// one, with a new node group): its only version is 1.
			params.LaunchTemplate = &awseks.LaunchTemplateParameters{Name: ptr.To(LaunchTemplateName(name)), Version: ptr.To("1")}
		}
	}
	if n.DiskSizeGB > 0 {
		params.DiskSize = ptr.To(float64(n.DiskSizeGB))
	}
	for _, t := range n.Taints {
		params.Taint = append(params.Taint, awseks.TaintParameters{Key: ptr.To(t.Key), Value: ptr.To(t.Value), Effect: ptr.To(t.Effect)})
	}
	return &awseks.NodeGroup{
		ObjectMeta: meta,
		Spec:       awseks.NodeGroupSpec{ManagedResourceSpec: b.spec(), ForProvider: params},
	}
}

func (b clusterBuilder) accessEntry(suffix, principal string) *awseks.AccessEntry {
	return b.accessEntryOf(suffix, principal, accessEntryStandard)
}

func (b clusterBuilder) accessEntryOf(suffix, principal, entryType string) *awseks.AccessEntry {
	name := b.name(suffix)
	return &awseks.AccessEntry{
		ObjectMeta: b.meta(name),
		Spec: awseks.AccessEntrySpec{ManagedResourceSpec: b.spec(), ForProvider: awseks.AccessEntryParameters{
			Region: ptr.To(b.p.Region), ClusterNameRef: b.ref(b.p.Prefix), PrincipalArn: ptr.To(principal),
			Type: ptr.To(entryType), Tags: b.tags(name),
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

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
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
		&awseks.AddonList{}, &awseks.ClusterAuthList{}, &awseks.NodeGroupList{}, &awsec2.LaunchTemplateList{},
		&awsiam.OpenIDConnectProviderList{}, &awseks.IdentityProviderConfigList{}, &awseks.ClusterList{},
		&awsec2.SecurityGroupIngressRuleList{}, &awsec2.SecurityGroupEgressRuleList{}, &awsec2.SecurityGroupList{},
		&awsiam.RolePolicyAttachmentList{}, &awsiam.PolicyList{}, &awsiam.RoleList{},
		&awskms.AliasList{}, &awskms.KeyList{}, &awscwl.GroupList{},
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
