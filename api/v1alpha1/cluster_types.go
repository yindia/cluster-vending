package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ReleaseChannelName is the channel a cluster takes platform releases
// from (the ReleaseChannel object of that name, in lower case). Channels
// are ordered: a release goes to Alpha first, then Beta, then Stable.
// Stored only for now: nothing acts on it yet.
// +kubebuilder:validation:Enum=Alpha;Beta;Stable
type ReleaseChannelName string

// Release channels.
const (
	// ReleaseChannelAlpha gets a release first.
	ReleaseChannelAlpha ReleaseChannelName = "Alpha"
	// ReleaseChannelBeta gets a release once Alpha has it.
	ReleaseChannelBeta ReleaseChannelName = "Beta"
	// ReleaseChannelStable gets a release last.
	ReleaseChannelStable ReleaseChannelName = "Stable"
)

// ClusterTier is how sensitive a cluster is. Within a release channel a
// release reaches Tier2 clusters first, then Tier1, then Tier0. Stored
// only for now: nothing acts on it yet.
// +kubebuilder:validation:Enum=Tier0;Tier1;Tier2
type ClusterTier string

// Cluster tiers.
const (
	// ClusterTier0 is the most sensitive: it gets a release last.
	ClusterTier0 ClusterTier = "Tier0"
	// ClusterTier1 gets a release after Tier2 and before Tier0.
	ClusterTier1 ClusterTier = "Tier1"
	// ClusterTier2 is the least sensitive: it gets a release first.
	ClusterTier2 ClusterTier = "Tier2"
)

// UpgradeMode is whether platform releases move a cluster.
// +kubebuilder:validation:Enum=Manual;Automatic
type UpgradeMode string

// Upgrade modes.
const (
	// UpgradeModeManual: the tenant chooses the versions; release channels
	// do not apply and a release never moves the cluster. The default.
	UpgradeModeManual UpgradeMode = "Manual"
	// UpgradeModeAutomatic: the cluster follows its release channel (unset
	// is Stable) and, within it, its tier. Not acted on yet.
	UpgradeModeAutomatic UpgradeMode = "Automatic"
)

