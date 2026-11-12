// Package cluster moves an AWS (EKS) cluster towards its stored spec: it
// applies the cluster's Crossplane managed resources in order, stages what
// must wait, replaces node groups, deletes, and reads the cluster's status
// from its resources. The stored row is the source of truth: the API
// handlers (pkg/server/cluster) only store what was asked for and read;
// the background loop calls Reconcile, which applies it and records what
// goes wrong as the cluster's status. It knows no RPC library: failures a
// caller can act on are faults (pkg/fault).
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Lifecycle states stored on the cluster row; the others are observed.
const (
	StateProvisioning = "CLUSTER_STATE_PROVISIONING"
	StateRunning      = "CLUSTER_STATE_RUNNING"
	StateUpdating     = "CLUSTER_STATE_UPDATING"
	StateDeleting     = "CLUSTER_STATE_DELETING"
)

// Reconciler moves clusters towards their spec.
type Reconciler struct{ deps reconcile.Deps }

// New returns the Reconciler.
func New(d reconcile.Deps) *Reconciler { return &Reconciler{deps: d} }

// Target is everything a cluster is built from: its network, account and
// the network's private subnets in the cluster's zones.
type Target struct {
	Network   *model.Network
	Account   *model.CloudAccount
	Zones     []string
	SubnetIDs []string
	// SubnetObjects are the Subnet managed resources of SubnetIDs.
	SubnetObjects []string
}

// Target loads the cluster's network and account and picks its subnets.
func (s *Reconciler) Target(ctx context.Context, tenantID string, spec *cloudv1.ClusterSpec) (Target, error) {
	n, err := s.deps.Store.Networks().Get(ctx, tenantID, spec.GetNetwork().GetNetworkId())
	if errors.Is(err, repository.ErrNotFound) {
		return Target{}, fault.New(fault.FailedPrecondition,
			fmt.Sprintf("tenant %s has no network %s", tenantID, spec.GetNetwork().GetNetworkId()))
	}
	if err != nil {
		return Target{}, storeError(err)
	}
	switch {
	case n.State == "NETWORK_STATE_DELETING", n.State == "NETWORK_STATE_IMPORTED", n.State == "NETWORK_STATE_FAILED":
		return Target{}, fault.New(fault.FailedPrecondition,
			fmt.Sprintf("network %s is %s; clusters need a network the platform owns", n.ID, n.State))
	case n.Provider != model.ProviderAWS || n.Region != spec.GetRegion():
		return Target{}, fault.New(fault.Invalid,
			fmt.Sprintf("network %s is %s/%s; the cluster is aws/%s", n.ID, n.Provider, n.Region, spec.GetRegion()))
	}
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, tenantID, n.CloudAccountID)
	if err != nil {
		return Target{}, storeError(err)
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerNetwork, n.ID)
	if err != nil {
		return Target{}, storeError(err)
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return Target{}, err
	}
	subnets := corecluster.PrivateSubnets(views)
	zones, ids, err := corecluster.ChooseSubnets(subnets, spec.GetNetwork().GetZoneIds())
	if err != nil {
		return Target{}, fault.New(fault.FailedPrecondition, fmt.Sprintf("network %s: %v", n.ID, err))
	}
	return Target{Network: n, Account: account, Zones: zones, SubnetIDs: ids, SubnetObjects: corecluster.SubnetObjectNames(subnets, ids)}, nil
}

// RenderPlan is the plan of the cluster's resources: the resolved spec on
// the network's subnets.
func (s *Reconciler) RenderPlan(spec *cloudv1.ClusterSpec, row *model.Cluster, t Target) crossplaneaws.ClusterPlan {
	p := corecluster.RenderPlan(spec, t.SubnetIDs, s.Base(row, t.Account))
	p.SubnetObjects = t.SubnetObjects
	return p
}

func (s *Reconciler) Base(row *model.Cluster, a *model.CloudAccount) crossplaneaws.ClusterPlan {
	return crossplaneaws.ClusterPlan{
		Namespace: a.Namespace, ProviderConfigName: a.ProviderConfigName, TenantID: row.TenantID,
		ClusterID: row.ID, ClusterName: row.Name, Prefix: crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name),
		Tags: row.Labels, Adoption: Adoption(row),
	}
}

