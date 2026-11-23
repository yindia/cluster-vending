// Package reconcile is what moves networks and clusters towards their
// stored spec: the dependencies that work needs (the store, the
// management cluster, how tenant cloud accounts are reached) and what
// every kind of owner shares (recording, observing and deleting its
// objects). It knows no RPC library: failures a caller can act on are
// faults (pkg/fault). The controller reconciles with it; the API handlers
// use it to check a request against the store and to read.
package reconcile

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/results"

	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// Deps are what reconciling works with: the repository backed by Kubernetes
// custom resources, the management cluster where Crossplane managed resources
// are created, and how tenant cloud accounts are reached.
// ResultsS3 makes the S3 client that reads and writes the results buckets of
// an account, in a region.
type ResultsS3 func(a awscloud.Access, region string) (results.S3API, error)

// InstanceProfiles makes the IAM client that reaches an account's instance
// profiles, as its tenant role, in a region.
type InstanceProfiles func(a awscloud.Access, region string) (awscloud.InstanceProfileAPI, error)

type Deps struct {
	// ManagedResourceKinds lists the managed kinds supplied by active provider adapters.
	// Nil preserves the current AWS installation; a future GCP adapter can
	// supply its kinds without changing inventory, planning or freeze logic.
	ManagedResourceKinds []schema.GroupVersionKind
	FreezeBarrier        *FreezeBarrier
	Store                repository.Store
	// Kube reads from the informer cache (the server's manager) and writes
	// to the API server; tests use a plain client.
	Kube client.Client
	// KubeReader reads from the API server, uncached: for objects the cache
	// does not have yet (just applied) or does not hold (no managed-by
	// label, managedFields). Nil: reads use Kube only.
	KubeReader client.Reader
	// RenderTarget is the renderer version resources are brought to by
	// what reconciles (0: none are re-applied for it). RenderRollout limits
	// that to these tenant, network or cluster IDs; empty means all.
	RenderTarget  int64
	RenderRollout []string
	// The AWS lookups are what the reconcilers look the cloud up
	// with (management.WithAWS). Only what reconciles has them: the API
	// handlers are built without, and make no AWS call.
	//
	// AWSDiscovery looks the VPC of an imported network up; nil: the
	// import fails as unavailable.
	AWSDiscovery awscloud.NetworkDiscoverer
	// AWSClusterDiscovery looks the EKS cluster of an imported cluster up;
	// nil: the import fails as unavailable.
	AWSClusterDiscovery awscloud.ClusterDiscoverer
	// AWSClusters checks a new cluster's name against EKS and resolves the
	// versions EKS offers; nil skips both (tests): EKS chooses at create.
	AWSClusters awscloud.ClusterLookup
	// AWSAccessCheck checks that the tenant role can be used and does what
	// the platform needs in an account (account.Reconciler.VerifyAccess);
	// nil: accounts are not checked (tests, the API handlers).
	AWSAccessCheck awscloud.AccessChecker
	// ResultsS3 makes the S3 client of an account's tenant role in a region
	// (Deps.AWSAccess gives the access): the scanners write the cluster's
	// results bucket with it and a cluster's deletion empties it. Nil: no
	// results are written to S3 and a bucket is not emptied (tests).
	ResultsS3 ResultsS3
	// InstanceProfiles makes the IAM client with which a cluster's deletion
	// takes Karpenter's node role out of the instance profiles Karpenter
	// made, before it deletes the role. Nil: none is removed (tests).
	InstanceProfiles InstanceProfiles
	// ProviderCredentials, when set, is the Secret every account's
	// ProviderConfig authenticates the Crossplane AWS providers with,
	// instead of IRSA (a management cluster without IRSA, such as a local
	// kind cluster). The tenant role is assumed either way. Nil in
	// production.
	ProviderCredentials *crossplaneaws.CredentialsSecret
	// ControllerServiceAccount is the controller's own service account in
	// the management cluster. Each tenant namespace binds it to
	// KubeconfigReaderRole, so the controller can get the kubeconfig
	// Secrets of that tenant's workload clusters and the Secrets of no
	// other namespace. Nil outside the cluster (local runs): no binding is
	// made.
	ControllerServiceAccount *ServiceAccount
	// Platform reads what the platform installs with every cluster: the
	// component catalog and the releases that pin its versions. Only what
	// reconciles has it; nil installs nothing (the API handlers, tests).
	Platform PlatformSource
	// CatalogOrder is the order a cluster applies the entries of a catalog
	// change in (definition names, or "platform-pool"): the ones named go
	// first, in this order, then the rest in the default order (removals
	// highest sequence first, then the others lowest sequence first).
	// PROP-03's placement moves supply theirs here. Nil: the default.
	CatalogOrder []string
	// Releases reads the release channels and the releases a cluster's
	// Kubernetes version is checked against (create and update) and
	// reported against (its status): the API handlers have it without
	// Platform. Nil: Platform is read; neither: nothing is checked
	// (tests).
	Releases ReleaseReader
	// Workloads reaches workload clusters, to recover a platform
	// component's Helm release that is stuck (RecoverStuckReleases). Nil:
	// nothing is recovered.
	Workloads Workloads
	// NodeGroupClients reaches workload clusters to apply their Karpenter
	// node groups (EC2NodeClasses and NodePools) and read their status.
	// Nil: no node group is applied or reported.
	NodeGroupClients WorkloadClients
	// SubTenantWorkloads opens provider-neutral workload clients using the parent
	// cluster's namespaced kubeconfig reference. Only the controller has this.
	SubTenantWorkloads SubTenantWorkloads
	// RecoverStuckReleases lets the cluster reconcile remove the Helm
	// record of a stuck revision in a workload cluster, so provider-helm
	// can install again.
	RecoverStuckReleases bool
	// HelmBackoffMax caps the pause between two attempts at a failed
	// Helm release (1m, 2m, 4m, ...); 0: helmrel.DefaultBackoffMax.
	// HelmMaxFailures is how many failures in a row that count (release
	// errors, not transient ones) make a component FAILED; 0:
	// helmrel.DefaultMaxFailures.
	HelmBackoffMax  time.Duration
	HelmMaxFailures int
	// Teardowns reaches workload clusters to delete, first in a cluster's
	// deletion, what their controllers made in the cloud (load
	// balancers, Karpenter's nodes). Nil: no teardown.
	Teardowns Teardowns
	// TeardownTimeout bounds that teardown; 0 is DefaultTeardownTimeout.
	TeardownTimeout time.Duration
	// NodeGroups is the platform admin's node group layout (the nodeGroups
	// section of the config file, the same for the server and the
	// controller): the system group's size and, for new clusters, its
	// instance type, capacity type and disk; the allowlist (which the API
	// server enforces on user groups) and the defaults of user groups. The
	// zero value: the system defaults and no restriction.
	NodeGroups config.NodeGroups
	// NaaS is the sub-tenant section of the config file (the same for the
	// server and the controller): the OIDC groups a sub-tenant may name,
	// which the API enforces and the controller checks again before it
	// opens access, and the qualifier's images. The zero value allows no
	// group and starts no qualifier Job.
	NaaS config.NaaS
	// NoRecommendations turns the sampling for resource recommendations
	// off (--recommendations=false).
	NoRecommendations bool
	// Events is where what happens to accounts, networks and clusters is
	// reported as Kubernetes events (events.go). Only what reconciles has
	// it; nil emits nothing.
	Events Events
	Logger *slog.Logger
}