// ClusterNetwork is where a cluster runs: its Network and the cluster's
// own address ranges. Immutable as a whole.
type ClusterNetwork struct {
	// networkRef names the Network the cluster runs in, in the same
	// namespace, possibly shared with other clusters. The network owns the
	// cluster; the cluster runs in the network's account and region, in the
	// network's Private subnets of zoneIDs.
	// +required
	NetworkRef LocalReference `json:"networkRef"`
	// zoneIDs are the zones of the network the cluster uses. Empty: every
	// zone of the network. AWS: zone IDs like "use1-az1", at least two.
	// +listType=set
	// +optional
	// +kubebuilder:validation:MaxItems=6
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9-]+$`
	ZoneIDs []string `json:"zoneIDs,omitempty"`
	// podCIDR is the IPv4 range for pod IPs, on providers that give pods
	// their own range. AWS: must be empty (pods take IPs from the subnets).
	// +optional
	// +kubebuilder:validation:MaxLength=18
	// +kubebuilder:validation:Pattern=`^(([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2})?$`
	PodCIDR string `json:"podCIDR,omitempty"`
	// serviceCIDR is the IPv4 range for Kubernetes Service IPs. Defaults to
	// a range that does not overlap the network. AWS: /12 to /24.
	// +optional
	// +kubebuilder:validation:MaxLength=18
	// +kubebuilder:validation:Pattern=`^(([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2})?$`
	ServiceCIDR string `json:"serviceCIDR,omitempty"`
}

// EndpointAccess configures the Kubernetes API server's endpoints. Nodes
// are private. By default the API server has a private and a public
// endpoint, the public one restricted to publicAccessCIDRs.
// +kubebuilder:validation:XValidation:rule="!has(self.privateAccess) || !has(self.publicAccess) || self.privateAccess || self.publicAccess",message="privateAccess and publicAccess cannot both be false"
// +kubebuilder:validation:XValidation:rule="!has(self.publicAccess) || self.publicAccess || !has(self.privateAccess) || self.privateAccess",message="publicAccess false needs privateAccess true (its default)"
type EndpointAccess struct {
	// publicAccessCIDRs are the IPv4 ranges allowed to reach the public
	// endpoint. At least one is required while the public endpoint is on:
	// who may reach the API is the client's choice, there is no default.
	// ["0.0.0.0/0"] allows any address.
	// +listType=set
	// +optional
	// +kubebuilder:validation:MaxItems=40
	// +kubebuilder:validation:items:MaxLength=18
	// +kubebuilder:validation:items:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
	PublicAccessCIDRs []string `json:"publicAccessCIDRs,omitempty"`
	// privateAccess is the API server's endpoint inside the network.
	// Defaults to true. Mutable.
	// +optional
	PrivateAccess *bool `json:"privateAccess,omitempty"`
	// publicAccess is the API server's public endpoint. Defaults to true.
	// Without it the API is reachable from the cluster's network only.
	// Mutable.
	// +optional
	PublicAccess *bool `json:"publicAccess,omitempty"`
}

// SecretsEncryption encrypts Kubernetes Secrets at rest. It can be turned
// on later but never off, and its key never changes: EKS cannot do either.
// +kubebuilder:validation:XValidation:rule="!((has(oldSelf.enabled) && oldSelf.enabled) || has(oldSelf.kmsKeyARN)) || (has(self.enabled) && self.enabled) || has(self.kmsKeyARN)",message="secrets encryption cannot be turned off once it is on"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.kmsKeyARN) || (has(self.kmsKeyARN) && self.kmsKeyARN == oldSelf.kmsKeyARN)",message="kmsKeyARN is immutable once set"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.enabled) && oldSelf.enabled && !has(oldSelf.kmsKeyARN)) || !has(self.kmsKeyARN)",message="secrets encryption cannot move from the platform's key to another one"
type SecretsEncryption struct {
	// enabled turns encryption on with a key the platform creates for the
	// cluster and deletes with it, unless kmsKeyARN names an existing one.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// kmsKeyARN is an existing KMS key to encrypt with instead. The
	// platform does not create, change or delete it. Implies enabled.
	// +optional
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:key/[A-Za-z0-9-]+$`
	KMSKeyARN string `json:"kmsKeyARN,omitempty"`
}

// ControlPlaneSpec is the control plane of a cluster.
type ControlPlaneSpec struct {
	// kubernetesVersion is the Kubernetes minor version, e.g. "1.33".
	// Defaults to the newest version the platform supports. It can only be
	// increased; the default node pool follows it once the control plane
	// runs it.
	// +optional
	// +kubebuilder:validation:Pattern=`^1\.[0-9]{2}$`
	// +kubebuilder:validation:XValidation:rule="int(self.split('.')[1]) >= int(oldSelf.split('.')[1])",message="kubernetesVersion can only be increased"
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// endpointAccess configures the API server's endpoints.
	// +optional
	EndpointAccess *EndpointAccess `json:"endpointAccess,omitempty"`
	// secretsEncryption is the envelope encryption of Kubernetes Secrets.
	// Unset: off for a new cluster, kept as it is for an imported one.
	// +optional
	SecretsEncryption *SecretsEncryption `json:"secretsEncryption,omitempty"`
}

// CapacityType is how the nodes of a pool are bought.
// +kubebuilder:validation:Enum=OnDemand;Spot
type CapacityType string

// Capacity types.
const (
	CapacityTypeOnDemand CapacityType = "OnDemand"
	CapacityTypeSpot     CapacityType = "Spot"
)

// ScalingConfig is the size of a node pool.
// +kubebuilder:validation:XValidation:rule="self.minSize <= self.maxSize && (!has(self.desiredSize) || (self.minSize <= self.desiredSize && self.desiredSize <= self.maxSize))",message="minSize <= desiredSize <= maxSize"
type ScalingConfig struct {
	// minSize is the smallest number of nodes.
	// +required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	MinSize int32 `json:"minSize"`
	// maxSize is the largest number of nodes.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	MaxSize int32 `json:"maxSize"`
	// desiredSize is the number of nodes the pool starts with. Defaults to
	// minSize, at least 1. Applied at creation only: afterwards the
	// autoscaler owns it.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	DesiredSize *int32 `json:"desiredSize,omitempty"`
}

// TaintEffect is the effect of a node taint, as Kubernetes spells it.
// +kubebuilder:validation:Enum=NoSchedule;PreferNoSchedule;NoExecute
type TaintEffect string

// Taint is a taint on the nodes of a pool.
type Taint struct {
	// key of the taint.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
	// value of the taint.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Value string `json:"value,omitempty"`
	// effect of the taint.
	// +required
	Effect TaintEffect `json:"effect"`
}

// NodePoolSpec is the default node pool: the one created with the cluster.
// EKS cannot change a node group's instance types, capacity type, AMI
// type, disk size or launch template: a change to one of them creates a
// replacement node group and retires the old one once the new one is
// ready.
type NodePoolSpec struct {
	// instanceTypes of the nodes. AWS accepts several (e.g. "m6i.large");
	// other providers one.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9._-]+$`
	InstanceTypes []string `json:"instanceTypes"`
	// capacityType is how the nodes are bought.
	// +optional
	// +kubebuilder:default=OnDemand
	CapacityType CapacityType `json:"capacityType,omitempty"`
	// scaling is the size of the pool.
	// +required
	Scaling *ScalingConfig `json:"scaling"`
	// diskSizeGB is the root volume size of a node. Unset: the provider's
	// default. Not with a launch template: the template decides.
	// +optional
	// +kubebuilder:validation:Minimum=20
	// +kubebuilder:validation:Maximum=16384
	DiskSizeGB *int32 `json:"diskSizeGB,omitempty"`
	// labels are the Kubernetes labels of the nodes.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Labels map[string]string `json:"labels,omitempty"`
	// taints of the nodes.
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Taints []Taint `json:"taints,omitempty"`
}

// AccessEntryType is what kind of principal an EKS access entry is for
// (AWS's own spelling). Only STANDARD entries take access policies.
// +kubebuilder:validation:Enum=STANDARD;EC2_LINUX;EC2_WINDOWS;FARGATE_LINUX;EC2;HYBRID_LINUX;HYPERPOD_LINUX
type AccessEntryType string

// AccessPolicyScope is where an access policy applies.
// +kubebuilder:validation:Enum=Cluster;Namespace
type AccessPolicyScope string

// Access policy scopes.
const (
	AccessPolicyScopeCluster   AccessPolicyScope = "Cluster"
	AccessPolicyScopeNamespace AccessPolicyScope = "Namespace"
)

// AccessPolicy is one EKS access policy of an access entry.
// +kubebuilder:validation:XValidation:rule="(has(self.scope) && self.scope == 'Namespace') == (has(self.namespaces) && size(self.namespaces) > 0)",message="namespaces are set exactly when the scope is Namespace"
type AccessPolicy struct {
	// policyARN of the EKS cluster access policy.
	// +required
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z-]*:eks::aws:cluster-access-policy/[A-Za-z0-9]+$`
	PolicyARN string `json:"policyARN"`
	// scope the policy applies to.
	// +optional
	// +kubebuilder:default=Cluster
	Scope AccessPolicyScope `json:"scope,omitempty"`
	// namespaces the policy applies to, with scope Namespace.
	// +listType=set
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	Namespaces []string `json:"namespaces,omitempty"`
}

// AccessEntry is an EKS access entry beyond the admins: a principal with
// the access policies it gets.
// +kubebuilder:validation:XValidation:rule="!has(self.policies) || size(self.policies) == 0 || !has(self.type) || self.type == 'STANDARD'",message="only STANDARD access entries take policies"
type AccessEntry struct {
	// principalARN is the IAM role or user the entry is for; not a
	// service-linked role.
	// +required
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z-]*:iam::[0-9]{12}:(role|user)/[A-Za-z0-9+=,.@_/-]+$`
	// +kubebuilder:validation:XValidation:rule="!self.contains(':role/aws-service-role/')",message="service-linked roles have access entries EKS manages"
	PrincipalARN string `json:"principalARN"`
	// type of the entry.
	// +optional
	// +kubebuilder:default=STANDARD
	Type AccessEntryType `json:"type,omitempty"`
	// policies the principal gets.
	// +listType=map
	// +listMapKey=policyARN
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Policies []AccessPolicy `json:"policies,omitempty"`
}

