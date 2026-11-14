package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
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
// management cluster (account.Reconciler.Apply), checks, when due, that the
// tenant role works and allows the platform's calls
// (account.Reconciler.VerifyAccess), and reports both in the status. An account is bootstrapped through the API, which decides its
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
			"this account was created without the API: accounts are bootstrapped through it (account create), which stores the account's role and ProviderConfig")
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
	var access *model.AccountAccess
	var wait time.Duration
	if err == nil {
		access, wait, err = r.verify(ctx, tenant, id)
	}
	if serr := r.report(ctx, req, err, access); serr != nil {
		return ctrl.Result{}, serr
	}
	if err != nil {
		r.deps.logger().ErrorContext(ctx, "reconcile account", "namespace", obj.Namespace, "name", obj.Name, "error", err)
		return ctrl.Result{RequeueAfter: failedDelay}, nil
	}
	if wait > 0 && wait < resyncDelay {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	return ctrl.Result{RequeueAfter: resyncDelay}, nil
}

// verify runs the account's access check when it is due
// (account.Reconciler.VerifyAccess): the access after it, nil when none
// ran, and how long until the next is due.
func (r *CloudAccountReconciler) verify(ctx context.Context, tenant, id string) (*model.AccountAccess, time.Duration, error) {
	row, err := r.deps.Store.Tenants().GetCloudAccountByID(ctx, tenant, id)
	if err != nil {
		return nil, 0, err
	}
	return r.rec.VerifyAccess(ctx, row, time.Now())
}

// report writes the phase, the Ready condition and, with a check that ran,
// the access of the account from how the pass went, when they changed.
// An account whose access was denied is Failed: the platform cannot act
// in it. The object is read again: the reconciler recorded its objects in
// the status meanwhile.
func (r *CloudAccountReconciler) report(ctx context.Context, req ctrl.Request, passErr error, access *model.AccountAccess) error {
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
	if access != nil {
		obj.Status.Access = cloudstore.AccessStatus(access)
	}
	current := cloudstore.AccessModel(obj.Status.Access)
	if r.deps.AWSAccessCheck != nil || current != nil {
		meta.SetStatusCondition(&obj.Status.Conditions, convert.Condition(account.AccessCondition(current), obj.Generation))
	}
	if account.AccessDenied(current) {
		obj.Status.Phase = cloudv1alpha1.CloudAccountPhaseFailed
	}
	pause := model.ParsePause(obj.Annotations[cloudstore.AnnotationPaused])
	if pause != nil {
		paused := account.PausedCondition(pause)
		meta.SetStatusCondition(&obj.Status.Conditions, metav1.Condition{Type: paused.GetType(), Status: metav1.ConditionTrue,
			Reason: paused.GetReason(), Message: paused.GetMessage(), ObservedGeneration: obj.Generation})
	} else {
		meta.RemoveStatusCondition(&obj.Status.Conditions, platform.ConditionPaused)
	}
	if equality.Semantic.DeepEqual(before, &obj.Status) {
		return nil
	}
	if err := r.deps.Direct.Status().Update(ctx, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	// What changed is an event of the account, once: the status is the
	// record of what was reported.
	if n := accountEvent(meta.FindStatusCondition(before.Conditions, ConditionReady), cond); n != nil {
		r.deps.Emit(ctx, accountObject(obj), n.Type, n.Reason, n.Message)
	}
	if n := accountPauseEvent(meta.FindStatusCondition(before.Conditions, platform.ConditionPaused), pause); n != nil {
		r.deps.Emit(ctx, accountObject(obj), n.Type, n.Reason, n.Message)
	}
	if n := account.AccessNote(cloudstore.AccessModel(before.Access), current); n != nil {
		r.deps.Emit(ctx, accountObject(obj), n.Type, n.Reason, n.Message)
	}
	return nil
}

// accountPauseEvent says what a change of the account's pause means: the
// pause began (Paused) or ended (Resumed). Nil when nothing changed.
func accountPauseEvent(was *metav1.Condition, pause *model.Pause) *platform.Note {
	switch {
	case pause != nil && was == nil:
		n := platform.Normal(platform.ReasonPaused, account.PausedCondition(pause).GetMessage())
		return &n
	case pause == nil && was != nil:
		n := platform.Normal(platform.ReasonResumed, "the pause ended: the platform and the providers act on it again")
		return &n
	default:
		return nil
	}
}

// accountObject is the object events of a CloudAccount are about.
func accountObject(obj *cloudv1alpha1.CloudAccount) platform.Object {
	return platform.Object{Kind: platform.KindCloudAccount, Namespace: obj.Namespace, Name: obj.Name}
}

// accountEvent says what a change of the account's Ready condition means:
// bootstrapped when it became True, a warning when it became False or is
// False for another reason than before. Nil when nothing changed.
func accountEvent(was *metav1.Condition, is metav1.Condition) *platform.Note {
	switch {
	case is.Status == metav1.ConditionTrue && (was == nil || was.Status != metav1.ConditionTrue):
		n := platform.Normal(platform.ReasonBootstrapped, "the tenant namespace and the account's ProviderConfig are in the management cluster")
		return &n
	case is.Status == metav1.ConditionFalse && (was == nil || was.Status != metav1.ConditionFalse || was.Message != is.Message):
		n := platform.Warning(platform.ReasonProviderConfigFailed, "the account's ProviderConfig could not be applied: "+is.Message)
		return &n
	default:
		return nil
	}
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
		if err := r.deps.Direct.Update(ctx, obj); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		r.deps.Emit(ctx, accountObject(obj), platform.EventNormal, platform.ReasonDeleted,
			"the account's ProviderConfig is gone and the account is removed")
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
		WithOptions(objectOptions(r.deps)).
		Complete(r)
}
