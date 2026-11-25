package v1alpha1

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

// NodeGroupStatus is what the controller observed of one node group.
type NodeGroupStatus struct {
	// name of the group.
	Name string `json:"name"`
	// platformManaged is true for the platform's own groups; false only for
	// a removed tenant group while it is deleted.
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
	// usage is the group's resource totals, from the controller's node pass.
	// +optional
	NodeGroupUsage `json:",inline"`
}

// NodeGroupUsage are the resource totals of a node group's nodes and pods. The
// scheduler's definition of requested (PROP-02, 5.3): max(init containers) vs
// sum(containers), plus overhead; finished pods excluded; a missing request
// counts as zero.
type NodeGroupUsage struct {
	// ready nodes of the group.
	// +optional
	Ready int32 `json:"ready,omitempty"`
	// notReady nodes of the group.
	// +optional
	NotReady int32 `json:"notReady,omitempty"`
	// allocatableCPUMillis, allocatableMemoryBytes and allocatablePods are the
	// sums of the nodes' status.allocatable.
	// +optional
	AllocatableCPUMillis int64 `json:"allocatableCPUMillis,omitempty"`
	// +optional
	AllocatableMemoryBytes int64 `json:"allocatableMemoryBytes,omitempty"`
	// +optional
	AllocatablePods int64 `json:"allocatablePods,omitempty"`
	// requestedCPUMillis and requestedMemoryBytes are the sums over the pods
	// bound to the group's nodes.
	// +optional
	RequestedCPUMillis int64 `json:"requestedCPUMillis,omitempty"`
	// +optional
	RequestedMemoryBytes int64 `json:"requestedMemoryBytes,omitempty"`
	// podCount is the pods bound to the nodes that are not finished.
	// +optional
	PodCount int32 `json:"podCount,omitempty"`
	// podsWithoutRequests are the pods with a container that sets no CPU or
	// memory request.
	// +optional
	PodsWithoutRequests int32 `json:"podsWithoutRequests,omitempty"`
}
