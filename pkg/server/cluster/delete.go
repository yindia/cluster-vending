package cluster

import (
	"context"
	"errors"
	"fmt"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// driveDelete moves a DELETING cluster on: its recorded objects are
// deleted all at once (a forced deletion first releases its Helm
// Releases, forceRelease); the Usages
// rendered with them (see crossplaneaws.ClusterResources) make the webhook
// hold each object until what uses it is gone, and replay its deletion
// then: node groups before the EKS cluster before its IAM. Once every
// recorded object is gone it deletes objects labelled with the cluster but
// never recorded, removes the network subnet tags and the cluster's rows.
// It reports whether the cluster is gone. Every step is idempotent: the
// background loop and DeleteCluster both call it.
func (s *ClusterService) driveDelete(ctx context.Context, row *model.Cluster) (bool, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, fmt.Errorf("list cluster resources: %w", err)
	}
	if row.DeleteForce {
		if err := s.forceRelease(ctx, row); err != nil {
			return false, err
		}
	}
	if err := s.deps.DeleteRecorded(ctx, records); err != nil {
		return false, err
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	if err := s.deps.MarkGone(ctx, records, views); err != nil {
		return false, err
	}
	if err := s.deps.ReleaseOrphanUsages(ctx, records, views); err != nil {
		return false, err
	}
	if !service.AllGone(views) {
		return false, nil
	}
	namespace := kube.TenantNamespace(row.TenantID)
	stray, err := s.deleteUnrecorded(ctx, row, namespace)
	if err != nil || stray > 0 {
		return false, err
	}
	if err := s.untagSubnets(ctx, row, namespace); err != nil {
		return false, fmt.Errorf("untag network subnets: %w", err)
	}
	// Records first: a failure after it is retried on the next pass (the
	// row is still there), never leaving records without a cluster.
	if err := s.deps.Store.ManagedResources().DeleteByOwner(ctx, model.OwnerCluster, row.ID); err != nil {
		return false, fmt.Errorf("forget the cluster's resources: %w", err)
	}
	if err := s.deps.Store.Clusters().Delete(ctx, row.TenantID, row.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return false, fmt.Errorf("remove the cluster row: %w", err)
	}
	s.logger().InfoContext(ctx, "cluster deleted", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name)
	return true, nil
}

// deleteUnrecorded confirms with the API server (by label, uncached) that
// nothing of the cluster is left, and reports how many objects it still
// found: the cluster is gone only once there are none. The cache saying
// "all gone" is not enough to forget cloud resources. Objects found that
// are not being deleted yet (never recorded: left by a failure between
// apply and record) are deleted; Usages last, once nothing else is left,
// so they protect until then.
func (s *ClusterService) deleteUnrecorded(ctx context.Context, row *model.Cluster, namespace string) (int, error) {
	found, err := s.sweep(ctx, row, namespace, crossplaneaws.ClusterLists())
	if err != nil || found > 0 {
		return found, err
	}
	return s.sweep(ctx, row, namespace, []client.ObjectList{&protectionv1beta1.UsageList{}})
}

func (s *ClusterService) sweep(ctx context.Context, row *model.Cluster, namespace string, lists []client.ObjectList) (int, error) {
	found := 0
	for _, list := range lists {
		err := s.deps.Reader().List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{crossplane.LabelCluster: row.ID})
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("list %T: %w", list, err)
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return 0, err
		}
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				continue
			}
			found++
			if obj.GetDeletionTimestamp() != nil {
				continue
			}
			err := s.deps.Kube.Delete(ctx, obj)
			if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return found, fmt.Errorf("delete %s %s: %w", kindOf(list), obj.GetName(), err)
			}
			s.logger().InfoContext(ctx, "deleting leftover cluster resource", "cluster_id", row.ID, "kind", kindOf(list), "name", obj.GetName())
		}
	}
	return found, nil
}

// kindOf is the item kind of a typed list, e.g. NodeGroupList -> NodeGroup.
func kindOf(list client.ObjectList) string {
	name := fmt.Sprintf("%T", list) // *v1beta1.NodeGroupList
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			name = name[i+1:]
			break
		}
	}
	return name[:len(name)-len("List")]
}
