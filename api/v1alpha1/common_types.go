package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// The kinds of this group form one ownership chain inside a tenant's
// namespace:
//
//	CloudAccount  owns  Network  owns  Cluster
//
// A Network names its account (spec.accountRef) and a Cluster names its
// network (spec.network.networkRef); both references are required and
// immutable. One account has many networks, one network many clusters. The
// controller sets a controller owner reference along the chain
// (blockOwnerDeletion), so `kubectl get` shows who owns what and the
// garbage collector knows the order.
//
// Deletion rule: an owner is not deleted while it has dependents. Each
// kind carries a finalizer; the owner's finalizer is released only once no
// dependent is left. Deleting an account therefore never deletes its
// networks and clusters by cascade: it waits (and says so in its
// conditions) until they have been deleted themselves.

// Finalizers of the kinds: held until the object's cloud resources are
// gone and, for an owner, until it has no dependents left.
const (
	CloudAccountFinalizer = "cloud.cluster-vending.io/cloudaccount"
	NetworkFinalizer      = "cloud.cluster-vending.io/network"
	ClusterFinalizer      = "cloud.cluster-vending.io/cluster"
)

// Labels the controller sets on dependents, so the dependents of an owner
// are a label selection.
const (
	// LabelAccount is the name of the CloudAccount a Network or Cluster
	// belongs to.
	LabelAccount = "cloud.cluster-vending.io/account"
	// LabelNetwork is the name of the Network a Cluster runs in.
	LabelNetwork = "cloud.cluster-vending.io/network"
	// LabelCluster is the name of the Cluster a Component is enabled on.
	LabelCluster = "cloud.cluster-vending.io/cluster"
)

// Condition types every kind reports.
const (
	// ConditionReady is True once every cloud resource of the object exists
	// and matches the spec.
	ConditionReady = "Ready"
	// ConditionSynced is False while the last attempt to apply the spec
	// failed.
	ConditionSynced = "Synced"
)

