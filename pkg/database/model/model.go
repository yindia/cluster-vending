// Package model defines the rows the repositories store (pkg/database/
// repository): tenants, their cloud accounts, networks and clusters, and
// the records of the objects created for them. The one store
// (pkg/database/cloudstore) keeps each as a custom resource of the
// management cluster: a tenant is its namespace, an account, a network and
// a cluster are CloudAccount, Network and Cluster objects in it.
//
//	tenant ─< cloud accounts ─< networks ─< clusters
//
// A network cannot be deleted while clusters use it, and an account while
// networks use it; the store enforces that.
//
// Every row is provider-neutral, like the cloud.v1 contract: Provider says
// which cloud a row belongs to, and the provider-specific parts live in the
// protojson Spec / Status (their provider oneofs).
//
// Every row has a time-ordered UUID (v7) ID. Networks and clusters are
// addressed by it in the API (Network.id, Cluster.id).
package model

import (
	"time"

	"github.com/google/uuid"
)

// Base holds the columns every table has.
type Base struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AssignID assigns a UUIDv7 when the caller did not: time-ordered, so
// ordering by ID is creation order.
func (b *Base) AssignID() error {
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

// Resource holds the fields of every API resource (networks and
// clusters): lifecycle state, optimistic concurrency and the protojson
// spec / status.
type Resource struct {
	Base
	DisplayName string
	Labels      Labels
	// State is the cloud.v1 *State enum name, e.g. "CLUSTER_STATE_RUNNING".
	State string
	// Generation increments on every spec change; ObservedGeneration is the
	// generation the stored status reflects.
	Generation         int64
	ObservedGeneration int64
	// AppliedGeneration is the generation whose objects were all applied
	// without error (rendered in full: not held back for EKS or an
	// upgrade). Below Generation, the resource is applied again.
	AppliedGeneration int64
	// RenderVersion is the renderer version (crossplaneaws.RenderVersion)
	// the resource's objects were last applied with. Below the controller's
	// target, the controller applies it again, so renderer changes
	// roll out resource by resource.
	RenderVersion int64
	// ETag changes on every write; updates must present the current one.
	ETag string
	// Spec and Status are the protojson cloud.v1 *Spec / *Status, including
	// their provider oneof (aws / gcp / azure).
	Spec   JSON
	Status JSON
}

// Meta returns the shared resource columns, for code that handles every
// resource kind the same way (e.g. etag-checked updates).
func (r *Resource) Meta() *Resource { return r }

// Tenant is a customer of the platform. Its ID is the API scope of every
// request.
type Tenant struct {
	ID          string
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Defaults names the tenant after its ID when no display name is given.
func (t *Tenant) Defaults() {
	if t.DisplayName == "" {
		t.DisplayName = t.ID
	}
}

// CloudAccount is a cloud account a tenant gave the platform admin access
// to, registered by TenantService.BootstrapTenant. A tenant has any number
// of them. The platform reaches it with a role / identity whose name is the
// named at bootstrap (AWS: AwsAccount.role_name, stored as Identity), so only the
// account is stored.
type CloudAccount struct {
	Base
	TenantID string
	Provider Provider
	// AccountID is the AWS account ID (12 digits), GCP project ID or Azure
	// subscription ID (UUID).
	AccountID string
	// ProviderConfigName is the account's Crossplane ProviderConfig in the
	// tenant namespace, generated on the first bootstrap and never changed:
	// the managed resources of the account reference it.
	ProviderConfigName string
	// Namespace is the tenant namespace holding the ProviderConfig and the
	// account's managed resources.
	Namespace string
	// Identity is what the ProviderConfig assumes in the account: AWS the
	// tenant role ARN (GCP: service account, Azure: client ID).
	Identity string
	// DirectoryID is the Azure Entra tenant ID (UUID) the subscription
	// belongs to. Empty for AWS and GCP.
	DirectoryID string
	// AccountName is the human-readable name (AWS account name, GCP project
	// name, Azure subscription name). Informational only.
	AccountName string
}

// AssignNew gives a new account its ID and, when the caller set none, its
// ProviderConfig name: "<provider>-<8 random hex>", a Kubernetes object
// name unique within the tenant. An existing account keeps its stored name.
func (a *CloudAccount) AssignNew() error {
	if err := a.AssignID(); err != nil {
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

// Network is a cloud.v1 Network.
type Network struct {
	Resource
	TenantID string
	// Name is cloud.v1 Network.name: chosen by the client, not unique. The
	// API identifier (Network.id) is ID.
	Name string
	// Input is the network as the client sent it (protojson cloud.v1
	// Network); Spec is the resolved one (defaults filled in).
	Input JSON
	// CloudAccountID is the cloud_accounts row of spec.account_id
	// (CloudAccount.Networks owns the foreign key). Its tenant and provider
	// must be the network's; the repository checks it.
	CloudAccountID string
	Provider       Provider
	Region         string
	// ExternalID is the cloud network an imported network stands for (AWS:
	// the VPC ID), unique per account and region: a VPC is imported once.
	// NULL for a network the platform created, and for an import that was
	// blocked (nothing was imported).
	ExternalID *string
	// CreateRequestID is the request_id of the creating call, for
	// idempotent retries. NULL when none was given.
	CreateRequestID *string
}

// Cluster is a cloud.v1 Cluster.
type Cluster struct {
	Resource
	TenantID string
	// Name is cloud.v1 Cluster.name: chosen by the client, unique per
	// tenant (it is the Cluster object's name in the tenant's namespace)
	// and the EKS cluster's name. The API identifier (Cluster.id) is ID.
	Name string
	// Input is the cluster as the client sent it (protojson cloud.v1
	// Cluster); Spec is the resolved one.
	Input JSON
	// NetworkID is the networks row the cluster runs in (Network.Clusters
	// owns the foreign key).
	NetworkID string
	// CloudAccountID is the account of the cluster's network, set by the
	// repository on create.
	CloudAccountID string
	// Provider must match the network's; the repository checks it.
	Provider Provider
	Region   string
	// Imported is true for a cluster that came in through ImportCluster,
	// also after its takeover.
	Imported bool
	// Adoption is what an imported cluster keeps of the resources it was
	// imported from that its spec cannot say (JSON
	// crossplaneaws.Adoption: the IAM role names and trust policies). NULL
	// for clusters the platform created.
	Adoption        JSON
	CreateRequestID *string
	// DeleteForce: the deletion was requested with force (DeleteClusterRequest.force);
	// every later step of it skips the in-cluster cleanup.
	DeleteForce bool
	// ResolvedGeneration is the generation whose cloud lookups were made by
	// the reconciler: the EKS cluster name was found free (a new cluster)
	// and the Kubernetes and add-on versions were resolved against what
	// EKS offers and written to the spec. The API stores a cluster (or an
	// update of its versions) without them; while it is below Generation
	// the reconciler makes them before it renders anything. A call that
	// changes nothing they depend on moves it along with Generation.
	ResolvedGeneration int64
}
