package model

import "time"

// OwnerKind is what a managed resource was created for.
type OwnerKind string

// Owner kinds.
const (
	OwnerCloudAccount OwnerKind = "cloud_account"
	OwnerNetwork      OwnerKind = "network"
	OwnerCluster      OwnerKind = "cluster"
	// OwnerClusterComponents owns the objects of a cluster's platform
	// components (their IAM roles and Helm releases); OwnerID is the
	// cluster's. Kept apart from the cluster's own objects: the cluster's
	// state is read from those only.
	OwnerClusterComponents OwnerKind = "cluster_components"
)

// ManagedResource is one Kubernetes object the platform created in the
// management cluster (a Crossplane managed resource, a ProviderConfig, a
// namespace), recorded so the API can say what was created for which
// account, network or cluster. Its live state is read from the
// cluster, not stored; its cloud identity and the time it went away are
// (history, and what a rebuilt management cluster needs to adopt the
// cloud resources instead of creating new ones).
type ManagedResource struct {
	Base
	TenantID string
	// CloudAccountID is the account (bootstrap ID) the object belongs to.
	CloudAccountID string
	// OwnerKind and OwnerID name the row the object was created for (the
	// account itself, a network or a cluster).
	OwnerKind OwnerKind
	OwnerID   string
	// APIVersion, Kind, Namespace (empty for cluster-scoped objects) and
	// Name identify the object.
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
	// ExternalName is the object's crossplane.io/external-name: the cloud
	// resource's ID (vpc-..., subnet-..., a-...) or name. Empty until the
	// provider created the resource; kept current by Deps.Observe.
	ExternalName string
	// CarriedOver marks an object imported with its owner that the owner's
	// spec does not describe (crossplane.CarriedOverAnnotation): it is kept
	// when the owner is rendered again (Deps.Prune) and deleted with it.
	CarriedOver bool
	// GoneAt is when the object was first seen gone from the management
	// cluster after its owner's deletion was requested (Deps.MarkGone).
	GoneAt *time.Time
}