// AccessSpec controls who can use the Kubernetes API. The platform's own
// provisioning identity is always an admin and is not listed.
type AccessSpec struct {
	// adminPrincipals are granted cluster-admin, in the provider's native
	// form (AWS: IAM role or user ARNs). Mutable; removing a principal
	// revokes its access.
	// +listType=set
	// +optional
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=2048
	AdminPrincipals []string `json:"adminPrincipals,omitempty"`
	// accessEntries are EKS access entries beyond the admins. A principal
	// is either an admin or has an entry, not both. Mutable.
	// +listType=map
	// +listMapKey=principalARN
	// +optional
	// +kubebuilder:validation:MaxItems=50
	AccessEntries []AccessEntry `json:"accessEntries,omitempty"`
	// oidc is an OIDC identity provider the Kubernetes API accepts tokens
	// from, next to IAM (AWS only; a cluster takes one). Mutable: a change
	// removes the association and makes a new one. The users it
	// authenticates have no permissions until a (Cluster)RoleBinding
	// names them or their groups.
	// +optional
	OIDC *OIDCIdentityProvider `json:"oidc,omitempty"`
}

// OIDCIdentityProvider is an OIDC identity provider of the Kubernetes API
// (EKS: IdentityProviderConfig.oidc): either entra (Microsoft Entra ID,
// the platform fills in the issuer and the claims) or issuerURL and
// clientID with the claims of any other provider.
// +kubebuilder:validation:XValidation:rule="has(self.entra) || (has(self.name) && has(self.issuerURL) && has(self.clientID))",message="set either entra, or name, issuerURL and clientID"
// +kubebuilder:validation:XValidation:rule="!has(self.entra) || (!has(self.issuerURL) && !has(self.clientID) && !has(self.usernameClaim) && !has(self.usernamePrefix) && !has(self.groupsClaim) && !has(self.groupsPrefix))",message="entra fills in the issuer, the client and the claims: leave issuerURL, clientID and the claim settings unset"
type OIDCIdentityProvider struct {
	// name of the provider. The EKS association is named
	// "<name>-<hash8 of its settings>". Required without entra; with
	// entra it defaults to "entra".
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=91
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	Name string `json:"name,omitempty"`
	// issuerURL is the "iss" claim of the provider's tokens: HTTPS, and
	// reachable from the internet.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^\s?#]+$`
	IssuerURL string `json:"issuerURL,omitempty"`
	// clientID is the client the tokens are issued to: their "aud" claim.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ClientID string `json:"clientID,omitempty"`
	// entra is Microsoft Entra ID (Azure AD) as the provider, from its
	// tenant and the app registration's client ID alone.
	// +optional
	Entra *EntraIdentityProvider `json:"entra,omitempty"`
	// usernameClaim is the claim that is the user name. Unset: "sub".
	// +optional
	// +kubebuilder:validation:MaxLength=255
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// usernamePrefix is put in front of the user name; "-" for none.
	// Unset: the issuer URL, unless usernameClaim is "email".
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="!self.contains('system:')",message="a prefix cannot contain system:"
	UsernamePrefix string `json:"usernamePrefix,omitempty"`
	// groupsClaim is the claim that lists the user's groups. Unset: no
	// groups.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	GroupsClaim string `json:"groupsClaim,omitempty"`
	// groupsPrefix is put in front of every group.
	// +optional
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="!self.contains('system:')",message="a prefix cannot contain system:"
	GroupsPrefix string `json:"groupsPrefix,omitempty"`
	// requiredClaims are claims a token must carry, with their values.
	// +optional
	// +kubebuilder:validation:MaxProperties=50
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) >= 1 && size(k) <= 63 && size(self[k]) >= 1 && size(self[k]) <= 253)",message="a required claim is 1 to 63 characters, its value 1 to 253"
	RequiredClaims map[string]string `json:"requiredClaims,omitempty"`
}

