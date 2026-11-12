package cluster

import (
	"context"
	"fmt"
	"slices"
	"time"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// afterActive are the kinds created only once the EKS cluster is ACTIVE.
// Their references resolve as soon as the cluster has a name (set at
// create), so applied together with it they would fail for the 10+
// minutes EKS takes and turn the cluster FAILED. Node groups and the
// ClusterAuth wait for ACTIVE by themselves.
var afterActive = map[string]bool{
	"Addon": true, "PodIdentityAssociation": true, "AccessEntry": true, "AccessPolicyAssociation": true,
}

// eksActiveStatus is the EKS cluster status once it can be used.
const eksActiveStatus = "ACTIVE"

// createdAfterActive reports whether the afterActive objects were created:
// once they were, they are always applied (an EKS cluster UPDATING during
// an upgrade must not prune its add-ons).
func createdAfterActive(records []model.ManagedResource) bool {
	for _, r := range records {
		if afterActive[r.Kind] {
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

// holdBack reports whether apply must leave out the afterActive objects:
// they were never created and the EKS cluster is not ACTIVE yet.
func (s *ClusterService) holdBack(ctx context.Context, row *model.Cluster) (bool, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, err
	}
	if createdAfterActive(records) {
		return false, nil
	}
	active, err := s.eksActive(ctx, records)
	return !active, err
}

// withoutAfterActive drops the afterActive objects, and the Usages whose
// "by" object is one of them: a Usage whose user never exists is never
// garbage-collected and would block its object's deletion for good.
func (s *ClusterService) withoutAfterActive(objects []client.Object) ([]client.Object, error) {
	out := make([]client.Object, 0, len(objects))
	kept := map[string]bool{}
	for _, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, err
		}
		if afterActive[gvk.Kind] {
			continue
		}
		if u, ok := obj.(*protectionv1beta1.Usage); ok {
			if by := u.Spec.By; by == nil || by.ResourceRef == nil || !kept[by.Kind+"/"+by.ResourceRef.Name] {
				continue
			}
		}
		kept[gvk.Kind+"/"+obj.GetName()] = true
		out = append(out, obj)
	}
	return out, nil
}

// resumeAfter is how long a cluster row must have been left alone before
// an interrupted create is resumed: a CreateCluster still applying (or
// rolling back) must not be raced.
var resumeAfter = 2 * time.Minute

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
		// key): apply renders the cluster once they exist.
		return true, nil
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
// later ones stay held back (apply, holdBack). A cluster past its first
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
	if err := s.apply(ctx, row, t.account, s.renderPlan(spec, row, t)); err != nil {
		return err
	}
	s.logger().InfoContext(ctx, "cluster applied again", "cluster_id", row.ID, "generation", row.Generation,
		"applied_generation", row.AppliedGeneration, "render_version", row.RenderVersion)
	return nil
}
