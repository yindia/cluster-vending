// Package repository declares the data access of cluster-vending: what the
// API handlers and the reconcilers store and read. pkg/database/cloudstore
// implements it on the platform's own kinds in the management cluster.
//
// Every Delete removes the row. The repositories enforce the relationships:
// a parent must exist to create a child, and a row in use cannot be
// deleted.
//
// The directory is not called "interface": that is a Go keyword and cannot
// be a package name. Generated mocks of every interface live in
// repository/mock (go generate ./pkg/database/repository, or make mocks).
package repository

//go:generate go tool mockgen -typed -destination=mock/repository.go -package=mock . Store,Tenants,Networks,Clusters,Components,ManagedResources,Events

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
	// RequestAccessCheck records that an access check of the account (by
	// row ID) was asked for at at (CloudAccount.AccessRequestedAt), of
	// regions too when any are given (AccessRequestedRegions; none keeps
	// the ones recorded before); the controller runs it. The row itself is
	// not written. ErrNotFound: no such account.
	RequestAccessCheck(ctx context.Context, tenantID, id string, at time.Time, regions []string) error
	// DeleteCloudAccount asks for the account's deletion (by row ID): the
	// controller removes its ProviderConfig, then the account. ErrInUse,
	// naming them, while networks belong to it; ErrNotFound: no such
	// account.
	DeleteCloudAccount(ctx context.Context, tenantID, id string) error
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
	// Record writes what the controller found out about the network (its
	// state, observed, applied and resolved generations, its status) like
	// Update, but keeps the etag: the user's etag only changes with what the
	// user asked for. ErrConflict when the row changed since it was read.
	Record(ctx context.Context, n *model.Network) error
	// SetStatus stores the status last recorded for the row (what Get and
	// List return) and nothing else: the etag and update time do not
	// change, so recording a status never invalidates a client's etag. It
	// only writes while the row still has etag, the one the status was
	// observed for: a status computed before a call changed the row must
	// not overwrite what that call stored. ErrNotFound when the row is
	// gone, ErrConflict when it changed.
	SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error
	// Delete fails with ErrInUse while clusters use the network.
	Delete(ctx context.Context, tenantID, id string) error
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
	// Record is Networks.Record for clusters.
	Record(ctx context.Context, c *model.Cluster) error
	// SetStatus is Networks.SetStatus for clusters.
	SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error
	// SetDeprecations records the last scan for deprecated API versions
	// (protojson cloud.v1 ClusterDeprecations) and nothing else: the etag,
	// the generation and the status stay.
	SetDeprecations(ctx context.Context, tenantID, id string, deprecations model.JSON) error
	// SetRecommendations records the summary of the usage recorded for
	// resource recommendations (protojson cloud.v1 ClusterRecommendations)
	// and nothing else, like SetDeprecations.
	SetRecommendations(ctx context.Context, tenantID, id string, recommendations model.JSON) error
	// SetResultRef records the summary of one kind of scan result whose
	// full data is in the cluster's results bucket (model.ResultRef), over
	// the summary of that kind and leaving the others, and nothing else.
	// Once a kind is summarised the full list it replaces leaves the
	// cluster: the deprecation findings, and the policy snapshot when
	// violations and violation events both are. A summary that says what
	// is stored writes nothing. ErrInvalid for a kind, bucket or key that
	// is not a results object.
	SetResultRef(ctx context.Context, tenantID, id string, ref model.ResultRef) error
	// SetRollout records the cluster's place in its release channel's
	// rollout and its last automatic upgrade (protojson cloud.v1
	// ClusterRollout) and nothing else, like SetDeprecations.
	SetRollout(ctx context.Context, tenantID, id string, rollout model.JSON) error
	// RequestRetry records a request to recover what failed on the
	// cluster, added to one not yet acted on; the etag stays.
	RequestRetry(ctx context.Context, tenantID, id string, retry model.Retry) error
	// ClearRetry removes the request made at at, once acted on; a newer
	// one stays.
	ClearRetry(ctx context.Context, tenantID, id string, at time.Time) error
	// UsageHistory is the cluster's recorded usage (pkg/recommend, as
	// History.Encode wrote it); empty when none is recorded yet.
	UsageHistory(ctx context.Context, tenantID, id string) ([]byte, error)
	// SetUsageHistory stores the cluster's recorded usage. It is deleted
	// with the cluster.
	SetUsageHistory(ctx context.Context, tenantID, id string, history []byte) error
	// Delete fails with ErrInUse while the cluster has components.
	Delete(ctx context.Context, tenantID, id string) error
}

// Events reads what the controller reported about a tenant's objects:
// the Kubernetes events of its Network and Cluster objects, which the
// management cluster keeps for about an hour. Read-only: the controller
// emits them through its event recorder, not through the store.
type Events interface {
	// ListFor returns the events about the tenant's object of kind
	// ("Network", "Cluster") named name, newest first. A tenant without a
	// namespace, or an object without events, has none.
	ListFor(ctx context.Context, tenantID, kind, name string) ([]model.Event, error)
}

// Components stores the components enabled on a tenant's clusters: at
// most one of each catalog component per cluster.
type Components interface {
	// Create assigns ID, ETag and generation 1. It fails with ErrNotFound
	// when the cluster is missing, ErrConflict when it is being deleted,
	// ErrAlreadyExists when the cluster has the component or the create
	// request ID is taken, and ErrInvalid when the values are not a JSON
	// object of at most 64 KiB.
	Create(ctx context.Context, c *model.Component) error
	// Get finds a component by row ID (the API's Component.id).
	Get(ctx context.Context, tenantID, id string) (*model.Component, error)
	GetByCreateRequest(ctx context.Context, tenantID, requestID string) (*model.Component, error)
	// List orders by ID (time-ordered: oldest first); page.After is an ID.
	List(ctx context.Context, tenantID string, page Page) ([]model.Component, error)
	// ListByCluster returns a cluster's components, oldest first.
	ListByCluster(ctx context.Context, tenantID, clusterID string) ([]model.Component, error)
	// Update writes version, values, state, generations and the force flag
	// if the stored etag equals c.ETag, and assigns a new ETag. The cluster
	// and the component cannot change (ErrInvalid).
	Update(ctx context.Context, c *model.Component) error
	// SetStatus is Networks.SetStatus for components.
	SetStatus(ctx context.Context, tenantID, id, etag string, status model.JSON) error
	Delete(ctx context.Context, tenantID, id string) error
}

// ManagedResources records the Kubernetes objects created for accounts,
// networks and clusters.
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
	// own and those of its networks and clusters.
	ListByAccount(ctx context.Context, tenantID, cloudAccountID string) ([]model.ManagedResource, error)
	// DeleteByOwner forgets an owner's objects, once they are gone from the
	// cluster.
	DeleteByOwner(ctx context.Context, kind model.OwnerKind, ownerID string) error
	// Delete forgets one object, by row ID.
	Delete(ctx context.Context, id string) error
	// SetExternalName stores an object's external name, by row ID.
	SetExternalName(ctx context.Context, id, externalName string) error
	// SetGoneAt stores when an object was first seen gone, by row ID; a
	// time already stored is kept.
	SetGoneAt(ctx context.Context, id string, at time.Time) error
}

// Store groups the repositories. WithinTransaction runs fn with
// repositories that take back what fn created when fn fails (the store
// says how far that goes).
type Store interface {
	Tenants() Tenants
	Networks() Networks
	Clusters() Clusters
	Components() Components
	ManagedResources() ManagedResources
	Events() Events
	WithinTransaction(ctx context.Context, fn func(Store) error) error
}