// EntraIdentityProvider is Microsoft Entra ID as a cluster's OIDC identity
// provider. The platform sets the issuer
// (https://login.microsoftonline.com/<tenantID>/v2.0), the user name (the
// "oid" claim, as "entra:<oid>") and the groups (the "groups" claim, group
// object IDs, as "entra:<id>").
type EntraIdentityProvider struct {
	// tenantID is the directory (tenant) ID.
	// +required
	// +kubebuilder:validation:Format=uuid
	TenantID string `json:"tenantID"`
	// clientID is the application (client) ID of the app registration.
	// +required
	// +kubebuilder:validation:Format=uuid
	ClientID string `json:"clientID"`
}

// AWSAuthenticationMode is the EKS cluster's authentication mode (AWS's
// own spelling).
// +kubebuilder:validation:Enum=API;API_AND_CONFIG_MAP;CONFIG_MAP
type AWSAuthenticationMode string

// AWSAccessConfig is the EKS access configuration.
type AWSAccessConfig struct {
	// authenticationMode of the cluster. Admins and access entries need
	// API or API_AND_CONFIG_MAP.
	// +optional
	// +kubebuilder:default=API
	AuthenticationMode AWSAuthenticationMode `json:"authenticationMode,omitempty"`
}

// AWSClusterLogType is a control plane log type (AWS's own spelling).
// +kubebuilder:validation:Enum=api;audit;authenticator;controllerManager;scheduler
type AWSClusterLogType string

// AWSSupportType is the EKS support policy (AWS's own spelling).
// +kubebuilder:validation:Enum=STANDARD;EXTENDED
type AWSSupportType string

// AWSUpgradePolicy is the EKS upgrade policy.
type AWSUpgradePolicy struct {
	// supportType of the cluster's Kubernetes version. Unset: EKS's
	// default.
	// +optional
	SupportType AWSSupportType `json:"supportType,omitempty"`
}

// AWSAMIType is the AMI type of an EKS node group (AWS's own spelling).
// CUSTOM is an image given in the launch template.
// +kubebuilder:validation:Enum=AL2023_x86_64_STANDARD;AL2023_ARM_64_STANDARD;AL2023_x86_64_NVIDIA;AL2023_x86_64_NEURON;AL2023_ARM_64_NVIDIA;BOTTLEROCKET_x86_64;BOTTLEROCKET_ARM_64;BOTTLEROCKET_x86_64_NVIDIA;BOTTLEROCKET_ARM_64_NVIDIA;BOTTLEROCKET_x86_64_FIPS;BOTTLEROCKET_ARM_64_FIPS;BOTTLEROCKET_x86_64_NVIDIA_FIPS;BOTTLEROCKET_ARM_64_NVIDIA_FIPS;WINDOWS_CORE_2019_x86_64;WINDOWS_FULL_2019_x86_64;WINDOWS_CORE_2022_x86_64;WINDOWS_FULL_2022_x86_64;WINDOWS_CORE_2025_x86_64;WINDOWS_FULL_2025_x86_64;CUSTOM
type AWSAMIType string