// LocalReference names an object of this group in the same namespace.
type LocalReference struct {
	// name of the referenced object.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// Tags are applied to every cloud resource of the object, next to the
// platform's own tags. They follow the AWS tag rules, which Kubernetes
// labels do not (spaces, 256-character values): hence a spec field and not
// metadata.labels.
// +kubebuilder:validation:MaxProperties=40
// Only the lengths are checked here: the character rules are the API's
// (protovalidate on the contract), which also checks objects the
// controller adopts. A regular expression per key and value exceeds the
// API server's CEL cost budget.
// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) >= 1 && size(k) <= 128 && size(self[k]) <= 256)",message="tag keys are 1-128 and values 0-256 characters"
type Tags map[string]string

// InventoryEntry is one Crossplane object the owner has in the management
// cluster.
type InventoryEntry struct {
	// apiVersion of the object.
	// +required
	APIVersion string `json:"apiVersion"`
	// kind of the object.
	// +required
	Kind string `json:"kind"`
	// name of the object.
	// +required
	Name string `json:"name"`
	// namespace of the object. Empty for a cluster-scoped object (the
	// tenant's namespace itself, recorded for its account).
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// externalName is the cloud identifier of the resource the object
	// stands for (crossplane.io/external-name): what a rebuilt management
	// cluster needs to adopt the resource instead of creating a new one.
	// Empty until the provider created the resource.
	// +optional
	ExternalName string `json:"externalName,omitempty"`
	// carriedOver marks an object that came with an import and that no spec
	// field describes: it is never removed when the owner is rendered
	// again, and is deleted with the owner.
	// +optional
	CarriedOver bool `json:"carriedOver,omitempty"`
	// recordedAt is when the object was first recorded as the owner's. An
	// object recorded a moment ago and not in the management cluster yet
	// is being created, not missing.
	// +optional
	RecordedAt *metav1.Time `json:"recordedAt,omitempty"`
	// goneAt is when the object was first seen gone from the management
	// cluster after its owner's deletion was requested.
	// +optional
	GoneAt *metav1.Time `json:"goneAt,omitempty"`
}

// ImportPhase is how far an import is.
// +kubebuilder:validation:Enum=Discovering;Planned;Blocked;Converging;Converged
type ImportPhase string

// Import phases.
const (
	// ImportPhaseDiscovering: the existing resources are being looked up.
	ImportPhaseDiscovering ImportPhase = "Discovering"
	// ImportPhasePlanned: imported read-only; the plan is ready to review.
	ImportPhasePlanned ImportPhase = "Planned"
	// ImportPhaseBlocked: the plan has Blocking lines; nothing was imported.
	ImportPhaseBlocked ImportPhase = "Blocked"
	// ImportPhaseConverging: taken over; the resources are being brought to
	// the spec.
	ImportPhaseConverging ImportPhase = "Converging"
	// ImportPhaseConverged: taken over and matching the spec.
	ImportPhaseConverged ImportPhase = "Converged"
)

// Disposition is what a takeover does with one discovered resource.
// +kubebuilder:validation:Enum=Matches;WillUpdate;WillCreate;NotImported;Blocking;CarriedOver
type Disposition string

// Dispositions.
const (
	// DispositionMatches: the resource already is what the spec describes.
	DispositionMatches Disposition = "Matches"
	// DispositionWillUpdate: the takeover changes the resource (detail says
	// what).
	DispositionWillUpdate Disposition = "WillUpdate"
	// DispositionWillCreate: the spec needs a resource that does not exist.
	DispositionWillCreate Disposition = "WillCreate"
	// DispositionNotImported: the resource stays outside the platform.
	DispositionNotImported Disposition = "NotImported"
	// DispositionBlocking: the import cannot proceed because of it.
	DispositionBlocking Disposition = "Blocking"
	// DispositionCarriedOver: owned as it is; no spec field describes it.
	DispositionCarriedOver Disposition = "CarriedOver"
)

// ImportedResource is one line of an import plan.
type ImportedResource struct {
	// apiVersion of the Crossplane object the resource is (or would be).
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// kind of that object.
	// +required
	Kind string `json:"kind"`
	// name of that object; empty for a resource that is not imported.
	// +optional
	Name string `json:"name,omitempty"`
	// externalName is the resource's cloud identifier.
	// +optional
	ExternalName string `json:"externalName,omitempty"`
	// specPath is the spec field that describes the resource, if any.
	// +optional
	SpecPath string `json:"specPath,omitempty"`
	// disposition is what a takeover does with the resource.
	// +required
	Disposition Disposition `json:"disposition"`
	// detail explains the disposition.
	// +optional
	Detail string `json:"detail,omitempty"`
	// requiresReplacement is true when bringing the resource to the spec
	// replaces it.
	// +optional
	RequiresReplacement bool `json:"requiresReplacement,omitempty"`
}

// ImportPlan is the plan of an import: what was found and what a takeover
// does with each resource. It is kept as a record after the takeover.
type ImportPlan struct {
	// phase of the import.
	// +optional
	Phase ImportPhase `json:"phase,omitempty"`
	// resources are the plan's lines.
	// +listType=atomic
	// +optional
	Resources []ImportedResource `json:"resources,omitempty"`
}

// Takeover is the explicit step from an imported, read-only object to one
// the platform owns. Nothing in the cloud changes before takeOver is true.
// It is inlined in a kind's importFrom, which carries the rule that a
// takeover cannot be undone.
type Takeover struct {
	// takeOver makes the platform the owner of the imported resources:
	// they switch from observe-only to full management and are brought to
	// the spec (the plan's WillUpdate and WillCreate lines). Once true it
	// stays true.
	// +optional
	TakeOver bool `json:"takeOver,omitempty"`
	// acknowledgeNotImported confirms that the plan's NotImported resources
	// stay outside the platform. A takeover of a plan with such lines waits
	// for it.
	// +optional
	AcknowledgeNotImported bool `json:"acknowledgeNotImported,omitempty"`
}

// NetworkResources are the cloud resources of a network, read from the
// Crossplane objects' status.
type NetworkResources struct {
	// cloudNetworkID is the network's cloud identifier (AWS: the VPC ID).
	// +optional
	CloudNetworkID string `json:"cloudNetworkID,omitempty"`
	// publicSubnetIDs are the cloud identifiers of the public subnets.
	// +listType=set
	// +optional
	PublicSubnetIDs []string `json:"publicSubnetIDs,omitempty"`
	// privateSubnetIDs are the cloud identifiers of the private subnets.
	// +listType=set
	// +optional
	PrivateSubnetIDs []string `json:"privateSubnetIDs,omitempty"`
	// natGatewayIDs are the cloud identifiers of the NAT gateways.
	// +listType=set
	// +optional
	NATGatewayIDs []string `json:"natGatewayIDs,omitempty"`
	// egressIPs are the public IPv4 addresses egress leaves through, for
	// allow-lists downstream.
	// +listType=set
	// +optional
	EgressIPs []string `json:"egressIPs,omitempty"`
}
