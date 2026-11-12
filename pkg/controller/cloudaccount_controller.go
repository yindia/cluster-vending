package controller

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/reconcile/account"
)

// ConditionReady is the account's condition: its provider credential (the
// ProviderConfig) is in the management cluster.
const ConditionReady = "Ready"

// CloudAccountReconciler reconciles CloudAccount objects by calling the
// account reconciler (pkg/reconcile/account) for the row each one is.
type CloudAccountReconciler struct {
	deps Deps
	rec  *account.Reconciler
}

// NewCloudAccountReconciler returns the controller of CloudAccounts.
func NewCloudAccountReconciler(d Deps) *CloudAccountReconciler {
	return &CloudAccountReconciler{deps: d, rec: account.New(d.Deps)}
}

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=cloudaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=cloudaccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=cloudaccounts/finalizers,verbs=update

// Reconcile keeps the account's namespace and ProviderConfig in the
// management cluster (account.Reconciler.Apply) and reports it in the
// status. An account is bootstrapped through the API, which decides its
// ProviderConfig name and its role: one created without it is marked, not
// adopted.
func (r *CloudAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := &cloudv1alpha1.CloudAccount{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	tenant, id := obj.Labels[cloudstore.LabelTenant], obj.Labels[cloudstore.LabelID]
	if id == "" {
		if !obj.DeletionTimestamp.IsZero() {
			return ctrl.Result{}, nil
		}
		return flag(ctx, r.deps, obj, &obj.Status.Conditions, reasonNotThroughAPI,
			"this account was created without the API: accounts are bootstrapped through it (bootstrap create), which stores the account's role and ProviderConfig")
	}
	if !obj.DeletionTimestamp.IsZero() {
		return r.deleting(ctx, obj, tenant, id)
	}
	if err := ensureFinalizer(ctx, r.deps.Direct, obj, cloudv1alpha1.CloudAccountFinalizer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	err := r.rec.Reconcile(ctx, tenant, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ctrl.Result{}, nil
	}
	if serr := r.report(ctx, req, err); serr != nil {
		return ctrl.Result{}, serr
	}
	if err != nil {
		r.deps.logger().ErrorContext(ctx, "reconcile account", "namespace", obj.Namespace, "name", obj.Name, "error", err)
		return ctrl.Result{RequeueAfter: failedDelay}, nil
	}
	return ctrl.Result{RequeueAfter: resyncDelay}, nil
}

// report writes the phase and the Ready condition of the account from
// how the pass went, when they changed. The object is read again: the
// reconciler recorded its objects in the status meanwhile.
func (r *CloudAccountReconciler) report(ctx context.Context, req ctrl.Request, passErr error) error {
	obj := &cloudv1alpha1.CloudAccount{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	before := obj.Status.DeepCopy()
	cond := metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Bootstrapped",
		Message: "the account's ProviderConfig is in the management cluster", ObservedGeneration: obj.Generation}
	obj.Status.Phase = cloudv1alpha1.CloudAccountPhaseReady
	if passErr != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "BootstrapFailed", describe(passErr)
		obj.Status.Phase = cloudv1alpha1.CloudAccountPhaseFailed
	}
	meta.SetStatusCondition(&obj.Status.Conditions, cond)
	meta.RemoveStatusCondition(&obj.Status.Conditions, ConditionAdopted)
	if equality.Semantic.DeepEqual(before, &obj.Status) {
		return nil
	}
	return client.IgnoreNotFound(r.deps.Direct.Status().Update(ctx, obj))
}

// deleting handles an account someone asked to delete. While it has
// networks the deletion does not start: an account is never deleted under
// them. Without, its ProviderConfig is removed (account.Reconciler.Delete;
// the tenant namespace stays) and, once that is gone, the finalizer.
func (r *CloudAccountReconciler) deleting(ctx context.Context, obj *cloudv1alpha1.CloudAccount, tenant, id string) (ctrl.Result, error) {
	networks := &cloudv1alpha1.NetworkList{}
	if err := r.deps.Direct.List(ctx, networks, client.InNamespace(obj.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	var names []string
	for _, n := range networks.Items {
		if n.Labels[cloudstore.LabelAccountID] == id || n.Spec.AccountRef.Name == obj.Name {
			names = append(names, n.Name)
		}
	}
	if len(names) > 0 {
		return blocked(ctx, r.deps, obj, &obj.Status.Conditions, fmt.Sprintf(
			"%d networks belong to this account (%v); delete them first: an account is not deleted under its networks", len(names), names))
	}
	row, err := r.deps.Store.Tenants().GetCloudAccountByID(ctx, tenant, id)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return ctrl.Result{}, err
	}
	if err == nil {
		done, err := r.rec.Delete(ctx, row)
		if err != nil {
			return blocked(ctx, r.deps, obj, &obj.Status.Conditions, describe(err))
		}
		if !done {
			return ctrl.Result{RequeueAfter: applyingDelay}, nil
		}
	}
	// Read again: forgetting the account's records changed the object.
	if err := r.deps.Direct.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if controllerutil.RemoveFinalizer(obj, cloudv1alpha1.CloudAccountFinalizer) {
		return ctrl.Result{}, client.IgnoreNotFound(r.deps.Direct.Update(ctx, obj))
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager: for
// CloudAccounts, woken by the Networks they own (a deletion that waited
// for the last of them).
func (r *CloudAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.CloudAccount{}, forObject()).
		Owns(&cloudv1alpha1.Network{}).
		Named("cloudaccount").
		Complete(r)
}