// AWSLaunchTemplate is the launch template of the default node group:
// either an existing one (id or name, with a version), or one the platform
// renders from securityGroupIDs and/or a custom image.
// +kubebuilder:validation:XValidation:rule="((has(self.id) != has(self.name)) && has(self.version) && !has(self.securityGroupIDs) && !has(self.imageID) && !has(self.userData)) || (!has(self.id) && !has(self.name) && !has(self.version) && (has(self.securityGroupIDs) || has(self.imageID)))",message="name an existing launch template (id or name, with a version) or give what the platform renders one from (securityGroupIDs, imageID with userData), not both"
// +kubebuilder:validation:XValidation:rule="has(self.imageID) == has(self.userData)",message="imageID and userData go together: a custom image needs its bootstrap script"
type AWSLaunchTemplate struct {
	// id of an existing launch template.
	// +optional
	// +kubebuilder:validation:Pattern=`^lt-[0-9a-f]{8,17}$`
	ID string `json:"id,omitempty"`
	// name of an existing launch template.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Name string `json:"name,omitempty"`
	// version of the existing launch template.
	// +optional
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]*$`
	Version string `json:"version,omitempty"`
	// securityGroupIDs the nodes get in addition to the cluster's security
	// group.
	// +listType=set
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:validation:items:Pattern=`^sg-[0-9a-f]{8,17}$`
	SecurityGroupIDs []string `json:"securityGroupIDs,omitempty"`
	// imageID of a custom AMI. Makes the AMI type CUSTOM.
	// +optional
	// +kubebuilder:validation:Pattern=`^ami-[0-9a-f]{8,17}$`
	ImageID string `json:"imageID,omitempty"`
	// userData is the bootstrap script of the custom AMI.
	// +optional
	// +kubebuilder:validation:MaxLength=21848
	UserData string `json:"userData,omitempty"`
}

// AWSNodePoolSpec are the AWS settings of the default node pool.
// +kubebuilder:validation:XValidation:rule="!has(self.amiType) || self.amiType != 'CUSTOM' || has(self.launchTemplate)",message="amiType CUSTOM needs a launch template with the image"
type AWSNodePoolSpec struct {
	// amiType of the nodes. Defaults to AL2023_x86_64_STANDARD.
	// +optional
	AMIType AWSAMIType `json:"amiType,omitempty"`
	// launchTemplate of the node group. Unset: EKS's own.
	// +optional
	LaunchTemplate *AWSLaunchTemplate `json:"launchTemplate,omitempty"`
}

// AWSAddon is an EKS add-on. The platform always installs vpc-cni,
// coredns, kube-proxy and eks-pod-identity-agent; an entry of the same
// name overrides its version or configuration.
// +kubebuilder:validation:XValidation:rule="(self.name == 'aws-mountpoint-s3-csi-driver') == (has(self.s3BucketNames) && size(self.s3BucketNames) > 0)",message="s3BucketNames are set exactly for aws-mountpoint-s3-csi-driver"
type AWSAddon struct {
	// name of the add-on.
	// +required
	// +kubebuilder:validation:Enum=vpc-cni;coredns;kube-proxy;eks-pod-identity-agent;aws-ebs-csi-driver;aws-efs-csi-driver;aws-fsx-csi-driver;aws-mountpoint-s3-csi-driver;snapshot-controller;cert-manager;metrics-server;kube-state-metrics
	Name string `json:"name"`
	// version of the add-on, e.g. "v1.19.0-eksbuild.1". Unset: EKS's
	// default for the cluster's Kubernetes version.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^(v[0-9][0-9A-Za-z.+-]*)?$`
	Version string `json:"version,omitempty"`
	// configurationValues is the add-on's configuration, as JSON or YAML.
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	ConfigurationValues string `json:"configurationValues,omitempty"`
	// s3BucketNames the Mountpoint for S3 driver may mount.
	// +listType=set
	// +optional
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`
	S3BucketNames []string `json:"s3BucketNames,omitempty"`
}

// AWSControlPlaneLogGroup makes the platform own the CloudWatch Logs group
// of the control plane logs, with a retention, instead of leaving EKS to
// create one that never expires.
type AWSControlPlaneLogGroup struct {
	// enabled turns it on.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// retentionDays is how long the group keeps the logs: one of the
	// retention periods CloudWatch Logs has.
	// +optional
	// +kubebuilder:default=90
	// +kubebuilder:validation:Enum=1;3;5;7;14;30;60;90;120;150;180;365;400;545;731;1096;1827;2192;2557;2922;3288;3653
	RetentionDays int32 `json:"retentionDays,omitempty"`
}

// AWSClusterSpec are the settings of an EKS cluster that have no
// cross-cloud equivalent.
type AWSClusterSpec struct {
	// accessConfig is the EKS access configuration.
	// +optional
	AccessConfig *AWSAccessConfig `json:"accessConfig,omitempty"`
	// enabledClusterLogTypes are the control plane logs sent to CloudWatch
	// Logs.
	// +listType=set
	// +optional
	EnabledClusterLogTypes []AWSClusterLogType `json:"enabledClusterLogTypes,omitempty"`
	// upgradePolicy is the EKS upgrade policy.
	// +optional
	UpgradePolicy *AWSUpgradePolicy `json:"upgradePolicy,omitempty"`
	// defaultNodePool are the AWS settings of the default node pool.
	// +optional
	DefaultNodePool *AWSNodePoolSpec `json:"defaultNodePool,omitempty"`
	// addons are EKS add-ons beyond, or overriding, the default ones.
	// +listType=map
	// +listMapKey=name
	// +optional
	// +kubebuilder:validation:MaxItems=12
	Addons []AWSAddon `json:"addons,omitempty"`
	// controlPlaneLogGroup makes the platform own the control plane's log
	// group.
	// +optional
	ControlPlaneLogGroup *AWSControlPlaneLogGroup `json:"controlPlaneLogGroup,omitempty"`
	// irsa creates the cluster's IAM OIDC provider (IAM roles for service
	// accounts). Unset or false: none.
	// +optional
	IRSA *bool `json:"irsa,omitempty"`
	// additionalSecurityGroupIDs are attached to the control plane's
	// network interfaces. Immutable: EKS cannot change them.
	// +listType=set
	// +optional
	// +kubebuilder:validation:MaxItems=4
	// +kubebuilder:validation:items:Pattern=`^sg-[0-9a-f]{8,17}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="additionalSecurityGroupIDs are immutable"
	AdditionalSecurityGroupIDs []string `json:"additionalSecurityGroupIDs,omitempty"`
}

// AWSImportCluster names what of an existing EKS cluster is imported. The
// EKS cluster itself is the one named like the Cluster object.
type AWSImportCluster struct {
	// defaultNodeGroupName is the node group that becomes
	// spec.defaultNodePool. The cluster's other node groups are not
	// imported: they stay unmanaged and block deleting the cluster until
	// they are removed.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z][A-Za-z0-9_-]*$`
	DefaultNodeGroupName string `json:"defaultNodeGroupName"`
}

// ClusterImport makes the cluster an existing one: it is looked up once,
// read-only, and imported with observe-only objects and a plan
// (status.importPlan). Nothing changes in the cloud until takeOver.
// +kubebuilder:validation:XValidation:rule="has(self.aws)",message="exactly one provider must be set: aws"
// +kubebuilder:validation:XValidation:rule="self.aws == oldSelf.aws",message="what is imported is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.takeOver) || !oldSelf.takeOver || (has(self.takeOver) && self.takeOver)",message="a takeover cannot be undone"
type ClusterImport struct {
	Takeover `json:",inline"`
	// aws names what of the EKS cluster is imported.
	// +optional
	AWS *AWSImportCluster `json:"aws,omitempty"`
}

