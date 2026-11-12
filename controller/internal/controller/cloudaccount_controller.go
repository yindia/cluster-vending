package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/controller/api/v1alpha1"
)

// CloudAccountReconciler reconciles a CloudAccount object. Not
// implemented.
type CloudAccountReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=cloudaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=cloudaccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=cloudaccounts/finalizers,verbs=update

// Reconcile is not implemented: it does nothing. It will bootstrap the
// account's provider credential, report it in the status, and hold the
// account's finalizer (cloudv1alpha1.CloudAccountFinalizer) while the
// account owns a Network.
func (r *CloudAccountReconciler) Reconcile(context.Context, ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager: for
// CloudAccounts, woken by the Networks they own.
func (r *CloudAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.CloudAccount{}).
		Owns(&cloudv1alpha1.Network{}).
		Named("cloudaccount").
		Complete(r)
}
