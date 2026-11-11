package cluster

import (
	"context"
	"fmt"

	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/yindia/cluster-vending/pkg/database/model"
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
		if err := getIgnoreMissing(ctx, s.deps.Kube, client.ObjectKey{Namespace: r.Namespace, Name: r.Name}, c); err != nil {
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

// withoutAfterActive drops the afterActive objects.
func (s *ClusterService) withoutAfterActive(objects []client.Object) ([]client.Object, error) {
	out := make([]client.Object, 0, len(objects))
	for _, obj := range objects {
		gvk, err := apiutil.GVKForObject(obj, s.deps.Kube.Scheme())
		if err != nil {
			return nil, err
		}
		if !afterActive[gvk.Kind] {
			out = append(out, obj)
		}
	}
	return out, nil
}

// advance creates the held-back objects of a PROVISIONING cluster once its
// EKS cluster is ACTIVE. GetCluster and ListClusters call it, so reading a
// cluster (e.g. the CLI's --wait) moves its creation on.
func (s *ClusterService) advance(ctx context.Context, row *model.Cluster) error {
	if row.State != stateProvisioning && row.State != stateUpdating {
		return nil
	}
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil || createdAfterActive(records) || len(records) == 0 {
		return err
	}
	active, err := s.eksActive(ctx, records)
	if err != nil || !active {
		return err
	}
	spec, err := specOf(row)
	if err != nil {
		return err
	}
	t, err := s.target(ctx, row.TenantID, spec)
	if err != nil {
		return err
	}
	if err := s.apply(ctx, row, t.account, clusterPlan(spec, t.subnetIDs, s.base(row, t.account))); err != nil {
		return err
	}
	s.logger().InfoContext(ctx, "EKS cluster active: created add-ons and access entries", "cluster_id", row.ID)
	return nil
}

// progress moves a cluster's creation on (advance) when it is read. A
// failure is logged, not returned: the read still shows the cluster, and
// the next read retries.
func (s *ClusterService) progress(ctx context.Context, row *model.Cluster) {
	if err := s.advance(ctx, row); err != nil {
		s.logger().WarnContext(ctx, "continue creating cluster", "cluster_id", row.ID, "error", err)
	}
}