// ClusterSpec is the desired state of a cluster. Region, network, control
// plane, default node pool and access mean the same on every cloud; the
// provider struct (aws) selects the cloud and carries what has no
// cross-cloud equivalent. Exactly one provider is set, even if empty
// (`aws: {}`).
// +kubebuilder:validation:XValidation:rule="has(self.aws)",message="exactly one provider must be set: aws (it may be empty)"
// +kubebuilder:validation:XValidation:rule="has(self.importFrom) == has(oldSelf.importFrom)",message="a cluster is created or imported for good: importFrom cannot be added or removed"
// +kubebuilder:validation:XValidation:rule="has(self.importFrom) || has(self.defaultNodePool)",message="defaultNodePool is required (an imported cluster's is read from the cloud)"
// +kubebuilder:validation:XValidation:rule="has(self.importFrom) || (has(self.controlPlane) && has(self.controlPlane.endpointAccess) && ((has(self.controlPlane.endpointAccess.publicAccessCIDRs) && size(self.controlPlane.endpointAccess.publicAccessCIDRs) > 0) || (has(self.controlPlane.endpointAccess.publicAccess) && !self.controlPlane.endpointAccess.publicAccess)))",message="controlPlane.endpointAccess.publicAccessCIDRs needs at least one CIDR allowed to reach the public API endpoint (0.0.0.0/0 for any)"
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || self.region.matches('^[a-z]{2}(-gov|-iso[a-z]?)?-[a-z]+-[0-9]+$')",message="region must be an AWS region, e.g. us-east-1"
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || !has(self.defaultNodePool) || self.defaultNodePool.instanceTypes.all(t, t.matches('^[a-z][a-z0-9-]*[.][a-z0-9]+$'))",message="AWS instance types look like m6i.large"
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || !has(self.access) || !has(self.access.adminPrincipals) || self.access.adminPrincipals.all(p, p.matches('^arn:aws[a-z-]*:iam::[0-9]{12}:(role|user)/[A-Za-z0-9+=,.@_/-]+$') && !p.contains(':role/aws-service-role/'))",message="AWS admin principals must be IAM role or user ARNs, not service-linked roles"
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || !has(self.aws.accessConfig) || self.aws.accessConfig.authenticationMode != 'CONFIG_MAP' || !has(self.access) || ((!has(self.access.adminPrincipals) || size(self.access.adminPrincipals) == 0) && (!has(self.access.accessEntries) || size(self.access.accessEntries) == 0))",message="adminPrincipals and accessEntries need authenticationMode API or API_AND_CONFIG_MAP"
// +kubebuilder:validation:XValidation:rule="!has(self.access) || !has(self.access.accessEntries) || !has(self.access.adminPrincipals) || self.access.accessEntries.all(e, !(e.principalARN in self.access.adminPrincipals))",message="a principal is either in adminPrincipals or in accessEntries, not both"
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || !has(self.network.podCIDR) || size(self.network.podCIDR) == 0",message="AWS pods take IPs from the VPC subnets; network.podCIDR must be empty"
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || !has(self.network.zoneIDs) || size(self.network.zoneIDs) == 0 || (size(self.network.zoneIDs) >= 2 && self.network.zoneIDs.all(z, z.matches('^[a-z]+[0-9]-az[0-9]+$')))",message="AWS clusters need at least two zoneIDs, given as zone IDs like use1-az1"
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || !has(self.network.serviceCIDR) || size(self.network.serviceCIDR) == 0 || self.network.serviceCIDR.matches('/(1[2-9]|2[0-4])$')",message="EKS serviceCIDR must be /12 to /24"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultNodePool) || !has(self.defaultNodePool.diskSizeGB) || !has(self.aws) || !has(self.aws.defaultNodePool) || !has(self.aws.defaultNodePool.launchTemplate)",message="defaultNodePool.diskSizeGB cannot be set with a launch template: EKS takes the disk from the template"
type ClusterSpec struct {
	// displayName is a human-friendly name. Mutable.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`
	// tags of every cloud resource of the cluster. Mutable: a change
	// re-tags the resources. Keys starting with "aws:" are refused for AWS.
	// +optional
	Tags Tags `json:"tags,omitempty"`
	// region the cluster runs in: its network's. Immutable.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="region is immutable"
	Region string `json:"region"`
	// network is where the cluster runs: its Network (which owns the
	// cluster) and the cluster's own ranges. Immutable.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="network is immutable"
	Network ClusterNetwork `json:"network"`
	// controlPlane settings.
	// +optional
	ControlPlane *ControlPlaneSpec `json:"controlPlane,omitempty"`
	// defaultNodePool is the node pool created together with the cluster.
	// Required for a cluster the platform creates.
	// +optional
	DefaultNodePool *NodePoolSpec `json:"defaultNodePool,omitempty"`
	// access controls who administers the cluster. Mutable.
	// +optional
	Access *AccessSpec `json:"access,omitempty"`
	// releaseChannel is the channel the cluster takes platform releases
	// from: Alpha, then Beta, then Stable. Stored only for now.
	// +optional
	ReleaseChannel ReleaseChannelName `json:"releaseChannel,omitempty"`
	// tier is how sensitive the cluster is. Within a release channel a
	// release reaches Tier2 first, then Tier1, then Tier0. Stored only for
	// now.
	// +optional
	Tier ClusterTier `json:"tier,omitempty"`
	// upgradeMode is whether platform releases move the cluster: Manual
	// (the default; the tenant chooses versions, channels do not apply) or
	// Automatic (the cluster follows its release channel by tier). Mutable.
	// Stored only for now.
	// +optional
	// +kubebuilder:default=Manual
	UpgradeMode UpgradeMode `json:"upgradeMode,omitempty"`
	// aws selects AWS (EKS) and carries its settings.
	// +optional
	AWS *AWSClusterSpec `json:"aws,omitempty"`
	// importFrom imports an existing cluster instead of creating one.
	// +optional
	ImportFrom *ClusterImport `json:"importFrom,omitempty"`
}

