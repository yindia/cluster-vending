package reconcile

import "time"

// Metrics is what the reconcilers measure of the transitions they make
// (pkg/telemetry.Controller). Nil in Deps: nothing is measured (the API
// handlers, tests).
type Metrics interface {
	// ClusterProvisioned: a cluster reached RUNNING for the first time, d
	// after it was created.
	ClusterProvisioned(d time.Duration)
	// UpgradeFinished: a Kubernetes upgrade finished, d after it started.
	UpgradeFinished(d time.Duration)
	// ComponentReady: a platform component became Ready after installing
	// for d.
	ComponentReady(component string, d time.Duration)
	// ResultWriteFailed: a scan result of kind was not written to the
	// cluster's results bucket.
	ResultWriteFailed(kind string)
}

// noMetrics measures nothing.
type noMetrics struct{}

func (noMetrics) ClusterProvisioned(time.Duration)     {}
func (noMetrics) UpgradeFinished(time.Duration)        {}
func (noMetrics) ComponentReady(string, time.Duration) {}
func (noMetrics) ResultWriteFailed(string)             {}

// Measure is d.Metrics, or one that measures nothing.
func (d Deps) Measure() Metrics {
	if d.Metrics == nil {
		return noMetrics{}
	}
	return d.Metrics
}
