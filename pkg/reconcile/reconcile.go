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

	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"

	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// Deps are what reconciling works with: the database (the source of
// truth), the management cluster where Crossplane managed resources are
// created, and how tenant cloud accounts are reached.
type Deps struct {
	Store repository.Store
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
	// RecoverStuckReleases lets the cluster reconcile remove the Helm
	// record of a stuck revision in a workload cluster, so provider-helm
	// can install again.
	RecoverStuckReleases bool
	// Teardowns reaches workload clusters to delete, first in a cluster's
	// deletion, what their controllers made in the cloud (load
	// balancers, Karpenter's nodes). Nil: no teardown.
	Teardowns Teardowns
	// TeardownTimeout bounds that teardown; 0 is DefaultTeardownTimeout.
	TeardownTimeout time.Duration
	// ComponentUpgrades: a cluster moves its components to the release its
	// channel offers, one at a time (release_move.go); off, it keeps the
	// release its components were installed from.
	ComponentUpgrades bool
	// ComponentUpgradeTimeout is how long one component of a move may take
	// to settle before the move halts; 0 is DefaultComponentUpgradeTimeout.
	ComponentUpgradeTimeout time.Duration
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

// HoldReader reads the release holds: the pauses of upgrades
// (AdminService.PauseReconcile with only_upgrades). A ReleaseReader that
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

// RenderBehind reports whether a resource last applied with renderer
// version must be applied again: it is below the target and the rollout
// covers its tenant or the resource itself.
func (d Deps) RenderBehind(version int64, tenantID, id string) bool {
	if version >= d.RenderTarget {
		return false
	}
	return len(d.RenderRollout) == 0 || slices.Contains(d.RenderRollout, tenantID) || slices.Contains(d.RenderRollout, id)
}
