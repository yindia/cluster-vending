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
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
)

// NetworkReconciler reconciles Network objects by calling the network
// reconciler (pkg/reconcile/network) for the row each one is.
type NetworkReconciler struct {
	deps   Deps
	rec    *netreconcile.Reconciler
	owners *owners
}

// NewNetworkReconciler returns the controller of Networks.
func NewNetworkReconciler(d Deps) *NetworkReconciler {
	return &NetworkReconciler{deps: d, rec: netreconcile.New(d.Deps), owners: &owners{}}
}

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=networks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=networks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=networks/finalizers,verbs=update

// Reconcile moves the network one step on (see the package comment).
func (r *NetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := &cloudv1alpha1.Network{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	tenant, id := obj.Labels[cloudstore.LabelTenant], obj.Labels[cloudstore.LabelID]
	if id == "" {
		return r.adopt(ctx, obj)
	}
	r.owners.remember(id, obj)
	if !obj.DeletionTimestamp.IsZero() {
		if res, stop, err := r.deleting(ctx, obj, tenant, id); stop || err != nil {
			return res, err
		}
	} else if err := ensureFinalizer(ctx, r.deps.Direct, obj, cloudv1alpha1.NetworkFinalizer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if edited(obj.Generation, obj.Status.ObservedGeneration) {
		if err := r.bump(ctx, tenant, id); err != nil {
			return ctrl.Result{}, err
		}
	}
	err := r.rec.Reconcile(ctx, tenant, id)
	if err != nil {
		r.deps.logger().ErrorContext(ctx, "reconcile network", "namespace", obj.Namespace, "name", obj.Name,
			"network_id", id, "error", err)
	}
	row, rerr := r.deps.Store.Networks().Get(ctx, tenant, id)
	if errors.Is(rerr, repository.ErrNotFound) {
		r.owners.forget(id)
		return ctrl.Result{}, nil
	}
	if rerr != nil {
		return ctrl.Result{}, rerr
	}
	res := next(&row.Resource, netreconcile.PendingStates(), netreconcile.StateImported, err)
	if netreconcile.CodeChanging(row) && res.RequeueAfter > codeDelay {
		// A code change waits for its tier of the rollout, which admits it
		// without waking the network.
		res.RequeueAfter = codeDelay
	}
	return res, nil
}

// bump makes the reconciler apply a spec that was edited on the object:
// the row's generation moves on, as an update through the API would have
// moved it. The edit is applied as written; of what the API's update
// checks only what the kind's own validation rules say is checked.
func (r *NetworkReconciler) bump(ctx context.Context, tenant, id string) error {
	row, err := r.deps.Store.Networks().Get(ctx, tenant, id)
	if err != nil {
		return fmt.Errorf("read network %s: %w", id, err)
	}
	if row.State == netreconcile.StateDeleting {
		return nil
	}
	if row.State == netreconcile.StateImported || row.State == netreconcile.StateBlocked {
		// Imported and not taken over: read-only. The edit is not a new
		// generation; the reconciler puts what the import left back and
		// reports it (keepDiscovered).
		return nil
	}
	row.Generation++
	if row.State == netreconcile.StateReady {
		row.State = netreconcile.StateUpdating
	}
	if err := r.deps.Store.Networks().Update(ctx, row); err != nil && !errors.Is(err, repository.ErrConflict) {
		return fmt.Errorf("record the edit of network %s: %w", id, err)
	}
	return nil // a conflict: the row changed; the event of that change comes
}

// deleting handles an object someone asked to delete. With dependents the
// deletion does not start (stop is true): a network is never deleted
// under its clusters. Without, the row is marked DELETING; the reconciler
// then deletes the cloud resources pass by pass and removes the row, and
// with it the object's finalizer.
func (r *NetworkReconciler) deleting(ctx context.Context, obj *cloudv1alpha1.Network, tenant, id string) (ctrl.Result, bool, error) {
	clusters := &cloudv1alpha1.ClusterList{}
	// At the API server, not the cache: a cluster created a moment ago
	// must keep its network.
	if err := r.deps.Direct.List(ctx, clusters, client.InNamespace(obj.Namespace)); err != nil {
		return ctrl.Result{}, true, err
	}
	var names []string
	for _, c := range clusters.Items {
		if c.Labels[cloudstore.LabelNetworkID] == id || c.Spec.Network.NetworkRef.Name == obj.Name {
			names = append(names, c.Name)
		}
	}
	if len(names) > 0 {
		res, err := blocked(ctx, r.deps, obj, &obj.Status.Conditions, fmt.Sprintf(
			"%d clusters run in this network (%v); delete them first: a network is not deleted under its clusters", len(names), names))
		return res, true, err
	}
	row, err := r.deps.Store.Networks().Get(ctx, tenant, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ctrl.Result{}, true, nil
	}
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if row.State == netreconcile.StateDeleting {
		return ctrl.Result{}, false, nil
	}
	row.State = netreconcile.StateDeleting
	if err := r.deps.Store.Networks().Update(ctx, row); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("mark network %s as deleting: %w", id, err)
	}
	return ctrl.Result{}, false, nil
}

// adopt makes a Network that was created without the API a row: the row
// the API's CreateNetwork would have stored for the same network (the
// same checks, netreconcile.Reconciler.NewRow), written onto the object
// (cloudstore.AdoptNetwork). What the API would have refused is a
// condition on the object, not an error.
func (r *NetworkReconciler) adopt(ctx context.Context, obj *cloudv1alpha1.Network) (ctrl.Result, error) {
	if !obj.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // never a row: nothing of it exists in the cloud
	}
	if obj.Spec.ImportFrom != nil {
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reasonNotThroughAPI,
			"this network was created without the API and imports an existing VPC: imports are made through the API (network import), which looks the VPC up first")
	}
	tenant, err := tenantOf(ctx, r.deps, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if tenant == "" {
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reasonInvalid, fmt.Sprintf("namespace %s is not a tenant's: bootstrap the tenant first", obj.Namespace))
	}
	account := &cloudv1alpha1.CloudAccount{}
	err = r.deps.getAdopted(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: obj.Spec.AccountRef.Name}, account)
	if err != nil || account.Labels[cloudstore.LabelID] == "" {
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reasonWaiting, fmt.Sprintf("account %s is not a bootstrapped account of this tenant", obj.Spec.AccountRef.Name))
	}
	accountID := account.Labels[cloudstore.LabelID]
	row, err := r.rec.NewRow(ctx, &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{
		Name: obj.Name, DisplayName: obj.Spec.DisplayName, Labels: obj.Spec.Tags,
		Spec: convert.NetworkSpecFromAPI(obj.Spec, accountID, nil),
	}})
	if err != nil {
		reason, message := refusal(err)
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reason, message)
	}
	if err := cloudstore.AdoptNetwork(ctx, r.deps.Store, obj, row); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return ctrl.Result{RequeueAfter: soon}, nil
		}
		reason, message := refusal(err)
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reason, message)
	}
	r.deps.logger().InfoContext(ctx, "network adopted", "namespace", obj.Namespace, "name", obj.Name, "network_id", row.ID)
	return ctrl.Result{RequeueAfter: soon}, nil
}

// SetupWithManager sets up the controller with the Manager: for Networks,
// woken by the Clusters they own and by their provider objects.
func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.Network{}, forObject()).
		Owns(&cloudv1alpha1.Cluster{}).
		Named("network").
		WithOptions(objectOptions(r.deps))
	b, err := providerWatches(b, mgr, ownerRequests(r.owners, r.deps.Direct, crossplane.LabelNetwork,
		func() client.ObjectList { return &cloudv1alpha1.NetworkList{} }))
	if err != nil {
		return err
	}
	return b.Complete(r)
}
