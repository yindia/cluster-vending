// Package repository declares data access for cluster-vending, independent
// of the database. pkg/database/implementation implements it with GORM.
//
// Nothing is removed: every Delete is a soft delete (it sets deleted_at),
// and Get / List never return deleted rows. The repositories enforce the
// relationships themselves, because soft-deleted rows still exist: a parent
// must be live to create a child, and a row in use cannot be deleted.
//
// The directory is not called "interface": that is a Go keyword and cannot
// be a package name. Generated mocks of every interface live in
// repository/mock (go generate ./pkg/database/repository, or make mocks).
package repository

//go:generate go tool mockgen -typed -destination=mock/repository.go -package=mock . Store,Tenants,Networks,Clusters,NodePools,ManagedResources

import (
	"context"
	"errors"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// Errors returned by every repository, to be mapped to Connect codes.
var (
	// ErrNotFound: the row, or the parent it points at, does not exist
	// (NOT_FOUND).
	ErrNotFound = errors.New("not found")
	// ErrAlreadyExists: the name or create request ID is taken
	// (ALREADY_EXISTS).
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict: the row changed since it was read; the etag did not
	// match (ABORTED).
	ErrConflict = errors.New("etag mismatch")
	// ErrInUse: other rows still reference this one, e.g. a network used by
	// clusters (FAILED_PRECONDITION).
	ErrInUse = errors.New("in use")
	// ErrInvalid: the row does not fit its provider, or a cluster's provider
	// or tenant differs from its network's (INVALID_ARGUMENT).
	ErrInvalid = errors.New("invalid")
)

// Page selects one page of a list ordered by name.
type Page struct {
	// After is the last name of the previous page; empty for the first.
	After string
	// Limit is the page size; it must be positive.
	Limit int
}

// Tenants stores tenants and their cloud accounts.
type Tenants interface {
	Create(ctx context.Context, t *model.Tenant) error
	Get(ctx context.Context, id string) (*model.Tenant, error)
	// Delete fails with ErrInUse while the tenant has networks or clusters;
	// it also deletes the tenant's cloud accounts.
	Delete(ctx context.Context, id string) error
	// PutCloudAccount creates the account (tenant, provider, account ID) or
	// updates its name, directory, namespace and identity.
	// ProviderConfigName is only written on create: an existing account
	// keeps its own.
	PutCloudAccount(ctx context.Context, account *model.CloudAccount) error
	// GetCloudAccount finds an account by its cloud identifier.
	GetCloudAccount(ctx context.Context, tenantID string, provider model.Provider, accountID string) (*model.CloudAccount, error)
	// GetCloudAccountByID finds an account by its row ID (the API's
	// bootstrap ID).
	GetCloudAccountByID(ctx context.Context, tenantID, id string) (*model.CloudAccount, error)
	// ListCloudAccounts returns the tenant's accounts, ordered by provider
	// then account ID.
	ListCloudAccounts(ctx context.Context, tenantID string) ([]model.CloudAccount, error)
}

// Networks stores networks of a tenant.
type Networks interface {
	// Create assigns ID and ETag. It fails with ErrNotFound when the cloud
	// account is missing or deleted, and ErrInvalid when it belongs to
	// another tenant or provider.
	Create(ctx context.Context, n *model.Network) error
	// Get finds a network by row ID (the API's Network.id).
	Get(ctx context.Context, tenantID, id string) (*model.Network, error)
	GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Network, error)
	// List orders by ID (time-ordered: oldest first); page.After is an ID.
	List(ctx context.Context, tenantID string, page Page) ([]model.Network, error)
	// Update writes every column of n if the stored etag equals n.ETag, and
	// assigns a new ETag.
	Update(ctx context.Context, n *model.Network) error
	// Delete fails with ErrInUse while clusters use the network.
	Delete(ctx context.Context, tenantID, id string) error
	// ListPending returns, across tenants, the networks the background loop
	// must look at: in one of states, not fully applied
	// (applied_generation < generation), or rendered by a renderer older
	// than renderTarget.
	ListPending(ctx context.Context, states []string, renderTarget int64) ([]model.Network, error)
}

// Clusters stores clusters of a tenant.
type Clusters interface {
	// Create fails with ErrNotFound when the network is missing or deleted,
	// and ErrInvalid when it belongs to another tenant or provider.
	Create(ctx context.Context, c *model.Cluster) error
	// Get finds a cluster by row ID (the API's Cluster.id).
	Get(ctx context.Context, tenantID, id string) (*model.Cluster, error)
	GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Cluster, error)
	// List orders by ID (time-ordered: oldest first); page.After is an ID.
	List(ctx context.Context, tenantID string, page Page) ([]model.Cluster, error)
	// ListByNetwork returns the clusters using a network (by network row ID).
	ListByNetwork(ctx context.Context, networkID string) ([]model.Cluster, error)
	// ListByName returns the tenant's clusters with this name, oldest first.
	ListByName(ctx context.Context, tenantID, name string) ([]model.Cluster, error)
	Update(ctx context.Context, c *model.Cluster) error
	// Delete also deletes the cluster's node pools.
	Delete(ctx context.Context, tenantID, id string) error
	// ListPending is Networks.ListPending for clusters.
	ListPending(ctx context.Context, states []string, renderTarget int64) ([]model.Cluster, error)
	// CountByState counts the clusters of every stored state, across
	// tenants.
	CountByState(ctx context.Context) (map[string]int64, error)
}

// NodePools stores node pools of a cluster (by cluster row ID).
type NodePools interface {
	Create(ctx context.Context, p *model.NodePool) error
	Get(ctx context.Context, clusterID, name string) (*model.NodePool, error)
	List(ctx context.Context, clusterID string, page Page) ([]model.NodePool, error)
	Update(ctx context.Context, p *model.NodePool) error
	Delete(ctx context.Context, clusterID, name string) error
}

// ManagedResources records the Kubernetes objects created for accounts,
// networks, clusters and node pools.
type ManagedResources interface {
	// Put records objects, all for one owner; recording an object again is
	// a no-op.
	Put(ctx context.Context, objects []model.ManagedResource) error
	// ListByOwner returns an owner's objects in the order they were recorded.
	ListByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) ([]model.ManagedResource, error)
	// ListByOwners is ListByOwner for several owners of one kind in one
	// query, keyed by owner ID.
	ListByOwners(ctx context.Context, kind model.OwnerKind, ownerIDs []string) (map[string][]model.ManagedResource, error)
	// ListByAccount returns every object of an account (bootstrap ID): its
	// own and those of its networks, clusters and node pools.
	ListByAccount(ctx context.Context, tenantID, cloudAccountID string) ([]model.ManagedResource, error)
	// DeleteByOwner forgets an owner's objects (soft delete), once they are
	// gone from the cluster.
	DeleteByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) error
	// Delete forgets one object (soft delete), by row ID.
	Delete(ctx context.Context, id string) error
	// SetExternalName stores an object's external name, by row ID.
	SetExternalName(ctx context.Context, id, externalName string) error
	// SetGoneAt stores when an object was first seen gone, by row ID; a
	// time already stored is kept.
	SetGoneAt(ctx context.Context, id string, at time.Time) error
}

// Store groups the repositories. WithinTransaction runs fn with
// repositories bound to one transaction: fn's error rolls it back.
type Store interface {
	Tenants() Tenants
	Networks() Networks
	Clusters() Clusters
	NodePools() NodePools
	ManagedResources() ManagedResources
	WithinTransaction(ctx context.Context, fn func(Store) error) error
}
