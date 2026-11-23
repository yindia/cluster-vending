package live

import (
	"context"
	"sync"
	"time"
)

// guard holds the limits of 5.9 that are not about memory: one build at a
// time per key (singleflight), builds per minute per cluster and per tenant,
// the fleet's builds in flight, and the cooldown after failures.
type guard struct {
	lim   Limits
	fleet chan struct{}

	mu        sync.Mutex
	inflight  map[buildKey]*call
	started   map[string][]time.Time // "c/<tenant>/<cluster>" and "t/<tenant>"
	cooldowns map[buildKey]*cooldown
}

type call struct {
	done chan struct{}
	cb   *cachedBuild
	err  error
}

type cooldown struct {
	failures int
	until    time.Time
	err      *Error
}

func newGuard(lim Limits) *guard {
	return &guard{lim: lim, fleet: make(chan struct{}, max(lim.FleetInFlight, 1)), inflight: map[buildKey]*call{},
		started: map[string][]time.Time{}, cooldowns: map[buildKey]*cooldown{}}
}

// do runs fn once per key at a time: a request that arrives while a build of
// its key runs waits for that build and shares its result.
func (g *guard) do(ctx context.Context, key buildKey, fn func() (*cachedBuild, error)) (*cachedBuild, error) {
	g.mu.Lock()
	if c, ok := g.inflight[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			return c.cb, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &call{done: make(chan struct{})}
	g.inflight[key] = c
	g.mu.Unlock()
	c.cb, c.err = fn()
	g.mu.Lock()
	delete(g.inflight, key)
	g.mu.Unlock()
	close(c.done)
	return c.cb, c.err
}

// cooling is the error a cluster in its cooldown answers with; nil when it
// is not cooling. No request reaches the cluster meanwhile.
func (g *guard) cooling(key buildKey, now time.Time) *Error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cd := g.cooldowns[key]
	if cd == nil || cd.err == nil || !now.Before(cd.until) {
		return nil
	}
	e := *cd.err
	e.RetryAfter = cd.until.Sub(now)
	return &e
}

// failed records a failure of key: one that reached the cluster starts the
// cooldown, the longer one after FailuresForLong in a row.
func (g *guard) failed(key buildKey, err *Error, now time.Time) {
	if !err.cools() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cd := g.cooldowns[key]
	if cd == nil {
		cd = &cooldown{}
		g.cooldowns[key] = cd
	}
	cd.failures++
	cd.err = err
	wait := g.lim.Cooldown
	if cd.failures >= g.lim.FailuresForLong {
		wait = g.lim.LongCooldown
	}
	cd.until = now.Add(wait)
}

// succeeded clears key's failures.
func (g *guard) succeeded(key buildKey) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.cooldowns, key)
}

// admit counts a build of key against the per-cluster and per-tenant rates;
// BUSY when either is used up for the last minute.
func (g *guard) admit(key buildKey, now time.Time) *Error {
	g.mu.Lock()
	defer g.mu.Unlock()
	ck, tk := "c/"+key.tenant+"/"+key.cluster, "t/"+key.tenant
	c, t := g.recent(ck, now), g.recent(tk, now)
	switch {
	case len(c) >= g.lim.BuildsPerClusterMinute:
		return &Error{Reason: ReasonBusy, Message: "too many live reads of this cluster in the last minute", RetryAfter: c[0].Add(time.Minute).Sub(now)}
	case len(t) >= g.lim.BuildsPerTenantMinute:
		return &Error{Reason: ReasonBusy, Message: "too many live reads of this tenant's clusters in the last minute", RetryAfter: t[0].Add(time.Minute).Sub(now)}
	}
	g.started[ck], g.started[tk] = append(c, now), append(t, now)
	return nil
}

// recent are the build starts of k within the last minute, oldest first.
func (g *guard) recent(k string, now time.Time) []time.Time {
	in := g.started[k]
	i := 0
	for i < len(in) && now.Sub(in[i]) >= time.Minute {
		i++
	}
	out := in[i:]
	if len(out) == 0 {
		delete(g.started, k)
		return nil
	}
	g.started[k] = out
	return out
}

// slot takes one of the fleet's build slots, waiting at most FleetWait; BUSY
// when none frees up. The returned func gives it back.
func (g *guard) slot(ctx context.Context) (func(), *Error) {
	t := time.NewTimer(g.lim.FleetWait)
	defer t.Stop()
	select {
	case g.fleet <- struct{}{}:
		return func() { <-g.fleet }, nil
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, &Error{Reason: ReasonBusy, Message: "too many live reads in flight; retry in a few seconds", RetryAfter: g.lim.FleetWait}
}
