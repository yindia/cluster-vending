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

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"

	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/deprecation"
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
	// AWSClusterAPI is how the Kubernetes API of a tenant's EKS cluster is
	// reached, as the tenant role, to scan the cluster for objects written
	// with deprecated API versions; nil: no cluster is scanned.
	AWSClusterAPI awscloud.ClusterAPIAccess
	// ScanCluster scans the cluster a config reaches; nil is
	// deprecation.ScanCluster (tests set it).
	ScanCluster func(context.Context, *rest.Config) (deprecation.Result, error)
	// ProviderCredentials, when set, is the Secret every account's
	// ProviderConfig authenticates the Crossplane AWS providers with,
	// instead of IRSA (a management cluster without IRSA, such as a local
	// kind cluster). The tenant role is assumed either way. Nil in
	// production.
	ProviderCredentials *crossplaneaws.CredentialsSecret
	Logger              *slog.Logger
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
