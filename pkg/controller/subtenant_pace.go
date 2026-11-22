package controller

import (
	"sync"
	"time"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/naas"
)

// How often a sub-tenant is looked at again, by state (PROP-05 6.3). The
// workload cluster is not watched, so remote drift is caught by these resyncs.
const (
	// subTenantReadyResync: a settled Ready sub-tenant (access open, or none
	// requested).
	subTenantReadyResync = 10 * time.Minute
	// subTenantFailedResync: the allocation itself is unusable.
	subTenantFailedResync = 5 * time.Minute
	// subTenantSlowResync: a Pending, Provisioning or unsettled Ready
	// sub-tenant whose status did not change for subTenantBackoffAfter passes.
	subTenantSlowResync   = 2 * time.Minute
	subTenantBackoffAfter = 10
	// subTenantBootstrapTTL: how long one cluster's verified shared templates
	// are trusted before they are read again.
	subTenantBootstrapTTL = 10 * time.Minute
	// subTenantClusterSlots: sub-tenant reconciles of one cluster at a time,
	// so one large cluster cannot take the whole worker pool.
	subTenantClusterSlots = 4
	// subTenantBusyDelay: a reconcile that found its cluster's slots taken.
	subTenantBusyDelay = time.Second
)

// subTenantSettled are the Ready reasons that need nothing further from the
// controller until something changes.
var subTenantSettled = map[string]bool{"AccessReady": true, "Provisioned": true}

// subTenantPace is the controller's memory across passes: unchanged-pass
// counts, verified bootstraps per cluster and per-cluster reconcile slots.
// It is only a cache: losing it (a restart) costs reads, never correctness.
type subTenantPace struct {
	mu        sync.Mutex
	unchanged map[string]int
	bootstrap map[string]verifiedBootstrap
	busy      map[string]int
}

type verifiedBootstrap struct {
	at time.Time
	b  *naas.Bootstrap
}

func newSubTenantPace() *subTenantPace {
	return &subTenantPace{unchanged: map[string]int{}, bootstrap: map[string]verifiedBootstrap{}, busy: map[string]int{}}
}

// acquire takes one of the cluster's slots; false when all are taken. The
// returned func gives it back.
func (p *subTenantPace) acquire(cluster string) (func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy[cluster] >= subTenantClusterSlots {
		return nil, false
	}
	p.busy[cluster]++
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.busy[cluster]--; p.busy[cluster] <= 0 {
			delete(p.busy, cluster)
		}
	}, true
}

// cachedBootstrap is the cluster's bootstrap verified less than
// subTenantBootstrapTTL ago, or nil.
func (p *subTenantPace) cachedBootstrap(cluster string, now time.Time) *naas.Bootstrap {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.bootstrap[cluster]
	if !ok || now.Sub(v.at) >= subTenantBootstrapTTL {
		return nil
	}
	return v.b
}

func (p *subTenantPace) rememberBootstrap(cluster string, b *naas.Bootstrap, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bootstrap[cluster] = verifiedBootstrap{at: now, b: b}
}

// forgetBootstrap makes the cluster's next pass read the templates again: a
// pass that did not settle may have failed because they changed.
func (p *subTenantPace) forgetBootstrap(cluster string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.bootstrap, cluster)
}

// delay is when the sub-tenant is looked at again after a pass that ended in
// state for reason; changed: the pass wrote a new status.
func (p *subTenantPace) delay(key string, state cloud.SubTenantState, reason string, changed bool) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if changed {
		delete(p.unchanged, key)
	} else {
		p.unchanged[key]++
	}
	switch {
	case state == cloud.SubTenantStateReady && subTenantSettled[reason]:
		delete(p.unchanged, key)
		return subTenantReadyResync
	case state == cloud.SubTenantStateFailed:
		return subTenantFailedResync
	case state == cloud.SubTenantStateDeleting || state == cloud.SubTenantStateDegraded:
		return pendingDelay
	case p.unchanged[key] >= subTenantBackoffAfter:
		return subTenantSlowResync
	}
	return pendingDelay
}

// forget drops what is kept for a sub-tenant that is gone.
func (p *subTenantPace) forget(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.unchanged, key)
}

// untilExpiry caps a resync at an open access record's expiry, so access
// closes on time whatever the state's resync.
func untilExpiry(delay time.Duration, expires, now time.Time) time.Duration {
	if expires.IsZero() {
		return delay
	}
	return max(min(delay, expires.Sub(now)), soon)
}
