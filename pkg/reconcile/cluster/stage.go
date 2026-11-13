package cluster

import (
	"context"
	"fmt"
	"slices"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// eksActiveStatus is the EKS cluster status once it can be used.
const eksActiveStatus = crossplaneaws.EKSActive

// createdAfterActive reports whether objects created only once the EKS
// cluster is ACTIVE (crossplaneaws.AfterActiveKinds) were created:
// once they were, they are always applied (an EKS cluster UPDATING during
// an upgrade must not prune its add-ons).
func createdAfterActive(records []model.ManagedResource) bool {
	for _, r := range records {
		if crossplaneaws.AfterActiveKinds[r.Kind] {
			return true
		}
	}
	return false
}

// eksActive reports whether the cluster's EKS cluster is ACTIVE.
func (s *Reconciler) eksActive(ctx context.Context, records []model.ManagedResource) (bool, error) {
	for _, r := range records {
		if r.Kind != "Cluster" {
			continue
		}
		c := &awseks.Cluster{}
		if err := s.getIgnoreMissing(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, c); err != nil {
			return false, fmt.Errorf("read EKS cluster %s: %w", r.Name, err)
		}
		return ptr.Deref(c.Status.AtProvider.Status, "") == eksActiveStatus, nil
	}
	return false, nil
}

// FillLive sets the plan's values that only exist once other resources do
// (crossplaneaws.ClusterPlan.FillLive), from the cluster's objects as the
// management cluster has them. An apply does not need it: the set reads
// the same values when it is applied (crossplaneaws.ClusterSet). It is
// for a plan of an update, which renders without applying.
func (s *Reconciler) FillLive(ctx context.Context, row *model.Cluster, plan *crossplaneaws.ClusterPlan) error {
	owner := reconcile.Owner{TenantID: row.TenantID, Kind: model.OwnerCluster, ID: row.ID}
	seen, err := s.deps.Sequence().Observe(ctx, s.deps.Inventory(owner))
	if err != nil {
		return err
	}
	return plan.FillLive(ctx, seen)
}

// resumeCreate reports whether a cluster whose EKS cluster is not ACTIVE
// must be applied: a new cluster still before its EKS cluster (nothing is
// recorded yet: calls only store the cluster; or what must exist first,
// its log group or encryption key, is being created), or a create that
// stopped half-way (the server went away between the applies): still in
// its first stage, and objects of that stage are recorded but not in the
// management cluster. A cluster whose first stage is all there waits for
// EKS.
func (s *Reconciler) resumeCreate(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (bool, error) {
	if row.State != StateProvisioning || createdAfterActive(records) {
		return false, nil
	}
	if !slices.ContainsFunc(records, func(r model.ManagedResource) bool { return r.Kind == "Cluster" }) {
		return true, nil
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(views, func(v *cloudv1.ManagedResource) bool {
		return v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
	}), nil
}

// MissingObject reports whether the row's recorded status names an object
// that is gone from the management cluster, on a cluster the platform
// applies (not imported, not being deleted). Such a cluster is looked at
// again soon: the pass that finds the object missing may be the one that
// creates it again (advance), or the object may come back.
func MissingObject(row *model.Cluster) bool {
	if row.State == StateDeleting || row.State == StateImported || row.State == StateBlocked {
		return false
	}
	for _, v := range storedStatus(row).GetManagedResources() {
		if v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING {
			return true
		}
	}
	return false
}

// advance applies a cluster until its generation is applied in full with
// the target renderer: once a call stored it, after a failed apply, for the objects
// held back until EKS is ACTIVE (add-ons, access entries), during an
// upgrade for the node groups and add-ons once the control plane reports
// the target version (gateUpgrade), and when the renderer changed
// (Deps.RenderBehind). The controller calls it.
//
// While EKS is not ACTIVE it only starts or resumes a create
// (resumeCreate): the objects of the first stage are applied again, the
// later ones stay held back (crossplaneaws.ClusterSet). A cluster past its first
// stage (an update or upgrade in progress) waits for ACTIVE.
//
// A cluster whose generation is applied is applied once more when
// recorded objects were deleted from the management cluster and may be
// created again (Deps.Repairable): the repair. Without it the cluster
// would report them missing until an unrelated update applied it. What
// the render no longer has (a node group being retired) is not created;
// an object being deleted is not missing; and nothing is repaired while
// one missing object must not be created blindly.
func (s *Reconciler) advance(ctx context.Context, row *model.Cluster) error {
	if row.State == StateDeleting {
		return nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return err
	}
	var repair []model.ManagedResource
	if row.AppliedGeneration >= row.Generation && !s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID) {
		if repair, err = s.deps.Repairable(ctx, records); err != nil || len(repair) == 0 {
			return err
		}
	}
	active, err := s.eksActive(ctx, records)
	if err != nil {
		return err
	}
	if !active {
		resume, err := s.resumeCreate(ctx, row, records)
		if err != nil || !resume {
			return err
		}
		s.logger().InfoContext(ctx, "applying a cluster before its EKS cluster is active", "cluster_id", row.ID, "recorded", len(records))
	}
	spec, err := SpecOf(row)
	if err != nil {
		return err
	}
	t, err := s.Target(ctx, row.TenantID, spec)
	if err != nil {
		return err
	}
	// Before the apply, which marks the generation applied: a failure here
	// is tried again. Idempotent; it also tags the subnets of clusters
	// created before the platform tagged them.
	if err := s.TagSubnets(ctx, row, t); err != nil {
		return err
	}
	if err := s.Apply(ctx, row, t.Account, s.RenderPlan(spec, row, t)); err != nil {
		return err
	}
	if len(repair) > 0 {
		s.deps.LogRepaired(ctx, reconcile.Owner{TenantID: row.TenantID, Kind: model.OwnerCluster, ID: row.ID}, repair)
		return nil
	}
	s.logger().InfoContext(ctx, "cluster applied again", "cluster_id", row.ID, "generation", row.Generation,
		"applied_generation", row.AppliedGeneration, "render_version", row.RenderVersion)
	return nil
}
