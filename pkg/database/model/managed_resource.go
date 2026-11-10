package model

// OwnerKind is what a managed resource was created for.
type OwnerKind string

// Owner kinds.
const (
	OwnerCloudAccount OwnerKind = "cloud_account"
	OwnerNetwork      OwnerKind = "network"
	OwnerCluster      OwnerKind = "cluster"
	OwnerNodePool     OwnerKind = "node_pool"
)

// ManagedResource is one Kubernetes object the platform created in the
// management cluster (a Crossplane managed resource, a ProviderConfig, a
// namespace), recorded so the API can say what was created for which
// account, network, cluster or node pool. Its live state is read from the
// cluster, not stored.
type ManagedResource struct {
	Base
	TenantID string `gorm:"type:varchar(56);not null;index"`
	// CloudAccountID is the account (bootstrap ID) the object belongs to.
	CloudAccountID string `gorm:"type:varchar(36);not null;index"`
	// OwnerKind and OwnerID name the row the object was created for (the
	// account itself, a network, cluster or node pool).
	OwnerKind OwnerKind `gorm:"size:32;not null;check:chk_managed_resources_owner_kind,owner_kind IN ('cloud_account','network','cluster','node_pool');uniqueIndex:idx_managed_resources_object,where:deleted_at IS NULL;index:idx_managed_resources_owner"`
	OwnerID   string    `gorm:"type:varchar(36);not null;uniqueIndex:idx_managed_resources_object,where:deleted_at IS NULL;index:idx_managed_resources_owner"`
	// APIVersion, Kind, Namespace (empty for cluster-scoped objects) and
	// Name identify the object.
	APIVersion string `gorm:"size:128;not null;uniqueIndex:idx_managed_resources_object,where:deleted_at IS NULL"`
	Kind       string `gorm:"size:64;not null;uniqueIndex:idx_managed_resources_object,where:deleted_at IS NULL"`
	Namespace  string `gorm:"size:63;not null;default:'';uniqueIndex:idx_managed_resources_object,where:deleted_at IS NULL"`
	Name       string `gorm:"size:253;not null;uniqueIndex:idx_managed_resources_object,where:deleted_at IS NULL"`
}
