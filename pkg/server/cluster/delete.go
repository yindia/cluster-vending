package cluster

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// deleteOrder deletes a cluster from the outside in: what runs on or
// attaches to the cluster, then the node groups (EKS drains them), then
// the EKS cluster, then the IAM roles it and its nodes used. Deleting a
// role first would leave draining nodes or the control plane without
// permissions.
var deleteOrder = service.DeleteOrder{
	{"AccessPolicyAssociation", "AccessEntry", "PodIdentityAssociation", "Addon", "ClusterAuth"},
	{"NodeGroup"},
	{"Cluster"},
	{"RolePolicyAttachment"},
	{"Role", "Policy"},
}

// driveDelete moves a DELETING cluster one step on: it deletes the next
// tier of its recorded objects (deleteOrder), then any object labelled with
// the cluster but never recorded, then removes the network subnet tags and
// the cluster's rows. It reports whether the cluster is gone. Every
// DeleteCluster, GetCluster and ListClusters of the cluster calls it.
func (s *ClusterService) driveDelete(ctx context.Context, row *model.Cluster) (bool, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, fmt.Errorf("list cluster resources: %w", err)
	}
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	left, err := s.deps.DeleteNext(ctx, records, views, deleteOrder)
	if err != nil || left > 0 {
		return false, err
	}
	namespace := kube.TenantNamespace(row.TenantID)
	stray, err := s.deleteUnrecorded(ctx, row, namespace, records)
	if err != nil || stray > 0 {
		return false, err
	}
	if err := s.untagSubnets(ctx, row, namespace); err != nil {
		return false, fmt.Errorf("untag network subnets: %w", err)
	}
	// The controller and an API read can both get here: gone is gone.
	if err := s.deps.Store.Clusters().Delete(ctx, row.TenantID, row.ID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		return false, fmt.Errorf("remove the cluster row: %w", err)
	}
	if err := s.deps.Store.ManagedResources().DeleteByOwner(ctx, model.OwnerCluster, row.ID); err != nil {
		return false, fmt.Errorf("forget the cluster's resources: %w", err)
	}
	s.logger().InfoContext(ctx, "cluster deleted", "tenant_id", row.TenantID, "cluster_id", row.ID, "name", row.Name)
	return true, nil
}

// deleteUnrecorded deletes objects labelled with the cluster that have no
// record (left by a failure between apply and record) and reports how many
// it found; the cluster is gone only once there are none.
func (s *ClusterService) deleteUnrecorded(ctx context.Context, row *model.Cluster, namespace string,
	records []model.ManagedResource,
) (int, error) {
	recorded := make(map[string]bool, len(records))
	for _, r := range records {
		recorded[r.Kind+"/"+r.Name] = true
	}
	found := 0
	for _, list := range crossplaneaws.ClusterLists() {
		err := s.deps.Kube.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{crossplane.LabelCluster: row.ID})
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
			kind := kindOf(list)
			if recorded[kind+"/"+obj.GetName()] {
				continue
			}
			found++
			if obj.GetDeletionTimestamp() == nil {
				if err := client.IgnoreNotFound(s.deps.Kube.Delete(ctx, obj)); err != nil {
					return found, fmt.Errorf("delete unrecorded %s %s: %w", kind, obj.GetName(), err)
				}
				s.logger().InfoContext(ctx, "deleting unrecorded cluster resource", "cluster_id", row.ID, "kind", kind, "name", obj.GetName())
			}
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
