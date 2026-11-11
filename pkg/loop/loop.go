// Package loop is the server's background loop: every interval it moves
// on the networks and clusters that are not settled (being created,
// updated or deleted, not fully applied, or rendered by an older
// renderer), so API reads have no side effects and nothing waits for a
// client to poll. It runs behind the manager's leader election (a Lease):
// one server replica at a time. Reconciling is idempotent; a failed row is
// retried on the next pass.
package loop

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/yindia/cluster-vending/pkg/database/repository"
)

// DefaultInterval is how often the loop runs.
const DefaultInterval = 30 * time.Second

// Kinds of rows, as metric labels.
const (
	kindNetwork = "network"
	kindCluster = "cluster"
)

var (
	passDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "cluster_vending_loop_duration_seconds", Help: "Duration of one pass of the background loop.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
	})
	rowsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cluster_vending_loop_rows_total", Help: "Networks and clusters the background loop reconciled.",
	}, []string{"kind"})
	rowErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cluster_vending_loop_errors_total", Help: "Networks and clusters the background loop failed to reconcile (apply, delete or read errors).",
	}, []string{"kind"})
	clustersByState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cluster_vending_clusters", Help: "Clusters by stored state.",
	}, []string{"state"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(passDuration, rowsProcessed, rowErrors, clustersByState)
}

// Reconciler moves one network or cluster on (NetworkService.Reconcile,
// ClusterService.Reconcile).
type Reconciler interface {
	Reconcile(ctx context.Context, tenantID, id string) error
}

// Loop is the background loop. Add it to the manager: it implements
// manager.Runnable and needs leader election.
type Loop struct {
	Store              repository.Store
	Networks, Clusters Reconciler
	// NetworkStates and ClusterStates are the stored states looked at
	// every pass; RenderTarget also selects rows rendered by an older
	// renderer.
	NetworkStates, ClusterStates []string
	RenderTarget                 int64
	// Interval between passes (DefaultInterval when zero).
	Interval time.Duration
	Logger   *slog.Logger
}

// NeedLeaderElection makes the manager run the loop on the leader only.
func (l *Loop) NeedLeaderElection() bool { return true }

// Start runs passes until ctx ends.
func (l *Loop) Start(ctx context.Context) error {
	interval := l.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	l.Logger.InfoContext(ctx, "background loop started", "interval", interval.String(), "render_target", l.RenderTarget)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		l.Pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Pass reconciles every pending cluster, then every pending network
// (clusters first: a network is deleted once its clusters are gone).
func (l *Loop) Pass(ctx context.Context) {
	start := time.Now()
	defer func() { passDuration.Observe(time.Since(start).Seconds()) }()

	clusters, err := l.Store.Clusters().ListPending(ctx, l.ClusterStates, l.RenderTarget)
	if err != nil {
		l.Logger.ErrorContext(ctx, "background loop: list clusters", "error", err)
	}
	for i := range clusters {
		l.one(ctx, kindCluster, l.Clusters, clusters[i].TenantID, clusters[i].ID)
	}
	networks, err := l.Store.Networks().ListPending(ctx, l.NetworkStates, l.RenderTarget)
	if err != nil {
		l.Logger.ErrorContext(ctx, "background loop: list networks", "error", err)
	}
	for i := range networks {
		l.one(ctx, kindNetwork, l.Networks, networks[i].TenantID, networks[i].ID)
	}
	l.countClusters(ctx)
}

func (l *Loop) one(ctx context.Context, kind string, r Reconciler, tenantID, id string) {
	if ctx.Err() != nil {
		return
	}
	rowsProcessed.WithLabelValues(kind).Inc()
	if err := r.Reconcile(ctx, tenantID, id); err != nil {
		rowErrors.WithLabelValues(kind).Inc()
		l.Logger.ErrorContext(ctx, "background loop: reconcile", "kind", kind, "tenant_id", tenantID, "id", id, "error", err)
	}
}

func (l *Loop) countClusters(ctx context.Context) {
	counts, err := l.Store.Clusters().CountByState(ctx)
	if err != nil {
		l.Logger.ErrorContext(ctx, "background loop: count clusters", "error", err)
		return
	}
	clustersByState.Reset()
	for state, n := range counts {
		clustersByState.WithLabelValues(state).Set(float64(n))
	}
}
