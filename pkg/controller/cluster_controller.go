package controller

import (
	"context"
	"errors"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// ClusterReconciler reconciles Cluster objects by calling the cluster
// reconciler (pkg/reconcile/cluster) for the row each one is.
type ClusterReconciler struct {
	deps   Deps
	rec    *reconcilecluster.Reconciler
	owners *owners
}

// NewClusterReconciler returns the controller of Clusters.
func NewClusterReconciler(d Deps) *ClusterReconciler {
	return &ClusterReconciler{deps: d, rec: reconcilecluster.New(d.Deps), owners: &owners{}}
}

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=clusters/finalizers,verbs=update

// Reconcile moves the cluster one step on (see the package comment).
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := &cloudv1alpha1.Cluster{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	tenant, id := obj.Labels[cloudstore.LabelTenant], obj.Labels[cloudstore.LabelID]
	if id == "" {
		return r.adopt(ctx, obj)
	}
	r.owners.remember(id, obj)
	if !obj.DeletionTimestamp.IsZero() {
		if stop, err := r.deleting(ctx, tenant, id); stop || err != nil {
			return ctrl.Result{}, err
		}
	} else if err := ensureFinalizer(ctx, r.deps.Direct, obj, cloudv1alpha1.ClusterFinalizer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if edited(obj.Generation, obj.Status.ObservedGeneration) {
		if err := r.bump(ctx, tenant, id); err != nil {
			return ctrl.Result{}, err
		}
	}
	err := r.rec.Reconcile(ctx, tenant, id)
	if err != nil {
		r.deps.logger().ErrorContext(ctx, "reconcile cluster", "namespace", obj.Namespace, "name", obj.Name,
			"cluster_id", id, "error", err)
	}
	row, rerr := r.deps.Store.Clusters().Get(ctx, tenant, id)
	if errors.Is(rerr, repository.ErrNotFound) {
		r.owners.forget(id)
		return ctrl.Result{}, nil
	}
	if rerr != nil {
		return ctrl.Result{}, rerr
	}
	res := next(&row.Resource, reconcilecluster.PendingStates(), reconcilecluster.StateImported, err)
	if reconcilecluster.PlatformPending(row) && res.RequeueAfter > pendingDelay {
		// RUNNING, but its platform components are still being installed.
		res.RequeueAfter = pendingDelay
	}
	return res, nil
}

// bump makes the reconciler apply a spec that was edited on the object:
// the row's generation moves on, as an update through the API would have
// moved it. The edit is applied as written; of what the API's update
// checks (version steps, what a carried-over resource allows, resolving
// add-on versions) only what the kind's own validation rules say is
// checked.
func (r *ClusterReconciler) bump(ctx context.Context, tenant, id string) error {
	row, err := r.deps.Store.Clusters().Get(ctx, tenant, id)
	if err != nil {
		return fmt.Errorf("read cluster %s: %w", id, err)
	}
	if row.State == reconcilecluster.StateDeleting {
		return nil
	}
	row.Generation++
	if row.State == reconcilecluster.StateRunning {
		row.State = reconcilecluster.StateUpdating
	}
	reconcilecluster.ClearFailure(row)
	if err := r.deps.Store.Clusters().Update(ctx, row); err != nil && !errors.Is(err, repository.ErrConflict) {
		return fmt.Errorf("record the edit of cluster %s: %w", id, err)
	}
	return nil // a conflict: the row changed; the event of that change comes
}

// deleting handles an object someone asked to delete: the row is marked
// DELETING; the reconciler then deletes the cloud resources pass by pass
// and removes the row, and with it the object's finalizer. A cluster has
// no dependents of this API. stop is true when there is nothing to
// reconcile.
func (r *ClusterReconciler) deleting(ctx context.Context, tenant, id string) (bool, error) {
	row, err := r.deps.Store.Clusters().Get(ctx, tenant, id)
	if errors.Is(err, repository.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	if row.State == reconcilecluster.StateDeleting {
		return false, nil
	}
	row.State = reconcilecluster.StateDeleting
	if err := r.deps.Store.Clusters().Update(ctx, row); err != nil {
		return true, fmt.Errorf("mark cluster %s as deleting: %w", id, err)
	}
	return false, nil
}

// adopt makes a Cluster that was created without the API a row: the row
// the API's CreateCluster would have stored for the same cluster (the
// same checks and defaults, reconcilecluster.Reconciler.NewRow),
// written onto the object (cloudstore.AdoptCluster: the resolved spec
// too). What the API would have refused is a condition on the object,
// not an error.
func (r *ClusterReconciler) adopt(ctx context.Context, obj *cloudv1alpha1.Cluster) (ctrl.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // never a row: nothing of it exists in the cloud
	}
	if obj.Spec.ImportFrom != nil {
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reasonNotThroughAPI,
			"this cluster was created without the API and imports an existing EKS cluster: imports are made through the API (cluster import), which looks the cluster up first")
	}
	tenant, err := tenantOf(ctx, r.deps.Direct, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if tenant == "" {
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reasonInvalid, fmt.Sprintf("namespace %s is not a tenant's: bootstrap the tenant first", obj.Namespace))
	}
	network := &cloudv1alpha1.Network{}
	err = r.deps.Direct.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.Network.NetworkRef.Name}, network)
	if err != nil || network.Labels[cloudstore.LabelID] == "" {
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reasonWaiting, fmt.Sprintf("network %s does not exist in this namespace (yet)", obj.Spec.Network.NetworkRef.Name))
	}
	networkID := network.Labels[cloudstore.LabelID]
	row, err := r.rec.NewRow(ctx, &cloudv1.CreateClusterRequest{TenantId: tenant, Cluster: &cloudv1.Cluster{
		Name: obj.Name, DisplayName: obj.Spec.DisplayName, Labels: obj.Spec.Tags,
		Spec: convert.ClusterSpecFromAPI(obj.Spec, networkID, nil),
	}})
	if err != nil {
		reason, message := refusal(err)
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reason, message)
	}
	if err := cloudstore.AdoptCluster(ctx, r.deps.Store, obj, row); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return ctrl.Result{RequeueAfter: soon}, nil
		}
		reason, message := refusal(err)
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reason, message)
	}
	r.deps.logger().InfoContext(ctx, "cluster adopted", "namespace", obj.Namespace, "name", obj.Name, "cluster_id", row.ID)
	return ctrl.Result{RequeueAfter: soon}, nil
}

// SetupWithManager sets up the controller with the Manager: for Clusters,
// woken by their provider objects. They own nothing of this API.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.Cluster{}, forObject()).
		Named("cluster")
	b, err := providerWatches(b, mgr, ownerRequests(r.owners, r.deps.Direct, crossplane.LabelCluster,
		func() client.ObjectList { return &cloudv1alpha1.ClusterList{} }))
	if err != nil {
		return err
	}
	return b.Complete(r)
}
