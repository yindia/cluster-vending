package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/controller/api/v1alpha1"
)

// NetworkReconciler reconciles a Network object. Not implemented.
type NetworkReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=networks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=networks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=networks/finalizers,verbs=update

// Reconcile is not implemented: it does nothing. It will make the
// network's CloudAccount its owner (owner.Set), create or import its cloud
// resources, report them in the status, and hold the network's finalizer
// (cloudv1alpha1.NetworkFinalizer) while the network owns a Cluster.
func (r *NetworkReconciler) Reconcile(context.Context, ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager: for Networks,
// woken by the Clusters they own.
func (r *NetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.Network{}).
		Owns(&cloudv1alpha1.Cluster{}).
		Named("network").
		Complete(r)
}
