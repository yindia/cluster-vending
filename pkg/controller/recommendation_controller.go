package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/reconcile/recommendations"
)

// systemNamespaces are the platform's whatever the catalog says: the
// Kubernetes and EKS ones.
var systemNamespaces = []string{"kube-system", "kube-public", "kube-node-lease"}

// RecommendationReconciler samples running clusters for resource
// recommendations (pkg/reconcile/recommendations): every five minutes it
// reads the workloads, the usage metrics-server reports and the cluster's
// own VPA recommendations with the kubeconfig its ClusterAuth wrote, and
// changes nothing there. The rolling usage it records is a ConfigMap in
// the tenant namespace (which binds the controller to
// reconcile.UsageHistoryWriterRole); the summary is
// status.recommendations. It is apart from the cluster's own controller:
// a sample that fails never fails the cluster.
type RecommendationReconciler struct {
	deps    Deps
	sampler recommendations.Sampler
	// enabled is --recommendations; off, nothing is sampled.
	enabled bool
}

// NewRecommendationReconciler returns the sampler of Clusters.
func NewRecommendationReconciler(d Deps) *RecommendationReconciler {
	return &RecommendationReconciler{deps: d, enabled: !d.NoRecommendations}
}

// SetupWithManager registers the sampler: one cluster at a time, woken
// by a change of a cluster's spec and by its own schedule.
func (r *RecommendationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("cluster-recommendations").
		For(&cloudv1alpha1.Cluster{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile samples the cluster when a sample is due and comes back when
// the next one is. A cluster that cannot be sampled (not running, being
// deleted, paused, or imported without a kubeconfig) is the same as for
// the deprecation scan (scannable).
func (r *RecommendationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if !r.enabled {
		return ctrl.Result{}, nil
	}
	obj := &cloudv1alpha1.Cluster{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	tenant, id := obj.Labels[cloudstore.LabelTenant], obj.Labels[cloudstore.LabelID]
	if id == "" || !scannable(obj) {
		return ctrl.Result{RequeueAfter: notScannableDelay}, nil
	}
	sampler := r.sampler
	sampler.Store = r.deps.Store.Clusters()
	next, err := sampler.Run(ctx, recommendations.Cluster{
		TenantID: tenant, ID: id, Last: convert.RecommendationsFromAPI(obj.Status.Recommendations),
		Kubeconfig: func(ctx context.Context) ([]byte, error) {
			return reconcile.ReadKubeconfig(ctx, r.deps.Direct, obj.Namespace, obj.Status.KubeconfigSecret)
		},
		Platform: r.platformNamespaces(ctx),
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// platformNamespaces says which namespaces are the platform's: those of
// the catalog's components, and the system ones. A catalog that cannot be
// read leaves the system ones only.
func (r *RecommendationReconciler) platformNamespaces(ctx context.Context) func(string) bool {
	set := map[string]bool{}
	for _, ns := range systemNamespaces {
		set[ns] = true
	}
	defs := &cloudv1alpha1.ComponentDefinitionList{}
	if err := r.deps.listCached(ctx, defs); err == nil {
		for _, d := range defs.Items {
			if d.Spec.Namespace != "" {
				set[d.Spec.Namespace] = true
			}
		}
	}
	return func(ns string) bool { return set[ns] }
}
