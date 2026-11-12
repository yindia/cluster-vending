package cluster

import (
	"context"
	"fmt"
	"slices"
	"time"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/server/service"
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
func (s *ClusterService) eksActive(ctx context.Context, records []model.ManagedResource) (bool, error) {
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

// fillLive sets the plan's values that only exist once other resources do
// (crossplaneaws.ClusterPlan.FillLive), from the cluster's objects as the
// management cluster has them. An apply does not need it: the set reads
// the same values when it is applied (crossplaneaws.ClusterSet). It is
// for a plan of an update, which renders without applying.
func (s *ClusterService) fillLive(ctx context.Context, row *model.Cluster, plan *crossplaneaws.ClusterPlan) error {
	owner := service.Owner{TenantID: row.TenantID, Kind: model.OwnerCluster, ID: row.ID}
	seen, err := s.deps.Sequence().Observe(ctx, s.deps.Inventory(owner))
	if err != nil {
		return err
	}
	return plan.FillLive(ctx, seen)
}

// resumeAfter is how long a cluster row must have been left alone before
// an interrupted create is resumed: a CreateCluster still applying (or
// rolling back) must not be raced.
var resumeAfter = 2 * time.Minute

// stageAfter is how long after a cluster row was last written the loop
// moves a create on to its next stage. CreateCluster applies its first
// stage within a second or two; a pass that started in that window would
// apply alongside it, and after its rollback re-create what it removed.
var stageAfter = 15 * time.Second

// resumeCreate reports whether a cluster whose EKS cluster is not ACTIVE
// must be applied again: a new cluster still before its EKS cluster (what
// must exist first is being created), or a create that stopped half-way
// (the server went away between the applies): still in its first stage,
// no call touched its row for resumeAfter, and objects of that stage are
// not in the management cluster (none recorded yet, or recorded and
// missing).
func (s *ClusterService) resumeCreate(ctx context.Context, row *model.Cluster, records []model.ManagedResource) (bool, error) {
	if row.State != stateProvisioning || createdAfterActive(records) {
		return false, nil
	}
	if len(records) > 0 && !slices.ContainsFunc(records, func(r model.ManagedResource) bool { return r.Kind == "Cluster" }) {
		// The stage before the EKS cluster (its log group, its encryption
		// key): apply renders the cluster once they exist. Not while the
		// create that stored the row may still be applying or rolling back.
		return time.Since(row.UpdatedAt) >= stageAfter, nil
	}
	if time.Since(row.UpdatedAt) < resumeAfter {
		return false, nil
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	return len(records) == 0 || slices.ContainsFunc(views, func(v *cloudv1.ManagedResource) bool {
		return v.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING
	}), nil
}

// advance applies a cluster again until its generation is applied in
// full with the target renderer: after a failed apply, for the objects
// held back until EKS is ACTIVE (add-ons, access entries), during an
// upgrade for the node groups and add-ons once the control plane reports
// the target version (gateUpgrade), and when the renderer changed
// (Deps.RenderBehind). The background loop calls it.
//
// While EKS is not ACTIVE it only resumes an interrupted create
// (resumeCreate): the objects of the first stage are applied again, the
// later ones stay held back (crossplaneaws.ClusterSet). A cluster past its first
// stage (an update or upgrade in progress) waits for ACTIVE.
func (s *ClusterService) advance(ctx context.Context, row *model.Cluster) error {
	if row.State == stateDeleting ||
		(row.AppliedGeneration >= row.Generation && !s.deps.RenderBehind(row.RenderVersion, row.TenantID, row.ID)) {
		return nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return err
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
		s.logger().InfoContext(ctx, "resuming an interrupted cluster create", "cluster_id", row.ID, "recorded", len(records))
	}
	spec, err := specOf(row)
	if err != nil {
		return err
	}
	t, err := s.target(ctx, row.TenantID, spec)
	if err != nil {
		return err
	}
	defer s.recheck(ctx, row)
	if err := s.apply(ctx, row, t.account, s.renderPlan(spec, row, t)); err != nil {
		return err
	}
	s.logger().InfoContext(ctx, "cluster applied again", "cluster_id", row.ID, "generation", row.Generation,
		"applied_generation", row.AppliedGeneration, "render_version", row.RenderVersion)
	return nil
}
