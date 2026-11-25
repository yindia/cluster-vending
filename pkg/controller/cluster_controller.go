package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	accountreconcile "github.com/yindia/cluster-vending/pkg/reconcile/account"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// ConditionEditRefused was True on a Cluster whose edit (made without the
// API) named node groups outside the admin's allowlist. Tenants define no
// node groups now: a condition an earlier build set is removed.
const ConditionEditRefused = "EditRefused"

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
	leave := r.deps.FreezeBarrier.Enter()
	defer leave()
	obj := &cloudv1alpha1.Cluster{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	tenant, id := obj.Labels[cloudstore.LabelTenant], obj.Labels[cloudstore.LabelID]
	frozen, err := r.deps.SyncFreeze(ctx, platform.PauseScope{TenantID: tenant, ClusterID: id})
	if err != nil {
		return ctrl.Result{}, err
	}
	if result, stop, err := r.guardSubTenantSafety(ctx, obj, frozen); stop || err != nil {
		return result, err
	}
	if frozen.Holds() {
		if id != "" {
			if err := r.rec.Reconcile(ctx, tenant, id); err != nil {
				return ctrl.Result{}, err
			}
		}
		return untilFreezeEnds(frozen, ctrl.Result{RequeueAfter: pendingDelay}, time.Now()), nil
	}
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
		if err := r.bump(ctx, obj, tenant, id); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.clearRefusal(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	err = r.rec.Reconcile(ctx, tenant, id)
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
	return nextCluster(row, err), nil
}

// nextCluster decides when a cluster is looked at again: as next does,
// and sooner while its platform components are being installed or one of
// its recorded objects is missing, although it is RUNNING.
func nextCluster(row *model.Cluster, err error) ctrl.Result {
	res := next(&row.Resource, reconcilecluster.PendingStates(), reconcilecluster.StateImported, err)
	if reconcilecluster.PlatformPending(row) && res.RequeueAfter > pendingDelay {
		// RUNNING, but its platform components are still being installed.
		res.RequeueAfter = pendingDelay
	}
	if reconcilecluster.PlatformStuck(row) && res.RequeueAfter > failedDelay {
		// A platform component's Helm release is stuck: it is recovered
		// once it has been stuck long enough, and no event says when.
		res.RequeueAfter = failedDelay
	}
	if at, ok := reconcilecluster.PlatformRetryAt(row); ok {
		// A failed Helm release is paused for its backoff: looked at again
		// when it ends, so it is lifted on time and not at the next poll.
		if wait := max(time.Until(at), retryFloor); res.RequeueAfter > wait {
			res.RequeueAfter = wait
		}
	}
	if reconcilecluster.Upgrading(row) && res.RequeueAfter > upgradeDelay {
		// An upgrade moves step by step, each read as AWS and Helm report
		// it: looked at again soon, not at the providers' poll alone.
		res.RequeueAfter = upgradeDelay
	}
	if reconcilecluster.CodeChanging(row) && res.RequeueAfter > codeDelay {
		// A code change waits for its turn in the channel's rollout, which
		// admits it without waking the cluster.
		res.RequeueAfter = codeDelay
	}
	if reconcilecluster.MissingObject(row) && res.RequeueAfter > failedDelay {
		// An object was deleted behind the platform's back: the pass that
		// found it missing may have created it again, and the next one says
		// so; one that cannot be created again may come back.
		res.RequeueAfter = failedDelay
	}
	return res
}

// bump makes the reconciler apply a spec that was edited on the object:
// the row's generation moves on, as an update through the API would have
// moved it, and its input follows the edit as an API update's does (the
// add-ons the client pinned). The edit is applied as written; of what the API's update
// checks (version steps, what a carried-over resource allows, resolving
// add-on versions) only what the kind's own validation rules say is
// checked.
//
// A generation the store itself produced (an API update, whose status
// write has not come yet) is no edit: the row took it up, and only its
// acknowledgement is made good.
func (r *ClusterReconciler) bump(ctx context.Context, obj *cloudv1alpha1.Cluster, tenant, id string) error {
	row, err := r.deps.Store.Clusters().Get(ctx, tenant, id)
	if err != nil {
		return fmt.Errorf("read cluster %s: %w", id, err)
	}
	if row.State == reconcilecluster.StateDeleting {
		return nil
	}
	if row.Imported && (row.State == reconcilecluster.StateImported || row.State == reconcilecluster.StateBlocked) {
		// Imported and not taken over: read-only. The edit is not a new
		// generation; the reconciler puts the discovered spec back and
		// reports it (keepDiscovered).
		return nil
	}
	if cloudstore.StoreWrote(obj) {
		// The row already is this generation (and its etag the API
		// caller's): a second bump would rotate the etag and force a full
		// re-apply. A write of the row acknowledges the generation.
		if err := r.deps.Store.Clusters().Record(ctx, row); err != nil && !errors.Is(err, repository.ErrConflict) {
			return fmt.Errorf("acknowledge the update of cluster %s: %w", id, err)
		}
		return nil
	}
	installed := map[string]string{}
	if obj.Status.AWS != nil {
		for _, a := range obj.Status.AWS.Addons {
			installed[a.Name] = a.Version
		}
	}
	if row.Input, err = reconcilecluster.EditedInput(row, installed); err != nil {
		return fmt.Errorf("rebuild the input of cluster %s: %w", id, err)
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

// clearRefusal removes the refusal an earlier build set on the object.
func (r *ClusterReconciler) clearRefusal(ctx context.Context, obj *cloudv1alpha1.Cluster) error {
	if meta.FindStatusCondition(obj.Status.Conditions, ConditionEditRefused) == nil {
		return nil
	}
	fresh := &cloudv1alpha1.Cluster{}
	if err := r.deps.Direct.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !meta.RemoveStatusCondition(&fresh.Status.Conditions, ConditionEditRefused) {
		return nil
	}
	return client.IgnoreNotFound(r.deps.Direct.Status().Update(ctx, fresh))
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
	tenant, err := tenantOf(ctx, r.deps, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if tenant == "" {
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reasonInvalid, fmt.Sprintf("namespace %s is not a tenant's: bootstrap the tenant first", obj.Namespace))
	}
	network := &cloudv1alpha1.Network{}
	err = r.deps.getAdopted(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.Network.NetworkRef.Name}, network)
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
	if err := r.usableAccount(ctx, row); err != nil {
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

// usableAccount refuses the account of the cluster's network when its
// access is not verified for creating in the network's region, as the API's
// CreateCluster does (accountreconcile.Usable); a refusal starts the
// region's check, so it fixes itself.
func (r *ClusterReconciler) usableAccount(ctx context.Context, row *model.Cluster) error {
	n, err := r.deps.Store.Networks().Get(ctx, row.TenantID, row.NetworkID)
	if err != nil {
		return err
	}
	a, err := r.deps.Store.Tenants().GetCloudAccountByID(ctx, row.TenantID, n.CloudAccountID)
	if err != nil {
		return err
	}
	warning, err := accountreconcile.Usable(a, n.Region, accountreconcile.Create)
	if err != nil {
		if regions, ok := accountreconcile.RegionToCheck(a, n.Region); ok {
			if rerr := r.deps.Store.Tenants().RequestAccessCheck(ctx, a.TenantID, a.ID, time.Now(), regions); rerr != nil {
				r.deps.logger().WarnContext(ctx, "start the check of a region", "tenant_id", a.TenantID, "bootstrap_id", a.ID,
					"region", n.Region, "error", rerr)
			}
		}
		return err
	}
	if warning != "" {
		r.deps.logger().WarnContext(ctx, warning, "tenant_id", a.TenantID, "bootstrap_id", a.ID, "region", n.Region)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager: for Clusters,
// woken by their provider objects and by their Helm objects
// (clusterHelmKinds). They own nothing of this API.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.Cluster{}, forObject()).
		Named("cluster").
		WithOptions(objectOptions(r.deps)).Watches(&cloudv1alpha1.ReleaseHold{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return enqueueAll(ctx, r.deps.Direct, &cloudv1alpha1.ClusterList{})
	}))
	owner := ownerRequests(r.owners, r.deps.Direct, crossplane.LabelCluster,
		func() client.ObjectList { return &cloudv1alpha1.ClusterList{} })
	b, err := providerWatches(b, mgr, owner)
	if err != nil {
		return err
	}
	for _, gvk := range clusterHelmKinds() {
		b = watchOwned(b, gvk, owner)
	}
	// A tenant's Component (enabled, changed, disabled) wakes the cluster
	// it is on: the cluster reconcile installs and removes it.
	b = b.Watches(&cloudv1alpha1.Component{}, handler.EnqueueRequestsFromMapFunc(componentCluster(r.owners, r.deps.Direct)))
	b = b.Watches(&cloudv1alpha1.Network{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		return enqueueAll(ctx, r.deps.Direct, &cloudv1alpha1.ClusterList{})
	}), builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	return b.Complete(r)
}

// componentCluster maps a Component to the request of the cluster it is
// on (its cluster-ID label).
func componentCluster(o *owners, c client.Reader) handler.MapFunc {
	return ownerRequests(o, c, cloudstore.LabelClusterID, func() client.ObjectList { return &cloudv1alpha1.ClusterList{} })
}

// clusterHelmKinds are the provider-helm kinds a cluster has objects of,
// next to the AWS ones (crossplaneaws.ManagedObjects): its Helm
// ProviderConfig and the Releases of its platform components. A change
// or a deletion of one wakes the cluster, as its AWS objects do: a
// deleted ProviderConfig is created again at once, and a chart that
// finished installing is seen without waiting for the next look.
func clusterHelmKinds() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{crossplaneaws.HelmProviderConfigGVK, crossplaneaws.HelmReleaseGVK}
}
