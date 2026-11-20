package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/fault"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/results"
)

// driveDelete moves a DELETING cluster on, in the reverse of its creation
// (delete_stage.go): the workload cluster's load balancers; the tenant's
// components; the platform's components above sequence 0, highest sequence
// first; Karpenter's node groups; Karpenter; the EKS add-ons; then the
// rest, which the Usages rendered with the objects order (see
// crossplaneaws.ClusterResources: the webhook holds each object until what
// uses it is gone and replays its deletion then): the default node group,
// the EKS cluster, its IAM, the results bucket. A stage starts only when
// the one before is gone, and the cluster's status says which it waits in.
// A forced deletion first releases its Helm Releases (forceRelease), and
// so does one whose EKS cluster is not ACTIVE: nothing is uninstalled from
// a cluster that cannot be reached. Once every recorded object is gone it
// deletes objects labelled with the cluster but never recorded, removes the
// network subnet tags and the cluster's rows. It reports whether the
// cluster is gone. Every step is idempotent: the controller calls it;
// DeleteCluster only stores the deletion.
func (s *Reconciler) driveDelete(ctx context.Context, row *model.Cluster) (bool, error) {
	records, err := s.deps.Store.ManagedResources().ListByOwner(ctx, model.OwnerCluster, row.ID)
	if err != nil {
		return false, fmt.Errorf("list cluster resources: %w", err)
	}
	// 1. While the controllers that made them still run: the load
	// balancers the workload cluster has in the cloud, which nothing else
	// would delete.
	if done, err := s.teardown(ctx, row, records); err != nil || !done {
		return false, err
	}
	active, err := s.eksActive(ctx, records)
	if err != nil {
		return false, err
	}
	if row.DeleteForce || !active {
		if err := s.forceRelease(ctx, row); err != nil {
			return false, err
		}
	}
	// 2. The tenant's components, a tenant component may require a
	// platform one.
	if gone, err := s.deleteTenant(ctx, row); err != nil || !gone {
		return false, err
	}
	// 3. The platform's components above Karpenter.
	plat, err := s.loadPlatformRemoval(ctx, row)
	if err != nil {
		return false, err
	}
	recordPlatformDeletion(ctx, row, len(plat.components) == 0)
	if gone, err := s.removeAbove(ctx, row, plat); err != nil || !gone {
		return false, err
	}
	// 4. Karpenter's node groups, while Karpenter still runs to terminate
	// their nodes.
	cl, err := s.loadClusterRemoval(ctx, row, records)
	if err != nil {
		return false, err
	}
	reachable := !row.DeleteForce && active && storedStatus(row).GetKubeconfigSecret() != ""
	if !s.deleteNodeGroups(ctx, row, cl, reachable) {
		return false, nil
	}
	// 5. Karpenter: its chart and IAM, then its queue, rules and node role.
	if gone, err := s.removeKarpenterChart(ctx, row, plat); err != nil || !gone {
		return false, err
	}
	if gone, err := s.removeStage(ctx, cl, crossplaneaws.DeleteStageKarpenter, stageKarpenter,
		"removing its interruption queue, rules and targets, node role and access entry"); err != nil || !gone {
		return false, err
	}
	// 6. The EKS add-ons with their pod identity associations and roles.
	if gone, err := s.removeStage(ctx, cl, crossplaneaws.DeleteStageAddons, stageAddons,
		"removing the add-ons, their pod identity associations and roles"); err != nil || !gone {
		return false, err
	}
	// 7. The rest.
	// The results bucket is emptied before its Bucket resource is deleted
	// (AWS refuses a bucket with objects and the platform never
	// force-destroys one): every pass until the Bucket is gone, so a scan that
	// wrote late is emptied too.
	if err := s.emptyResultsBucket(ctx, row); err != nil {
		return false, err
	}
	owner := reconcile.Owner{TenantID: row.TenantID, CloudAccountID: row.CloudAccountID, Kind: model.OwnerCluster, ID: row.ID}
	if _, err := s.deps.Sequence().Delete(ctx, s.deps.Inventory(owner), crossplaneaws.ClusterDeleteOrder); err != nil {
		return false, deleteError(err)
	}
	// As they are after the step: what it deleted may be gone already.
	views, err := s.deps.Observe(ctx, records)
	if err != nil {
		return false, err
	}
	if err := s.deps.MarkGone(ctx, records, views); err != nil {
		return false, err
	}
	// Again, with what the step left: a Usage whose user went away in this
	// very step is released now, not a pass later.
	if err := s.deps.ReleaseOrphanUsages(ctx, records, views); err != nil {
		return false, err
	}
	if !reconcile.AllGone(views) {
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
	if err := s.deps.Store.ManagedResources().DeleteByOwner(ctx, model.OwnerClusterComponents, row.ID); err != nil {
		return false, fmt.Errorf("forget the platform components' resources: %w", err)
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

// deleteError maps an object the deletion could not read to UNAVAILABLE;
// other errors pass as they are.
func deleteError(err error) error {
	var unreadable *managed.ReadError
	if errors.As(err, &unreadable) {
		return fault.New(fault.Unavailable,
			fmt.Sprintf("cannot read %s %s from the management cluster", unreadable.Ref.Kind, unreadable.Ref.Name))
	}
	return err
}

// deleteUnrecorded confirms with the API server (by label, uncached) that
// nothing of the cluster is left, and reports how many objects it still
// found: the cluster is gone only once there are none. The cache saying
// "all gone" is not enough to forget cloud resources. Objects found that
// are not being deleted yet (never recorded: left by a failure between
// apply and record) are deleted; Usages last, once nothing else is left,
// so they protect until then.
func (s *Reconciler) deleteUnrecorded(ctx context.Context, row *model.Cluster, namespace string) (int, error) {
	found, err := s.sweep(ctx, row, namespace, crossplaneaws.ClusterLists())
	if err != nil || found > 0 {
		return found, err
	}
	return s.sweep(ctx, row, namespace, []client.ObjectList{&protectionv1beta1.UsageList{}})
}

func (s *Reconciler) sweep(ctx context.Context, row *model.Cluster, namespace string, lists []client.ObjectList) (int, error) {
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

// emptyResultsBucketTimeout bounds emptying a bucket in one pass (listing
// and deleting pages of 1,000 keys); what is left is the next pass's.
const emptyResultsBucketTimeout = time.Minute

// emptyResultsBucket deletes the objects of the cluster's results bucket
// (results.EmptyBucket, with the tenant role of its account) while the
// Bucket managed resource exists. A failure is the cluster's status
// (UNAVAILABLE, with the reason) and the pass is tried again; nothing of the
// Bucket is deleted before it succeeds. A bucket already gone is empty.
// Without S3 clients (Deps.ResultsS3 nil: tests) it does nothing.
func (s *Reconciler) emptyResultsBucket(ctx context.Context, row *model.Cluster) error {
	if s.deps.ResultsS3 == nil {
		return nil
	}
	prefix := crossplaneaws.ClusterPrefix(row.TenantID, row.ID, row.Name)
	account, err := s.deps.Store.Tenants().GetCloudAccountByID(ctx, row.TenantID, row.CloudAccountID)
	if err != nil {
		return fmt.Errorf("read the cluster's account: %w", storeError(err))
	}
	err = s.deps.Reader().Get(ctx, client.ObjectKey{Namespace: account.Namespace, Name: crossplaneaws.ResultsBucketObjectName(prefix)}, &awss3.Bucket{})
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil // no Bucket (an imported cluster's, or already deleted): nothing to empty
	}
	if err != nil {
		return fmt.Errorf("read the results Bucket: %w", err)
	}
	bucket := crossplaneaws.ResultsBucketName(prefix, account.AccountID, row.Region, row.ID)
	fail := func(why string, cause error) error {
		s.logger().WarnContext(ctx, "results bucket not emptied", "cluster_id", row.ID, "bucket", bucket, "error", cause)
		return fault.New(fault.Unavailable, fmt.Sprintf("deleting; cannot empty the results bucket %s yet (%s); retrying", bucket, why))
	}
	access, err := s.deps.AWSAccess(ctx, account)
	if err != nil {
		return fail("the account cannot be reached", err)
	}
	client, err := s.deps.ResultsS3(access, row.Region)
	if err != nil {
		return fail("the account cannot be reached", err)
	}
	ctx, cancel := context.WithTimeout(ctx, emptyResultsBucketTimeout)
	defer cancel()
	n, err := results.EmptyBucket(ctx, client, bucket)
	if err != nil {
		return fail(results.Notice(err), err)
	}
	if n > 0 {
		s.logger().InfoContext(ctx, "results bucket emptied", "cluster_id", row.ID, "bucket", bucket, "objects", n)
	}
	return nil
}
