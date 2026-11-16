package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SubnetRole decides routing and placement for a subnet.
// +kubebuilder:validation:Enum=Public;Private
type SubnetRole string

// Subnet roles.
const (
	// SubnetRolePublic: routed to the internet gateway; hosts NAT gateways
	// and internet-facing load balancers.
	SubnetRolePublic SubnetRole = "Public"
	// SubnetRolePrivate: egress through NAT only; hosts nodes, control-plane
	// interfaces and internal load balancers.
	SubnetRolePrivate SubnetRole = "Private"
)

// NATGatewayMode controls NAT redundancy.
// +kubebuilder:validation:Enum=Single;PerZone;Regional
type NATGatewayMode string

// NAT gateway modes.
const (
	// NATGatewayModeSingle: one zonal NAT gateway. Cheapest; private egress
	// depends on one zone.
	NATGatewayModeSingle NATGatewayMode = "Single"
	// NATGatewayModePerZone: one NAT gateway per zone; needs a public subnet
	// in every zone that has a private one.
	NATGatewayModePerZone NATGatewayMode = "PerZone"
	// NATGatewayModeRegional: one regional NAT spanning all zones.
	NATGatewayModeRegional NATGatewayMode = "Regional"
)

// Subnet is one subnet of a network.
type Subnet struct {
	// role of the subnet.
	// +required
	Role SubnetRole `json:"role"`
	// cidrBlock is the subnet's IPv4 range, inside the network's.
	// +required
	// +kubebuilder:validation:MaxLength=18
	// +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`
	CIDRBlock string `json:"cidrBlock"`
	// zoneID is the zone the subnet lives in, as a zone ID (AWS:
	// "use1-az1"): the same physical zone in every account, unlike zone
	// names. Required on providers with zonal subnets (AWS).
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]*$`
	ZoneID string `json:"zoneID,omitempty"`
}

// NetworkLayout is the address space, subnets and NAT of a network.
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || size(self.subnets) == 0 || !has(self.availabilityZoneCount) || self.subnets.all(s, !has(s.zoneID) || size(s.zoneID) == 0)",message="zonal subnets already define the zones; availabilityZoneCount only combines with regional subnets (no zoneID)"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || self.subnets.all(a, self.subnets.exists_one(b, b.cidrBlock == a.cidrBlock))",message="subnet cidrBlock values must be unique"
// +kubebuilder:validation:XValidation:rule="!has(self.subnets) || size(self.subnets) == 0 || self.subnets.exists(s, s.role == 'Private')",message="subnets, when set, must include at least one Private subnet"
type NetworkLayout struct {
	// cidrBlock is the network's IPv4 range, /16 to /24.
	// +optional
	// +kubebuilder:default="10.0.0.0/16"
	// +kubebuilder:validation:MaxLength=18
	// +kubebuilder:validation:Pattern=`^([0-9]{1,3}\.){3}[0-9]{1,3}/(1[6-9]|2[0-4])$`
	CIDRBlock string `json:"cidrBlock,omitempty"`
	// subnets is the explicit subnet layout. Empty: the default layout is
	// carved out of cidrBlock (AWS: one Public and one Private subnet per
	// zone) and reported in status.layout. The ranges must lie inside
	// cidrBlock and not overlap (checked by the controller).
	// +listType=atomic
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Subnets []Subnet `json:"subnets,omitempty"`
	// availabilityZoneCount is the number of zones the default layout
	// spreads across. Defaults to 3. With explicit zonal subnets the zones
	// are the subnets', so it must be unset.
	// +optional
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=6
	AvailabilityZoneCount *int32 `json:"availabilityZoneCount,omitempty"`
	// natGatewayMode controls NAT redundancy.
	// +optional
	// +kubebuilder:default=Single
	NATGatewayMode NATGatewayMode `json:"natGatewayMode,omitempty"`
}

// FlowLogTrafficType is the traffic a flow log records.
// +kubebuilder:validation:Enum=All;Accept;Reject
type FlowLogTrafficType string

// Flow log traffic types.
const (
	FlowLogTrafficTypeAll    FlowLogTrafficType = "All"
	FlowLogTrafficTypeAccept FlowLogTrafficType = "Accept"
	FlowLogTrafficTypeReject FlowLogTrafficType = "Reject"
)

// NetworkFlowLogs records the network's IP traffic in the cloud's log
// service, with what delivering it needs (AWS: a FlowLog on the VPC, a
// CloudWatch Logs group and a delivery role). They can be turned on and
// off and the retention changed; the traffic type and aggregation interval
// of flow logs that exist cannot (turn them off, then on).
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.enabled) && oldSelf.enabled && has(self.enabled) && self.enabled) || (self.trafficType == oldSelf.trafficType && self.aggregationIntervalSeconds == oldSelf.aggregationIntervalSeconds)",message="the traffic type and aggregation interval of existing flow logs cannot be changed; turn flow logs off, then on with the new settings"
type NetworkFlowLogs struct {
	// enabled turns flow logs on. Unset or false: no flow log resources.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// trafficType is the traffic recorded.
	// +optional
	// +kubebuilder:default=All
	TrafficType FlowLogTrafficType `json:"trafficType,omitempty"`
	// retentionDays is how long the log group keeps the records: one of the
	// retention periods CloudWatch Logs has.
	// +optional
	// +kubebuilder:default=30
	// +kubebuilder:validation:Enum=1;3;5;7;14;30;60;90;120;150;180;365;400;545;731;1096;1827;2192;2557;2922;3288;3653
	RetentionDays int32 `json:"retentionDays,omitempty"`
	// aggregationIntervalSeconds is how many seconds of traffic one record
	// aggregates.
	// +optional
	// +kubebuilder:default=600
	// +kubebuilder:validation:Enum=60;600
	AggregationIntervalSeconds int32 `json:"aggregationIntervalSeconds,omitempty"`
}

// AWSImportNetwork names an existing VPC to import.
type AWSImportNetwork struct {
	// vpcID of the VPC.
	// +required
	// +kubebuilder:validation:Pattern=`^vpc-[0-9a-f]{8,17}$`
	VPCID string `json:"vpcID"`
	// subnetIDs are the VPC's subnets the network takes in.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:Pattern=`^subnet-[0-9a-f]{8,17}$`
	SubnetIDs []string `json:"subnetIDs"`
}

// NetworkImport makes the network an existing one: it is looked up once,
// read-only, and imported with observe-only objects and a plan
// (status.importPlan). Nothing changes in the cloud until takeOver.
// +kubebuilder:validation:XValidation:rule="has(self.aws)",message="exactly one provider must be set: aws"
// +kubebuilder:validation:XValidation:rule="self.aws == oldSelf.aws",message="what is imported is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.takeOver) || !oldSelf.takeOver || (has(self.takeOver) && self.takeOver)",message="a takeover cannot be undone"
type NetworkImport struct {
	Takeover `json:",inline"`
	// aws names the VPC to import.
	// +optional
	AWS *AWSImportNetwork `json:"aws,omitempty"`
}

// NetworkSpec is the desired state of a Network.
// +kubebuilder:validation:XValidation:rule="has(self.importFrom) != has(self.layout)",message="exactly one of layout (a network the platform creates) and importFrom (an existing one) must be set"
// +kubebuilder:validation:XValidation:rule="has(self.layout) == has(oldSelf.layout) && (!has(self.layout) || self.layout == oldSelf.layout)",message="layout is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.importFrom) == has(oldSelf.importFrom)",message="a network is created or imported for good: importFrom cannot be added or removed"
// +kubebuilder:validation:XValidation:rule="!has(self.importFrom) || !has(self.flowLogs)",message="flowLogs cannot be set on an imported network: it keeps the flow logs it was imported with"
type NetworkSpec struct {
	// +kubebuilder:default=Tier0
	Tier ClusterTier `json:"tier,omitempty"`
	// accountRef names the CloudAccount the network lives in, in the same
	// namespace. The account owns the network. Immutable. Clusters run in
	// their network's account.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="accountRef is immutable"
	AccountRef LocalReference `json:"accountRef"`
	// displayName is a human-friendly name. Mutable.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`
	// tags of every cloud resource of the network. Mutable: a change
	// re-tags the resources (an imported network's resources keep their
	// other tags).
	// +optional
	Tags Tags `json:"tags,omitempty"`
	// region of the network (AWS: e.g. "us-east-1"). Immutable.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="region is immutable"
	Region string `json:"region"`
	// layout is the address space, subnets and NAT of a network the
	// platform creates. Immutable. Not set for an imported network: its
	// layout is read from the cloud and reported in status.layout.
	// +optional
	Layout *NetworkLayout `json:"layout,omitempty"`
	// flowLogs of the network's IP traffic. Unset: none.
	// +optional
	FlowLogs *NetworkFlowLogs `json:"flowLogs,omitempty"`
	// importFrom imports an existing network instead of creating one.
	// +optional
	ImportFrom *NetworkImport `json:"importFrom,omitempty"`
}

