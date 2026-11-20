package controller

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/convert"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/recommend"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/reconcile/recommendations"
	"github.com/yindia/cluster-vending/pkg/reconcile/resultsink"
	"github.com/yindia/cluster-vending/pkg/results"
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
	// sink writes the recommendations to the cluster's results bucket.
	sink *resultsink.Sink
	// history decides when a cluster's recommendations get a history object
	// (at most one an hour).
	history *historyGate
}

// recommendationHistoryInterval is how often a cluster's recommendations get
// a history object: a set changes with every sample (every five minutes), and
// a history object per change would be about 8,600 small objects a cluster
// over the bucket's 30 days.
const recommendationHistoryInterval = time.Hour

// historyGate remembers when each cluster last got a history object. In
// memory: after a restart the first write has one.
type historyGate struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// due reports whether the cluster's next write has a history object at at.
func (g *historyGate) due(id string, at time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	last, ok := g.last[id]
	return !ok || at.Sub(last) >= recommendationHistoryInterval
}

// wrote records a history object written at at.
func (g *historyGate) wrote(id string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[string]time.Time{}
	}
	g.last[id] = at
}

// NewRecommendationReconciler returns the sampler of Clusters.
func NewRecommendationReconciler(d Deps) *RecommendationReconciler {
	return &RecommendationReconciler{deps: d, enabled: !d.NoRecommendations, sink: resultsink.New(d.Deps, d.logger()), history: &historyGate{}}
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
	leave := r.deps.FreezeBarrier.Enter()
	defer leave()
	if !r.enabled {
		return ctrl.Result{}, nil
	}
	obj := &cloudv1alpha1.Cluster{}
	if err := r.deps.Direct.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	tenant, id := obj.Labels[cloudstore.LabelTenant], obj.Labels[cloudstore.LabelID]
	frozen, err := r.deps.Freeze(ctx, id)
	if err != nil {
		return ctrl.Result{}, err
	}
	if frozen.Holds() || managed.IsPaused(obj) || id == "" || !scannable(obj) {
		return ctrl.Result{RequeueAfter: notScannableDelay}, nil
	}
	sampler := r.sampler
	sampler.Store = r.deps.Store.Clusters()
	sampler.Publish = func(ctx context.Context, _ recommendations.Cluster, summary *cloudv1.ClusterRecommendations, h recommend.History, at time.Time) string {
		return r.publish(ctx, tenant, id, summary, h, at)
	}
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

// publish computes the recommendations the recorded usage gives and writes
// them, all of them, to the results bucket, then records the summary. The
// API server then reads a bounded object, never the usage history. What it
// returns is a notice for the summary's message when the results stay in
// the Cluster object's reach only; "" when they were written, when nothing
// changed since the last write, or when the cluster has no bucket by design.
func (r *RecommendationReconciler) publish(ctx context.Context, tenant, id string, summary *cloudv1.ClusterRecommendations, h recommend.History, at time.Time) string {
	set := &cloudv1.ListClusterRecommendationsResponse{Summary: proto.CloneOf(summary)}
	for _, row := range recommend.Rows(h, at) {
		set.Recommendations = append(set.Recommendations, row.Proto())
	}
	set.Total = int32(len(set.Recommendations)) //nolint:gosec // bounded by recommend.MaxContainers
	content := proto.CloneOf(set)
	content.Summary.SampledAt, content.Summary.AttemptedAt = nil, nil // times change on every sample
	var opts []results.PutOption
	withHistory := r.history == nil || r.history.due(id, at)
	if !withHistory {
		opts = append(opts, results.WithoutHistory()) // latest only: the hour's history object exists
	}
	staged, notice := r.sink.Stage(ctx, tenant, id, results.Recommendations, set, at, model.ResultRef{
		Digest: resultsink.Digest(content), CollectedAt: at, AttemptedAt: summary.GetAttemptedAt().AsTime(),
		Total: set.Total, Truncated: summary.GetTruncated()}, opts...)
	if staged != nil && !staged.Unchanged && withHistory && r.history != nil {
		r.history.wrote(id, at)
	}
	if err := staged.Commit(ctx); err != nil {
		r.deps.logger().WarnContext(ctx, "recommendations summary not recorded", "tenant_id", tenant, "cluster_id", id, "error", err)
	}
	return notice
}