// ClusterPhase is the coarse lifecycle state of a cluster.
// +kubebuilder:validation:Enum=Provisioning;Imported;Running;Updating;Deleting;Failed
type ClusterPhase string

// Cluster phases.
const (
	// ClusterPhaseProvisioning: accepted; cloud resources are being created.
	ClusterPhaseProvisioning ClusterPhase = "Provisioning"
	// ClusterPhaseImported: imported and read-only; waiting for takeOver.
	ClusterPhaseImported ClusterPhase = "Imported"
	// ClusterPhaseRunning: all resources are ready and the API server is
	// reachable.
	ClusterPhaseRunning ClusterPhase = "Running"
	// ClusterPhaseUpdating: a spec change, or a takeover, is being rolled
	// out.
	ClusterPhaseUpdating ClusterPhase = "Updating"
	// ClusterPhaseDeleting: teardown in progress.
	ClusterPhaseDeleting ClusterPhase = "Deleting"
	// ClusterPhaseFailed: see message and the conditions.
	ClusterPhaseFailed ClusterPhase = "Failed"
)

// ClusterEndpoint is how clients reach the Kubernetes API server.
type ClusterEndpoint struct {
	// url is the HTTPS URL of the endpoint.
	// +optional
	URL string `json:"url,omitempty"`
	// certificateAuthorityData is the PEM-encoded cluster CA certificate.
	// +optional
	CertificateAuthorityData []byte `json:"certificateAuthorityData,omitempty"`
}

// AWSAddonStatus is the observed state of one EKS add-on.
type AWSAddonStatus struct {
	// name of the add-on.
	// +required
	Name string `json:"name"`
	// version that is installed.
	// +optional
	Version string `json:"version,omitempty"`
	// ready reports whether the add-on is active.
	// +optional
	Ready bool `json:"ready,omitempty"`
	// message explains a state that is not ready.
	// +optional
	Message string `json:"message,omitempty"`
}

// AWSClusterStatus is the observed state of the EKS cluster.
type AWSClusterStatus struct {
	// clusterARN of the EKS cluster.
	// +optional
	ClusterARN string `json:"clusterARN,omitempty"`
	// clusterSecurityGroupID is the security group EKS created for the
	// cluster.
	// +optional
	ClusterSecurityGroupID string `json:"clusterSecurityGroupID,omitempty"`
	// platformVersion of the EKS cluster.
	// +optional
	PlatformVersion string `json:"platformVersion,omitempty"`
	// nodeGroupARN of the default node group.
	// +optional
	NodeGroupARN string `json:"nodeGroupARN,omitempty"`
	// addons are the cluster's EKS add-ons.
	// +listType=map
	// +listMapKey=name
	// +optional
	Addons []AWSAddonStatus `json:"addons,omitempty"`
}

// ClusterUpgradePhase is the step of a Kubernetes upgrade. An upgrade runs
// in two steps, in this order, because EKS refuses node groups and add-ons
// ahead of the control plane.
// +kubebuilder:validation:Enum=ControlPlane;NodesAndAddons
type ClusterUpgradePhase string

const (
	// ClusterUpgradeControlPlane: the control plane is being upgraded and
	// still reports the version the upgrade started from. The node group
	// stays at that version and the add-ons at the versions they run.
	ClusterUpgradeControlPlane ClusterUpgradePhase = "ControlPlane"
	// ClusterUpgradeNodesAndAddons: the control plane runs the target
	// version; the node group and the add-ons are applied at it and are
	// rolling out.
	ClusterUpgradeNodesAndAddons ClusterUpgradePhase = "NodesAndAddons"
)

// ClusterUpgrade is where a Kubernetes upgrade stands.
type ClusterUpgrade struct {
	// fromVersion is the version the upgrade started from.
	// +optional
	FromVersion string `json:"fromVersion,omitempty"`
	// toVersion is the version being moved to
	// (spec.controlPlane.kubernetesVersion).
	// +optional
	ToVersion string `json:"toVersion,omitempty"`
	// phase is the step the upgrade is at.
	// +optional
	Phase ClusterUpgradePhase `json:"phase,omitempty"`
	// startTime is when the upgrade was first seen in progress.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// message is what the step waits for or, when the cloud refuses or
	// fails the step, the provider's error for the resource concerned.
	// +optional
	Message string `json:"message,omitempty"`
}

// ConditionPlatformComponentsReady is a Cluster's condition that every
// component the platform installs with it is installed.
const ConditionPlatformComponentsReady = "PlatformComponentsReady"

// PlatformComponentStatus is one component the platform installed with a
// cluster.
type PlatformComponentStatus struct {
	// name in the catalog, e.g. karpenter.
	// +required
	Name string `json:"name"`
	// version of the chart the cluster's release pins.
	// +optional
	Version string `json:"version,omitempty"`
	// phase: Pending (waits), Installing, Ready or Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// message says what it waits for or why it failed.
	// +optional
	Message string `json:"message,omitempty"`
	// chart is the Helm release name of the component's chart that failed
	// or is being installed; the fields below are about it.
	// +optional
	Chart string `json:"chart,omitempty"`
	// helmState is Helm's state of that chart's release (deployed, failed,
	// pending-install, pending-upgrade, pending-rollback, uninstalling).
	// +optional
	HelmState string `json:"helmState,omitempty"`
	// lastError is the provider's last error for that chart, in full.
	// +optional
	LastError string `json:"lastError,omitempty"`
	// failures is how many times in a row installing that chart failed.
	// +optional
	Failures int32 `json:"failures,omitempty"`
	// since is when the component entered this phase.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`
	// recoveries is how many times the platform recovered a stuck Helm
	// release of this component at this version (at most 3).
	// +optional
	Recoveries int32 `json:"recoveries,omitempty"`
}

