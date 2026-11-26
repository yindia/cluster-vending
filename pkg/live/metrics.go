package live

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/yindia/cluster-vending/pkg/telemetry"
)

// The live reader's metrics (PROP-02 7.4): builds by outcome and their
// duration, the answers given from a cooldown, BUSY refusals and the bytes
// the views keep. No label carries a tenant or cluster: the logs have them.
var (
	metrics = prometheus.NewRegistry()

	buildsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cluster_vending_live_builds_total",
		Help: "Live builds by kind and outcome (ok, or the failure reason)."}, []string{"kind", "outcome"})
	buildSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "cluster_vending_live_build_seconds",
		Help: "Duration of the live builds.", Buckets: []float64{.05, .1, .25, .5, 1, 2, 3, 5, 10, 15, 30}}, []string{"kind"})
	refusedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "cluster_vending_live_refused_total",
		Help: "Live reads answered without reaching the cluster: cooldown or busy."}, []string{"why"})
	viewBytes = prometheus.NewGauge(prometheus.GaugeOpts{Name: "cluster_vending_live_view_bytes",
		Help: "Bytes the live views keep (bounded by Limits.ViewBytes)."})

	// reads are the live RPCs served, by method and code, their duration
	// and those in flight (cluster_vending_live_rpc_*).
	reads = telemetry.NewRPCNamed(metrics, "cluster_vending_live_rpc")
)

func init() {
	metrics.MustRegister(buildsTotal, buildSeconds, refusedTotal, viewBytes,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}
