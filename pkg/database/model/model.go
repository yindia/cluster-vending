// Package model defines the database tables of cluster-vending.
//
//	tenants ─┬─< cloud_accounts ─< networks   a tenant has any number of
//	         │                       │         accounts; a network lives in one
//	         ├─< networks ───────────┘         (cloud_account_id)
//	         └─< clusters ── network_id -> networks: a network cannot be
//	                │              deleted while clusters use it; a cluster
//	                │              runs in its network's account
//	                └─< node_pools   deleted together with their cluster
//
// A tenant cannot be deleted while it has networks or clusters.
//
// Nothing is ever removed: deleting a row sets deleted_at (soft delete), and
// reads skip deleted rows. The lifecycle is State (e.g. CLUSTER_STATE_DELETING
// while Crossplane tears the cloud resources down), then deleted_at once
// they are gone. Because rows stay, the relationships above are enforced by
// the repositories (pkg/database/implementation); the foreign keys remain
// as a safety net against hard deletes. Unique names only count rows that
// are not deleted, so a deleted name can be reused.
//
// Every table is provider-neutral, like the cloud.v1 contract: Provider says
// which cloud a row belongs to, and the provider-specific parts live in the
// protojson Spec / Status (their provider oneofs).
//
// Every row has a time-ordered UUID (v7) primary key. Networks and clusters
// are addressed by it in the API (Network.id, Cluster.id); their Name is a
// free prefix. Node pools are still addressed by Name (NodePool.id), unique
// per cluster.
package model

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Base holds the columns every table has.
type Base struct {
	ID        string `gorm:"type:varchar(36);primaryKey"`
	CreatedAt time.Time
	UpdatedAt time.Time
	// DeletedAt marks a soft-deleted row; reads skip it.
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

// BeforeCreate assigns a UUIDv7 when the caller did not: time-ordered, so
// ordering by ID is creation order.
func (b *Base) BeforeCreate(*gorm.DB) error {
	if b.ID != "" {
		return nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	b.ID = id.String()
	return nil
}

// Resource holds the columns of every API resource (networks, clusters,
// node pools): lifecycle state, optimistic concurrency and the protojson
// spec / status.
type Resource struct {
	Base
	DisplayName string `gorm:"size:128;not null;default:''"`
	Labels      Labels
	// State is the cloud.v1 *State enum name, e.g. "CLUSTER_STATE_RUNNING".
	State string `gorm:"size:64;not null;index"`
	// Generation increments on every spec change; ObservedGeneration is the
	// generation the stored status reflects.
	Generation         int64 `gorm:"not null;default:1"`
	ObservedGeneration int64 `gorm:"not null;default:0"`
	// ETag changes on every write; updates must present the current one.
	ETag string `gorm:"column:etag;size:64;not null"`
	// Spec and Status are the protojson cloud.v1 *Spec / *Status, including
	// their provider oneof (aws / gcp / azure).
	Spec   JSON `gorm:"not null"`
	Status JSON
}

// Meta returns the shared resource columns, for code that handles every
// resource kind the same way (e.g. etag-checked updates).
func (r *Resource) Meta() *Resource { return r }

// Tenant is a customer of the platform. tenant_id is the primary key: it
// is the API scope of every request.
type Tenant struct {
	ID          string `gorm:"type:varchar(56);primaryKey"`
	DisplayName string `gorm:"size:128;not null;default:''"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   gorm.DeletedAt `gorm:"index"`

	CloudAccounts []CloudAccount `gorm:"constraint:OnDelete:RESTRICT"`
	Networks      []Network      `gorm:"constraint:OnDelete:RESTRICT"`
	Clusters      []Cluster      `gorm:"constraint:OnDelete:RESTRICT"`
}

// BeforeCreate names the tenant after its ID when no display name is given.
func (t *Tenant) BeforeCreate(*gorm.DB) error {
	if t.DisplayName == "" {
		t.DisplayName = t.ID
	}
	return nil
}

// CloudAccount is a cloud account a tenant gave the platform admin access
// to, registered by TenantService.BootstrapTenant. A tenant has any number
// of them. The platform reaches it with a role / identity whose name is the
// named at bootstrap (AWS: AwsAccount.role_name, stored as Identity), so only the
// account is stored.
type CloudAccount struct {
	Base
	TenantID string   `gorm:"type:varchar(56);not null;uniqueIndex:idx_cloud_accounts_tenant_account,where:deleted_at IS NULL;uniqueIndex:idx_cloud_accounts_tenant_provider_config,where:deleted_at IS NULL"`
	Provider Provider `gorm:"size:16;not null;check:chk_cloud_accounts_provider,provider IN ('aws','gcp','azure');uniqueIndex:idx_cloud_accounts_tenant_account,where:deleted_at IS NULL"`
	// AccountID is the AWS account ID (12 digits), GCP project ID or Azure
	// subscription ID (UUID).
	AccountID string `gorm:"size:128;not null;uniqueIndex:idx_cloud_accounts_tenant_account,where:deleted_at IS NULL"`
	// ProviderConfigName is the account's Crossplane ProviderConfig in the
	// tenant namespace, generated on the first bootstrap and never changed:
	// the managed resources of the account reference it.
	ProviderConfigName string `gorm:"size:63;not null;default:'';uniqueIndex:idx_cloud_accounts_tenant_provider_config,where:deleted_at IS NULL"`
	// Namespace is the tenant namespace holding the ProviderConfig and the
	// account's managed resources.
	Namespace string `gorm:"size:63;not null;default:''"`
	// Identity is what the ProviderConfig assumes in the account: AWS the
	// tenant role ARN (GCP: service account, Azure: client ID).
	Identity string `gorm:"size:2048;not null;default:''"`
	// DirectoryID is the Azure Entra tenant ID (UUID) the subscription
	// belongs to. Empty for AWS and GCP.
	DirectoryID string `gorm:"size:64;not null;default:''"`
	// AccountName is the human-readable name (AWS account name, GCP project
	// name, Azure subscription name). Informational only.
	AccountName string `gorm:"size:128;not null;default:''"`

	Networks []Network `gorm:"constraint:OnDelete:RESTRICT"`
}

// BeforeCreate generates the ProviderConfig name when the caller set none:
// "<provider>-<8 random hex>", a Kubernetes object name unique within the
// tenant. On an upsert of an existing account the stored name is kept.
func (a *CloudAccount) BeforeCreate(tx *gorm.DB) error {
	if err := a.Base.BeforeCreate(tx); err != nil {
		return err
	}
	if a.ProviderConfigName == "" {
		name, err := newProviderConfigName(a.Provider)
		if err != nil {
			return err
		}
		a.ProviderConfigName = name
	}
	return nil
}

// BeforeSave rejects accounts that do not fit their provider.
func (a *CloudAccount) BeforeSave(*gorm.DB) error {
	return ValidateAccount(a.Provider, a.AccountID, a.DirectoryID)
}

// Network is a cloud.v1 Network.
type Network struct {
	Resource
	TenantID string `gorm:"type:varchar(56);not null;index:idx_networks_tenant_name_lookup;uniqueIndex:idx_networks_tenant_request,where:deleted_at IS NULL"`
	// Name is cloud.v1 Network.name: chosen by the client, not unique. The
	// API identifier (Network.id) is ID.
	Name string `gorm:"size:40;not null;index:idx_networks_tenant_name_lookup"`
	// Input is the network as the client sent it (protojson cloud.v1
	// Network); Spec is the resolved one (defaults filled in).
	Input JSON
	// CloudAccountID is the cloud_accounts row of spec.account_id
	// (CloudAccount.Networks owns the foreign key). Its tenant and provider
	// must be the network's; the repository checks it.
	CloudAccountID string   `gorm:"type:varchar(36);not null;index"`
	Provider       Provider `gorm:"size:16;not null;check:chk_networks_provider,provider IN ('aws','gcp','azure')"`
	Region         string   `gorm:"size:64;not null"`
	// CreateRequestID is the request_id of the creating call, for
	// idempotent retries. NULL when none was given.
	CreateRequestID *string `gorm:"size:36;uniqueIndex:idx_networks_tenant_request,where:deleted_at IS NULL"`

	Clusters []Cluster `gorm:"constraint:OnDelete:RESTRICT"`
}

// BeforeSave rejects unknown providers.
func (n *Network) BeforeSave(*gorm.DB) error { return n.Provider.Validate() }

// Cluster is a cloud.v1 Cluster.
type Cluster struct {
	Resource
	TenantID string `gorm:"type:varchar(56);not null;index:idx_clusters_tenant_name_lookup;uniqueIndex:idx_clusters_tenant_request,where:deleted_at IS NULL"`
	// Name is cloud.v1 Cluster.name: chosen by the client, not unique. The
	// API identifier (Cluster.id) is ID.
	Name string `gorm:"size:40;not null;index:idx_clusters_tenant_name_lookup"`
	// Input is the cluster as the client sent it (protojson cloud.v1
	// Cluster); Spec is the resolved one.
	Input JSON
	// NetworkID is the networks row the cluster runs in (Network.Clusters
	// owns the foreign key).
	NetworkID string `gorm:"type:varchar(36);not null;index"`
	// Provider must match the network's; the repository checks it.
	Provider Provider `gorm:"size:16;not null;check:chk_clusters_provider,provider IN ('aws','gcp','azure')"`
	Region   string   `gorm:"size:64;not null"`
	// Imported is true while spec.existing_cluster is set (temporary
	// cluster import).
	Imported        bool    `gorm:"not null;default:false"`
	CreateRequestID *string `gorm:"size:36;uniqueIndex:idx_clusters_tenant_request,where:deleted_at IS NULL"`

	NodePools []NodePool `gorm:"constraint:OnDelete:RESTRICT"`
}

// BeforeSave rejects unknown providers.
func (c *Cluster) BeforeSave(*gorm.DB) error { return c.Provider.Validate() }

// NodePool is a cloud.v1 NodePool; its provider is its cluster's. The
// default pool of a cluster is a row too, with Name "default".
type NodePool struct {
	Resource
	ClusterID string `gorm:"type:varchar(36);not null;uniqueIndex:idx_node_pools_cluster_name,where:deleted_at IS NULL;uniqueIndex:idx_node_pools_cluster_request,where:deleted_at IS NULL"`
	// Name is cloud.v1 NodePool.id.
	Name string `gorm:"size:40;not null;uniqueIndex:idx_node_pools_cluster_name,where:deleted_at IS NULL"`
	// CarriedOver marks a node group taken over verbatim from an import.
	CarriedOver     bool    `gorm:"not null;default:false"`
	CreateRequestID *string `gorm:"size:36;uniqueIndex:idx_node_pools_cluster_request,where:deleted_at IS NULL"`
}

// All lists every model, parents before children, for migrations.
func All() []any {
	return []any{&Tenant{}, &CloudAccount{}, &Network{}, &Cluster{}, &NodePool{}, &ManagedResource{}}
}