// ClusterStatus is the observed state of a Cluster, aggregated from its
// Crossplane objects.
type ClusterStatus struct {
	// observedGeneration is the generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// phase is the coarse lifecycle state.
	// +optional
	Phase ClusterPhase `json:"phase,omitempty"`
	// message summarises the state, typically the reason for Failed.
	// +optional
	Message string `json:"message,omitempty"`
	// conditions: Ready and Synced, and finer ones (NetworkReady,
	// ControlPlaneReady, NodePoolReady).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// endpoint of the Kubernetes API server, once the control plane is
	// ready.
	// +optional
	Endpoint *ClusterEndpoint `json:"endpoint,omitempty"`
	// currentKubernetesVersion is the version the control plane runs.
	// +optional
	CurrentKubernetesVersion string `json:"currentKubernetesVersion,omitempty"`
	// oidcIssuerURL is the service-account token issuer, for workload
	// identity.
	// +optional
	OIDCIssuerURL string `json:"oidcIssuerURL,omitempty"`
	// network are the cloud resources of the cluster's Network.
	// +optional
	Network *NetworkResources `json:"network,omitempty"`
	// aws is the observed state of the EKS cluster.
	// +optional
	AWS *AWSClusterStatus `json:"aws,omitempty"`
	// importPlan is set for an imported cluster: the plan of the import,
	// kept as a record after the takeover.
	// +optional
	ImportPlan *ImportPlan `json:"importPlan,omitempty"`
	// inventory lists every Crossplane object of the cluster (IAM roles,
	// the EKS cluster, the default node group, add-ons, access entries,
	// ...) with its cloud identifier, in creation order.
	// +listType=atomic
	// +optional
	Inventory []InventoryEntry `json:"inventory,omitempty"`
	// upgrade is the Kubernetes upgrade in progress: set from the moment
	// spec.controlPlane.kubernetesVersion differs from what the control
	// plane runs until the node group and the add-ons have followed. Unset
	// when no upgrade is in progress.
	// +optional
	Upgrade *ClusterUpgrade `json:"upgrade,omitempty"`
	// kubeconfigSecret is the name of the Secret in this namespace holding
	// the cluster's kubeconfig (key "kubeconfig"). Empty until the control
	// plane is ready.
	// +optional
	KubeconfigSecret string `json:"kubeconfigSecret,omitempty"`
	// platformComponents are the components the platform installs with
	// the cluster (the Platform definitions of the catalog), in install
	// order: installed once the node group is ready and the kubeconfig
	// exists. Read-only for tenants. The cluster's phase does not wait for
	// them; the condition PlatformComponentsReady says whether all are
	// installed.
	// +listType=map
	// +listMapKey=name
	// +optional
	PlatformComponents []PlatformComponentStatus `json:"platformComponents,omitempty"`
	// platformRelease is the release the platform components' versions
	// come from: the one the cluster's channel offered when they were
	// first installed.
	// +optional
	PlatformRelease string `json:"platformRelease,omitempty"`
	// componentInventory lists the Crossplane objects of the cluster's
	// platform components (their IAM roles and Helm releases), in creation
	// order. Kept apart from inventory: the cluster's own state is read
	// from that one only.
	// +listType=atomic
	// +optional
	ComponentInventory []InventoryEntry `json:"componentInventory,omitempty"`
	// deprecations is what the last scan for deprecated API versions found.
	// +optional
	Deprecations *ClusterDeprecations `json:"deprecations,omitempty"`
	// contractStatus is the status as the cloud.v1 API returns it
	// (protojson), kept verbatim: the fields above are read from it, and it
	// carries what they leave out (the state of every object). Written by
	// the platform; not meant to be edited.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	ContractStatus *runtime.RawExtension `json:"contractStatus,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cvc;cvcluster,categories=cluster-vending
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.spec.network.networkRef.name`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.currentKubernetesVersion`
// +kubebuilder:printcolumn:name="Upgrade",type=string,JSONPath=`.status.upgrade.phase`,priority=1
// +kubebuilder:printcolumn:name="Upgrading-To",type=string,JSONPath=`.status.upgrade.toVersion`,priority=1
// +kubebuilder:printcolumn:name="Channel",type=string,JSONPath=`.spec.releaseChannel`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.upgradeMode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$')",message="the name is a lowercase DNS label of at most 40 characters: it is the cloud cluster's name"

// Cluster is a managed Kubernetes cluster (AWS: EKS) of a tenant. Its name
// is the cloud cluster's name, unique per account and region. It is owned
// by exactly one Network (spec.network.networkRef) and, through it, by the
// network's CloudAccount. The kind is also the name of the EKS provider's
// Cluster (eks.aws.m.upbound.io): `kubectl get cvc` (or
// clusters.cloud.cluster-vending.io) names this one.
type Cluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Cluster
	// +required
	Spec ClusterSpec `json:"spec"`

	// status defines the observed state of Cluster
	// +optional
	Status ClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Cluster{}, &ClusterList{})
		return nil
	})
}
