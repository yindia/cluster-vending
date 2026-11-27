package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/deprecation"
)

// scanOptions are the options of the three scanners (deprecated APIs,
// policy, resource recommendations): Deps.ScanWorkers clusters at once
// (--scan-workers), at least one. A scan reaches into a workload cluster
// and can take tens of seconds, so one at a time left a fleet of ~100
// waiting behind each slow one.
func scanOptions(d Deps) controller.Options {
	return controller.Options{MaxConcurrentReconciles: max(d.ScanWorkers, 1)}
}

const (
	// ReachabilityProbeInterval is how often an unreachable cluster's API
	// server is probed at most (by all the scanners together).
	ReachabilityProbeInterval = 5 * time.Minute
	// reachabilityProbeTimeout bounds a probe: a GET of /version answers
	// in milliseconds when the API server is there.
	reachabilityProbeTimeout = 5 * time.Second
)

// ReachabilityProbes keep the scanners off clusters whose status says
// Reachable=False (the controller could not read the workload cluster on
// its last pass): such a cluster is scanned again only after a cheap probe
// (a GET of /version with a short timeout) succeeds, and is probed at most
// every ReachabilityProbeInterval. Without it each scanner spent its full
// timeout on every unreachable cluster, every pass. One value is shared by
// the scanners; safe for concurrent use. A nil one never skips.
type ReachabilityProbes struct {
	probe func(ctx context.Context, kubeconfig []byte) error
	now   func() time.Time

	mu   sync.Mutex
	last map[types.UID]probeResult
}

type probeResult struct {
	at time.Time
	ok bool
}

// NewReachabilityProbes is a gate probing API servers with GET /version.
func NewReachabilityProbes() *ReachabilityProbes {
	return &ReachabilityProbes{probe: probeVersion, now: time.Now, last: map[types.UID]probeResult{}}
}

// Skip reports whether a scan of obj is to be skipped, and when to look
// again. A cluster not marked unreachable is scanned (and forgotten); an
// unreachable one is probed unless it was within the interval, and
// scanned when the latest probe succeeded.
func (p *ReachabilityProbes) Skip(ctx context.Context, obj *cloudv1alpha1.Cluster, kubeconfig func(context.Context) ([]byte, error)) (time.Duration, bool) {
	if p == nil {
		return 0, false
	}
	if !meta.IsStatusConditionFalse(obj.Status.Conditions, cloudv1alpha1.ConditionReachable) {
		p.mu.Lock()
		delete(p.last, obj.UID)
		p.mu.Unlock()
		return 0, false
	}
	now := p.now()
	p.mu.Lock()
	last, probed := p.last[obj.UID]
	p.mu.Unlock()
	if probed && now.Sub(last.at) < ReachabilityProbeInterval {
		return ReachabilityProbeInterval - now.Sub(last.at), !last.ok
	}
	ok := p.reach(ctx, kubeconfig) == nil
	p.mu.Lock()
	p.last[obj.UID] = probeResult{at: now, ok: ok}
	p.mu.Unlock()
	if ok {
		return 0, false
	}
	return ReachabilityProbeInterval, true
}

func (p *ReachabilityProbes) reach(ctx context.Context, kubeconfig func(context.Context) ([]byte, error)) error {
	call, cancel := context.WithTimeout(ctx, reachabilityProbeTimeout)
	defer cancel()
	k, err := kubeconfig(call)
	if err != nil {
		return err
	}
	return p.probe(call, k)
}

// probeVersion GETs the API server's /version with the cluster's
// kubeconfig, within reachabilityProbeTimeout.
func probeVersion(_ context.Context, kubeconfig []byte) error {
	cfg, err := deprecation.RESTConfig(kubeconfig)
	if err != nil {
		return err
	}
	cfg.Timeout = reachabilityProbeTimeout
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("probe the workload cluster: %w", err)
	}
	if _, err := dc.ServerVersion(); err != nil {
		return fmt.Errorf("probe the workload cluster: %w", err)
	}
	return nil
}
