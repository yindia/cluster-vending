package telemetry

import (
	"regexp"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Bounds of the controller's event and duration labels.
const (
	maxReasons    = 160 // event reasons are code constants (pkg/reconcile/events.go)
	maxComponents = 32  // catalog components
)

var (
	reasonPattern    = regexp.MustCompile(`^[A-Z][A-Za-z]{0,47}$`)
	componentPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	eventKinds       = map[string]string{"CloudAccount": "cloudaccount", "Network": "network", "Cluster": "cluster",
		"SubTenant": "subtenant", "Release": "release", "ReleaseChannel": "releasechannel"}
	resultKinds = []string{"deprecations", "recommendations", "violations", "violation-events", "nodes"}
)

// Controller measures what the reconcilers make happen: the events they
// emit (every state transition is one) and how long the long transitions
// take. It implements reconcile.Metrics and the controller's EventCounter.
type Controller struct {
	events       *prometheus.CounterVec
	provisioning prometheus.Histogram
	upgrade      prometheus.Histogram
	component    *prometheus.HistogramVec
	results      *prometheus.CounterVec

	mu         sync.Mutex
	reasons    map[string]bool
	components map[string]bool
}

// NewController registers the controller's transition metrics on reg.
func NewController(reg prometheus.Registerer) *Controller {
	long := []float64{60, 120, 300, 600, 900, 1200, 1800, 2700, 3600, 5400, 7200, 10800, 21600}
	c := &Controller{
		events: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cluster_vending_events_total",
			Help: "Kubernetes events the controller emitted, by object kind, type and reason: every transition (upgrade started, finished or halted, teardown timed out, stuck release recovered, Helm release given up on, rollout halted, ...) is one."},
			[]string{"kind", "type", "reason"}),
		provisioning: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "cluster_vending_cluster_provisioning_seconds",
			Help: "Time from a cluster's creation to its first RUNNING state.", Buckets: long}),
		upgrade: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "cluster_vending_upgrade_duration_seconds",
			Help: "Duration of finished Kubernetes upgrades, from first seen in progress to every step done.", Buckets: long}),
		component: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "cluster_vending_component_install_seconds",
			Help:    "Time a platform component spent installing before it was Ready, by component.",
			Buckets: []float64{10, 30, 60, 120, 300, 600, 900, 1200, 1800, 3600}}, []string{"component"}),
		results: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cluster_vending_results_write_failures_total",
			Help: "Scan results that could not be written to a cluster's results bucket, by result kind."}, []string{"kind"}),
		reasons:    map[string]bool{},
		components: map[string]bool{},
	}
	reg.MustRegister(c.events, c.provisioning, c.upgrade, c.component, c.results)
	for _, kind := range resultKinds {
		c.results.WithLabelValues(kind)
	}
	return c
}

// remember admits value into set while it is below limit.
func (c *Controller) remember(set map[string]bool, value string, limit int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if set[value] {
		return value
	}
	if len(set) >= limit {
		return labelOther
	}
	set[value] = true
	return value
}

// Event counts one emitted event. Unknown kinds and reasons that are not
// a reason's shape become "other".
func (c *Controller) Event(kind, eventType, reason string) {
	k, ok := eventKinds[kind]
	if !ok {
		k = labelOther
	}
	r := labelOther
	if reasonPattern.MatchString(reason) {
		r = c.remember(c.reasons, reason, maxReasons)
	}
	c.events.WithLabelValues(k, label(eventType, "Normal", "Warning"), r).Inc()
}

// ClusterProvisioned observes a cluster's creation to its first RUNNING.
func (c *Controller) ClusterProvisioned(d time.Duration) { c.provisioning.Observe(d.Seconds()) }

// UpgradeFinished observes a finished Kubernetes upgrade.
func (c *Controller) UpgradeFinished(d time.Duration) { c.upgrade.Observe(d.Seconds()) }

// ComponentReady observes a platform component's install time.
func (c *Controller) ComponentReady(component string, d time.Duration) {
	name := labelOther
	if componentPattern.MatchString(component) {
		name = c.remember(c.components, component, maxComponents)
	}
	c.component.WithLabelValues(name).Observe(d.Seconds())
}

// ResultWriteFailed counts a scan result not written to the results bucket.
func (c *Controller) ResultWriteFailed(kind string) {
	k := labelOther
	for _, known := range resultKinds {
		if kind == known {
			k = kind
		}
	}
	c.results.WithLabelValues(k).Inc()
}
