package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/controller/api/v1alpha1"
)

// ClusterReconciler reconciles a Cluster object. Not implemented.
type ClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cloud.cluster-vending.io,resources=clusters/finalizers,verbs=update

// Reconcile is not implemented: it does nothing. It will make the
// cluster's Network its owner (owner.Set), create or import its cloud
// resources, report them in the status, and hold the cluster's finalizer
// (cloudv1alpha1.ClusterFinalizer) until they are gone.
func (r *ClusterReconciler) Reconcile(context.Context, ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager: for Clusters,
// which own nothing of this API.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudv1alpha1.Cluster{}).
		Named("cluster").
		Complete(r)
}
