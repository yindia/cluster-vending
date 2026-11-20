package v1alpha1

// Architecture is a node CPU architecture, as the kinds spell it.
// +kubebuilder:validation:Enum=Amd64;Arm64
type Architecture string

// The architectures of a node group.
const (
	ArchitectureAmd64 Architecture = "Amd64"
	ArchitectureArm64 Architecture = "Arm64"
)

// NodeGroupPhase is where a node group is.
// +kubebuilder:validation:Enum=Pending;Ready;Failed;Deleting
type NodeGroupPhase string

// The phases of a node group.
const (
	NodeGroupPhasePending  NodeGroupPhase = "Pending"
	NodeGroupPhaseReady    NodeGroupPhase = "Ready"
	NodeGroupPhaseFailed   NodeGroupPhase = "Failed"
	NodeGroupPhaseDeleting NodeGroupPhase = "Deleting"
)

// NodeGroupSpec is one user node group: a Karpenter NodePool with its own
// EC2NodeClass in the workload cluster.
type NodeGroupSpec struct {
	// name is a DNS label, unique in the cluster. "platform", "default" and
	// "system" are reserved.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="!(self in ['platform', 'default', 'system'])",message="name is reserved"
	Name string `json:"name"`
	// instanceTypes are exact machine types, e.g. m7i.large. Empty: any
	// type of instanceFamilies.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[a-z][a-z0-9-]*[.][a-z0-9]+$`
	InstanceTypes []string `json:"instanceTypes,omitempty"`
	// instanceFamilies are instance families, e.g. m7i, c7g.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=32
	// +kubebuilder:validation:items:Pattern=`^[a-z][a-z0-9-]*$`
	InstanceFamilies []string `json:"instanceFamilies,omitempty"`
	// architectures of the nodes. Empty: Amd64.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=2
	Architectures []Architecture `json:"architectures,omitempty"`
	// capacityTypes are how the nodes are bought. Empty: OnDemand.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=2
	CapacityTypes []CapacityType `json:"capacityTypes,omitempty"`
	// labels of the nodes.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Labels map[string]string `json:"labels,omitempty"`
	// taints of the nodes.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Taints []Taint `json:"taints,omitempty"`
	// limits is the ceiling of what the group may provision. Unset: none.
	// +optional
	Limits *NodeGroupLimits `json:"limits,omitempty"`
}

// NodeGroupLimits bounds what a node group may provision in total.
type NodeGroupLimits struct {
	// cpu is the vCPU ceiling.
	// +optional
	// +kubebuilder:validation:Minimum=1
	CPU *int32 `json:"cpu,omitempty"`
	// memoryGiB is the memory ceiling in GiB.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MemoryGiB *int32 `json:"memoryGiB,omitempty"`
}

// NodeGroupStatus is what the controller observed of one node group.
type NodeGroupStatus struct {
	// name of the group.
	Name string `json:"name"`
	// platformManaged is true for the platform's own groups.
	// +optional
	PlatformManaged bool `json:"platformManaged,omitempty"`
	// phase of the group.
	// +optional
	Phase NodeGroupPhase `json:"phase,omitempty"`
	// message says why the group is not ready.
	// +optional
	Message string `json:"message,omitempty"`
	// nodes is the number of nodes the group runs.
	// +optional
	Nodes int32 `json:"nodes,omitempty"`
	// nodesNotice says why the group's node list is not stored: the results
	// bucket cannot be written. The counts above are still kept.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	NodesNotice string `json:"nodesNotice,omitempty"`
}
