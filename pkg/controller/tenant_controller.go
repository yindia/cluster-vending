package controller

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	tenantlife "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
)

// TenantReconciler owns registration and namespace lifecycle, never cloud access.
type TenantReconciler struct{ deps Deps }

func NewTenantReconciler(d Deps) *TenantReconciler { return &TenantReconciler{deps: d} }

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch

func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Namespace != "" || !tenantlife.ValidID(req.Name) {
		return ctrl.Result{}, nil
	}
	obj := &cloudv1alpha1.Tenant{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		migrated, err := tenantlife.MigrateLegacy(ctx, r.deps.Direct, req.Name)
		if apierrors.IsNotFound(err) || errors.Is(err, tenantlife.ErrNamespaceCollision) {
			return ctrl.Result{}, nil
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		obj = migrated
	}
	if !obj.DeletionTimestamp.IsZero() {
		return r.deleteTenant(ctx, obj)
	}
	if err := ensureFinalizer(ctx, r.deps.Direct, obj, cloudv1alpha1.TenantFinalizer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ns, err := tenantlife.EnsureNamespace(ctx, r.deps.Direct, obj)
	if err != nil {
		reason := "NamespaceUnavailable"
		if errors.Is(err, tenantlife.ErrNamespaceCollision) {
			reason = "NamespaceCollision"
		}
		if serr := r.reportTenant(ctx, obj, "", metav1.ConditionFalse, reason, err.Error()); serr != nil {
			return ctrl.Result{}, serr
		}
		delay, freezeErr := r.syncTenantFreeze(ctx, obj.Name)
		if freezeErr != nil {
			return ctrl.Result{}, freezeErr
		}
		return ctrl.Result{RequeueAfter: min(failedDelay, delay)}, nil
	}
	if err := r.reportTenant(ctx, obj, ns.Name, metav1.ConditionTrue, "NamespaceReady", "The tenant namespace is ready."); err != nil {
		return ctrl.Result{}, err
	}
	delay, err := r.syncTenantFreeze(ctx, obj.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: delay}, nil
}

func (r *TenantReconciler) reportTenant(ctx context.Context, obj *cloudv1alpha1.Tenant, namespace string, status metav1.ConditionStatus, reason, message string) error {
	before := obj.DeepCopy()
	obj.Status.Namespace = namespace
	obj.Status.ObservedGeneration = obj.Generation
	meta.SetStatusCondition(&obj.Status.Conditions, tenantlife.Condition(obj, status, reason, message))
	if equality.Semantic.DeepEqual(before.Status, obj.Status) {
		return nil
	}
	return r.deps.Direct.Status().Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *TenantReconciler) deleteTenant(ctx context.Context, obj *cloudv1alpha1.Tenant) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(obj, cloudv1alpha1.TenantFinalizer) {
		return ctrl.Result{}, nil
	}
	ns := tenantlife.NamespaceFor(obj.Name)
	networks := &cloudv1alpha1.NetworkList{}
	if err := r.deps.Direct.List(ctx, networks, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, err
	}
	clusters := &cloudv1alpha1.ClusterList{}
	if err := r.deps.Direct.List(ctx, clusters, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, err
	}
	subtenants := &cloudv1alpha1.SubTenantList{}
	if err := r.deps.Direct.List(ctx, subtenants, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, err
	}
	if len(networks.Items)+len(clusters.Items)+len(subtenants.Items) > 0 {
		if err := r.reportTenant(ctx, obj, ns, metav1.ConditionFalse, "HasDependents", "Remove the tenant's SubTenants, networks and clusters before deleting its registration."); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: failedDelay}, nil
	}
	accounts := &cloudv1alpha1.CloudAccountList{}
	if err := r.deps.Direct.List(ctx, accounts, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, err
	}
	for i := range accounts.Items {
		a := &accounts.Items[i]
		if err := r.deps.Direct.Delete(ctx, a, client.PropagationPolicy(metav1.DeletePropagationOrphan)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	if len(accounts.Items) > 0 {
		return ctrl.Result{RequeueAfter: applyingDelay}, nil
	}
	if err := tenantlife.RetainNamespace(ctx, r.deps.Direct, obj.Name); err != nil {
		return ctrl.Result{}, err
	}
	before := obj.DeepCopy()
	controllerutil.RemoveFinalizer(obj, cloudv1alpha1.TenantFinalizer)
	err := r.deps.Direct.Patch(ctx, obj, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	return ctrl.Result{}, client.IgnoreNotFound(err)
}

func (r *TenantReconciler) namespaceTenant(_ context.Context, obj client.Object) []reconcile.Request {
	id := obj.GetLabels()[tenantlife.LabelTenant]
	if !tenantlife.ValidID(id) || obj.GetName() != tenantlife.NamespaceFor(id) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: id}}}
}

func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&cloudv1alpha1.Tenant{}, forObject()).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.namespaceTenant)).
		Watches(&cloudv1alpha1.ReleaseHold{}, handler.EnqueueRequestsFromMapFunc(r.freezeTenants)).
		Named("tenant").WithOptions(objectOptions(r.deps)).Complete(r)
}