// KubeconfigReaderRole is the ClusterRole (config/rbac) that may get
// Secrets. It is never bound cluster-wide: the account reconcile binds it
// to the controller in each tenant namespace, with a RoleBinding of the
// same name.
const KubeconfigReaderRole = "cluster-vending-kubeconfig-reader"

// LiveReaderServiceAccount is the live reader's service account (the
// `cluster-vending live` Deployment), in the controller's namespace. Each
// tenant namespace binds it to KubeconfigReaderRole too (PROP-02 7.1).
const LiveReaderServiceAccount = "cluster-vending-live"

// UsageHistoryWriterRole is the ClusterRole (config/rbac) that may write
// ConfigMaps: bound, like KubeconfigReaderRole, only in tenant namespaces,
// where the controller keeps each cluster's recorded usage (resource
// recommendations).
const UsageHistoryWriterRole = "cluster-vending-usage-history-writer"

// ServiceAccount names a service account.
type ServiceAccount struct{ Namespace, Name string }

// PlatformSource is where the cluster reconcile reads the component
// catalog and the releases from (the management cluster's
// ComponentDefinitions, Releases and ReleaseChannels).
type PlatformSource interface {
	// Definitions are the catalog's definitions.
	Definitions(ctx context.Context) ([]cloudv1alpha1.ComponentDefinition, error)
	ReleaseReader
}

// ReleaseReader reads the release channels and the releases: what a
// cluster's Kubernetes version is checked and reported against (the minor
// its channel offers).
type ReleaseReader interface {
	// Offered is the name of the release the channel (alpha, beta,
	// stable) offers now; "" when it offers none.
	Offered(ctx context.Context, channel string) (string, error)
	// Release reads a release; nil when there is none of that name.
	Release(ctx context.Context, name string) (*cloudv1alpha1.Release, error)
}

// HoldReader reads the release holds: the freezes
// (AdminService.Freeze). A ReleaseReader that
// is one too lets a cluster's status say its upgrades are paused.
type HoldReader interface {
	Holds(ctx context.Context) ([]cloudv1alpha1.ReleaseHold, error)
}

// MoveAdmitter says whether a cluster may start moving its components to
// a release now: the channel's rollout of release moves (tiers, batches,
// soaks, pauses) admitted it. A PlatformSource that is one gates release
// moves by it; one that is not admits every move.
type MoveAdmitter interface {
	// MoveAdmitted: the cluster (tenant, name) may start moving to release
	// on channel; why says what it waits for when it may not.
	MoveAdmitted(ctx context.Context, channel, release, tenantID, cluster string) (admitted bool, why string, err error)
}

// CodeAdmitter says whether a cluster or a network may apply its code
// change (a new controller build rendering its objects differently) now.
// A PlatformSource that is one gates code changes by it; one that is not
// admits every one (a freeze still holds them).
type CodeAdmitter interface {
	// ClusterCodeAdmitted: the channel's code rollout admitted the cluster
	// (tenant, name).
	ClusterCodeAdmitted(ctx context.Context, channel, tenantID, cluster string) (admitted bool, why string, err error)
	// NetworkCodeAdmitted: no freeze holds the network and the code rollout
	// of each channel of its clusters reached the network's tier.
	NetworkCodeAdmitted(ctx context.Context, tenantID, networkID string) (admitted bool, why string, err error)
}

// RenderBehind reports whether a resource last applied with renderer
// version must be applied again: it is below the target and the rollout
// covers its tenant or the resource itself.
func (d Deps) RenderBehind(version int64, tenantID, id string) bool {
	if version >= d.RenderTarget {
		return false
	}
	return len(d.RenderRollout) == 0 || slices.Contains(d.RenderRollout, tenantID) || slices.Contains(d.RenderRollout, id)
}