// NetworkPhase is the coarse lifecycle state of a network.
// +kubebuilder:validation:Enum=Provisioning;Imported;Ready;Updating;Deleting;Failed
type NetworkPhase string

// Network phases.
const (
	// NetworkPhaseProvisioning: being created.
	NetworkPhaseProvisioning NetworkPhase = "Provisioning"
	// NetworkPhaseImported: imported and read-only; waiting for takeOver.
	NetworkPhaseImported NetworkPhase = "Imported"
	// NetworkPhaseReady: owned by the platform and matching the spec.
	NetworkPhaseReady NetworkPhase = "Ready"
	// NetworkPhaseUpdating: a spec change, or a takeover, is being applied.
	NetworkPhaseUpdating NetworkPhase = "Updating"
	// NetworkPhaseDeleting: deletion was requested; it waits until no
	// Cluster uses the network, then its resources are removed.
	NetworkPhaseDeleting NetworkPhase = "Deleting"
	// NetworkPhaseFailed: see message and the conditions.
	NetworkPhaseFailed NetworkPhase = "Failed"
)

// NetworkStatus is the observed state of a Network.
type NetworkStatus struct {
	// observedGeneration is the generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// phase is the coarse lifecycle state.
	// +optional
	Phase NetworkPhase `json:"phase,omitempty"`
	// message summarises the state, typically the reason for Failed.
	// +optional
	Message string `json:"message,omitempty"`
	// conditions: Ready and Synced.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// layout is the layout as built: spec.layout with its defaults
	// resolved (the carved subnets), or the layout read from an imported
	// network.
	// +optional
	Layout *NetworkLayout `json:"layout,omitempty"`
	// resources are the cloud identifiers and egress IPs of the network.
	// +optional
	Resources *NetworkResources `json:"resources,omitempty"`
	// clusters are the names of the Clusters the network owns. The network
	// cannot be deleted while there are any.
	// +listType=set
	// +optional
	Clusters []string `json:"clusters,omitempty"`
	// importPlan is set for an imported network: the plan of the import,
	// kept as a record after the takeover.
	// +optional
	ImportPlan *ImportPlan `json:"importPlan,omitempty"`
	// codeChange is a change of what the platform renders for the network
	// (a new controller build), applied in the tier of its most careful
	// cluster, before its clusters.
	// +optional
	CodeChange *CodeChange `json:"codeChange,omitempty"`
	// inventory lists every Crossplane object of the network (VPC, subnets,
	// NAT gateways, routes, ...) with its cloud identifier, in creation
	// order.
	// +listType=atomic
	// +optional
	Inventory []InventoryEntry `json:"inventory,omitempty"`
	// recentEvents are the last things the controller reported about the
	// network, oldest first: the states it reached, steps that finished,
	// failures. They are also Kubernetes events of this object, which are
	// kept for about an hour only.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=10
	// +optional
	RecentEvents []RecentEvent `json:"recentEvents,omitempty"`
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
// +kubebuilder:resource:shortName=cvnet;cvnetwork,categories=cluster-vending
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.accountRef.name`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
// +kubebuilder:printcolumn:name="VPC",type=string,JSONPath=`.status.resources.cloudNetworkID`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$')",message="the name is a lowercase DNS label of at most 40 characters: it prefixes the network's cloud resource names"

// Network is a tenant network (AWS: a VPC) with its subnets, NAT and
// routing, created by the platform or imported and then taken over. It is
// owned by exactly one CloudAccount (spec.accountRef) and owns the
// Clusters that run in it: one network serves many clusters. It is not
// deleted while it owns a Cluster (NetworkFinalizer).
type Network struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Network
	// +required
	Spec NetworkSpec `json:"spec"`

	// status defines the observed state of Network
	// +optional
	Status NetworkStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NetworkList contains a list of Network
type NetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Network `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Network{}, &NetworkList{})
		return nil
	})
}
