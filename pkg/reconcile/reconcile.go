// Package reconcile is what moves networks and clusters towards their
// stored spec: the dependencies that work needs (the store, the
// management cluster, how tenant cloud accounts are reached) and what
// every kind of owner shares (recording, observing and deleting its
// objects). It knows no RPC library: failures a caller can act on are
// faults (pkg/fault). The API handlers and the background loop both call
// it.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
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
	AWS        config.AWS
	// AWSDiscovery makes the server's own read-only AWS lookups
	// (ImportNetwork).
	// RenderTarget is the renderer version resources are brought to by the
	// background loop (0: none are re-applied for it). RenderRollout limits
	// that to these tenant, network or cluster IDs; empty means all.
	RenderTarget  int64
	RenderRollout []string
	AWSDiscovery  awscloud.NetworkDiscoverer
	// AWSClusterDiscovery looks an existing EKS cluster up (ImportCluster);
	// nil: imports are unavailable.
	AWSClusterDiscovery awscloud.ClusterDiscoverer
	// AWSClusters checks EKS cluster names before CreateCluster; nil skips
	// the check (tests).
	AWSClusters awscloud.ClusterLookup
	Logger      *slog.Logger
}

// AWSTenantRoleARN returns the IAM role the platform assumes in one of a
// tenant's bootstrapped AWS accounts, by bootstrap ID (CloudAccount.id, as
// in NetworkSpec.bootstrap_id): the role stored at bootstrap.
func (d Deps) AWSTenantRoleARN(ctx context.Context, tenantID, cloudAccountID string) (string, error) {
	account, err := d.Store.Tenants().GetCloudAccountByID(ctx, tenantID, cloudAccountID)
	if err != nil {
		return "", fmt.Errorf("account %s of tenant %s: %w", cloudAccountID, tenantID, err)
	}
	if account.Provider != model.ProviderAWS {
		return "", fault.New(fault.Invalid, fmt.Sprintf("account %s is a %s account, not aws", cloudAccountID, account.Provider))
	}
	if account.Identity == "" {
		return "", fault.New(fault.FailedPrecondition, fmt.Sprintf("account %s has no tenant role yet; bootstrap it again", cloudAccountID))
	}
	return account.Identity, nil
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