// Apply makes the cluster's managed resources match plan, in order
// (crossplaneaws.ClusterSet, sequence.Engine): what the cluster has is
// recorded before it is applied, so nothing is ever in the management
// cluster without the cluster knowing of it; add-ons and access entries are held back until EKS is ACTIVE;
// recorded objects the plan no longer has (e.g. a removed admin) are
// removed once nothing is held back. The generation is marked applied
// once the whole set is; until then the cluster is applied again
// (advance).
func (s *Reconciler) Apply(ctx context.Context, row *model.Cluster, a *model.CloudAccount, plan crossplaneaws.ClusterPlan) error {
	name, retiring, err := s.NodeGroupName(ctx, row, plan)
	if err != nil {
		s.logger().ErrorContext(ctx, "choose node group", "cluster_id", row.ID, "error", err)
		return fault.New(fault.Unavailable, "cannot read the cluster's node group")
	}
	plan.NodeGroupName = name
	upgrading, err := s.GateUpgrade(ctx, row, &plan)
	if err != nil {
		s.logger().ErrorContext(ctx, "read EKS cluster version", "cluster_id", row.ID, "error", err)
		return fault.New(fault.Unavailable, "cannot read the cluster's EKS cluster")
	}
	keep, err := s.kept(retiring)
	if err != nil {
		return fault.New(fault.Internal, err.Error())
	}
	if err := s.ownCarriedOver(ctx, row); err != nil {
		s.logger().ErrorContext(ctx, "take over carried-over resources", "cluster_id", row.ID, "error", err)
		return fault.New(fault.Internal, "could not take over the cluster's carried-over resources")
	}
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: a.ID, Kind: model.OwnerCluster, ID: row.ID}
	res, err := s.deps.Sequence().Apply(ctx, crossplaneaws.ClusterSet(plan), sequence.Keeping(s.deps.Inventory(owner), keep))
	if err != nil {
		s.logger().ErrorContext(ctx, "apply cluster resources", "cluster_id", row.ID, "error", err)
		return applyError(err)
	}
	if !res.Complete() || upgrading {
		// Held back (for EKS to be ACTIVE, or for a value of another
		// resource: nothing is removed meanwhile) or upgrading: applied
		// again (advance) until complete.
		return nil
	}
	return s.markApplied(ctx, row)
}

// kept selects the recorded objects that stay although the set applied
// now does not have them: the node groups being retired, with their launch
// templates.
func (s *Reconciler) kept(retiring []client.Object) (func(sequence.Entry) bool, error) {
	refs := make(map[managed.Ref]bool, len(retiring))
	for _, obj := range retiring {
		ref, err := managed.RefOf(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, err
		}
		refs[ref] = true
	}
	return func(e sequence.Entry) bool {
		return refs[managed.Ref{APIVersion: e.APIVersion, Kind: e.Kind, Namespace: e.Namespace, Name: e.Name}]
	}, nil
}

// markApplied records that every object of the row's generation was
// applied without error, with this build's renderer.
func (s *Reconciler) markApplied(ctx context.Context, row *model.Cluster) error {
	if row.AppliedGeneration == row.Generation && row.RenderVersion == crossplaneaws.RenderVersion {
		return nil
	}
	before, version := row.AppliedGeneration, row.RenderVersion
	row.AppliedGeneration, row.RenderVersion = row.Generation, crossplaneaws.RenderVersion
	err := s.deps.Store.Clusters().Update(ctx, row)
	if errors.Is(err, repository.ErrConflict) {
		// A call changed the row while it was applied: it is behind again
		// (calls only store), and the next pass applies it as it is now.
		row.AppliedGeneration, row.RenderVersion = before, version
		return nil
	}
	if err != nil {
		return fault.New(fault.Internal, "could not record the applied generation: "+err.Error())
	}
	return nil
}

func applyError(err error) error {
	if meta.IsNoMatchError(err) {
		return fault.New(fault.FailedPrecondition,
			"a Crossplane AWS provider the cluster needs (eks, iam, ec2, kms, cloudwatchlogs) is not installed in the management cluster")
	}
	return fault.New(fault.Internal, "could not create the cluster's resources")
}

func (s *Reconciler) logger() *slog.Logger {
	if s.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.deps.Logger
}

// storeError is a store failure as a fault a caller can answer with: the
// repository's errors keep their meaning, anything else is INTERNAL
// without its text (it may hold database details). A fault passes as it
// is, and so does a cancelled or expired context.
func storeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, repository.ErrNotFound):
		return fault.New(fault.NotFound, err.Error())
	case errors.Is(err, repository.ErrAlreadyExists):
		return fault.New(fault.AlreadyExists, err.Error())
	case errors.Is(err, repository.ErrConflict):
		return fault.New(fault.Aborted, err.Error())
	case errors.Is(err, repository.ErrInUse):
		return fault.New(fault.FailedPrecondition, err.Error())
	case errors.Is(err, repository.ErrInvalid):
		return fault.New(fault.Invalid, err.Error())
	}
	if _, ok := fault.As(err); ok {
		return err
	}
	return fault.New(fault.Internal, "internal error")
}
