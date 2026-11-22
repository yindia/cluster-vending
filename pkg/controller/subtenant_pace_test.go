package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/naas"
)

func TestSubTenantPaceDelayByState(t *testing.T) {
	tests := []struct {
		name   string
		state  cloud.SubTenantState
		reason string
		want   time.Duration
	}{
		{"ready with access", cloud.SubTenantStateReady, "AccessReady", subTenantReadyResync},
		{"ready without groups", cloud.SubTenantStateReady, "Provisioned", subTenantReadyResync},
		{"ready waiting for bindings", cloud.SubTenantStateReady, "AccessBindingsPending", pendingDelay},
		{"provisioning", cloud.SubTenantStateProvisioning, "BaselinePending", pendingDelay},
		{"pending", cloud.SubTenantStatePending, "BootstrapPending", pendingDelay},
		{"deleting", cloud.SubTenantStateDeleting, "NamespaceCleanupPending", pendingDelay},
		{"degraded", cloud.SubTenantStateDegraded, "AccessRevocationPending", pendingDelay},
		{"failed", cloud.SubTenantStateFailed, "InvalidAllocation", subTenantFailedResync},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newSubTenantPace().delay("k", tt.state, tt.reason, true); got != tt.want {
				t.Fatalf("delay = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSubTenantPaceBacksOffUnchangedPassesAndResetsOnChange(t *testing.T) {
	p := newSubTenantPace()
	for i := 1; i < subTenantBackoffAfter; i++ {
		if got := p.delay("k", cloud.SubTenantStatePending, "BootstrapPending", false); got != pendingDelay {
			t.Fatalf("pass %d: delay = %s before the backoff", i, got)
		}
	}
	if got := p.delay("k", cloud.SubTenantStatePending, "BootstrapPending", false); got != subTenantSlowResync {
		t.Fatalf("delay = %s after %d unchanged passes, want %s", got, subTenantBackoffAfter, subTenantSlowResync)
	}
	if got := p.delay("other", cloud.SubTenantStatePending, "BootstrapPending", false); got != pendingDelay {
		t.Fatal("backoff leaked to another sub-tenant")
	}
	if got := p.delay("k", cloud.SubTenantStatePending, "BootstrapPending", true); got != pendingDelay {
		t.Fatalf("a status change did not reset the backoff: %s", got)
	}
	// Deletion never slows down.
	for range subTenantBackoffAfter + 1 {
		if got := p.delay("d", cloud.SubTenantStateDeleting, "DeletionBlocked", false); got != pendingDelay {
			t.Fatalf("deletion backed off: %s", got)
		}
	}
}

func TestSubTenantPaceClusterSlots(t *testing.T) {
	p := newSubTenantPace()
	var releases []func()
	for range subTenantClusterSlots {
		release, ok := p.acquire("c1")
		if !ok {
			t.Fatal("slot refused below the limit")
		}
		releases = append(releases, release)
	}
	if _, ok := p.acquire("c1"); ok {
		t.Fatal("a fifth reconcile of one cluster was allowed")
	}
	if release, ok := p.acquire("c2"); !ok {
		t.Fatal("a full cluster blocked another cluster")
	} else {
		release()
	}
	releases[0]()
	if _, ok := p.acquire("c1"); !ok {
		t.Fatal("a released slot was not reusable")
	}
}

func TestSubTenantPaceBootstrapCache(t *testing.T) {
	b, err := naas.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := newSubTenantPace()
	now := time.Now()
	if p.cachedBootstrap("c1", now) != nil {
		t.Fatal("empty cache hit")
	}
	p.rememberBootstrap("c1", b, now)
	if p.cachedBootstrap("c1", now.Add(subTenantBootstrapTTL-time.Second)) != b {
		t.Fatal("cache missed within the TTL")
	}
	if p.cachedBootstrap("c2", now) != nil {
		t.Fatal("cache hit for another cluster")
	}
	if p.cachedBootstrap("c1", now.Add(subTenantBootstrapTTL)) != nil {
		t.Fatal("cache hit after the TTL")
	}
	p.forgetBootstrap("c1")
	if p.cachedBootstrap("c1", now) != nil {
		t.Fatal("forgotten bootstrap was still served")
	}
}

func TestUntilExpiryCapsTheResync(t *testing.T) {
	now := time.Now()
	if got := untilExpiry(subTenantReadyResync, time.Time{}, now); got != subTenantReadyResync {
		t.Fatalf("no expiry changed the delay: %s", got)
	}
	if got := untilExpiry(subTenantReadyResync, now.Add(3*time.Minute), now); got != 3*time.Minute {
		t.Fatalf("delay = %s, want the expiry", got)
	}
	if got := untilExpiry(subTenantReadyResync, now.Add(-time.Minute), now); got != soon {
		t.Fatalf("a past expiry gave %s", got)
	}
}

// stCounting counts the workload reads of a pass and the namespace lists that
// select nothing (every namespace of the cluster).
type stCounting struct {
	client.Client
	reads, unfiltered int
}

func (c *stCounting) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.reads++
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *stCounting) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.reads++
	if _, ok := list.(*corev1.NamespaceList); ok && len(opts) == 0 {
		c.unfiltered++
	}
	return c.Client.List(ctx, list, opts...)
}

// A settled sub-tenant on one reconciler: resynced every 10 minutes, never
// listing every namespace, and, with the cluster's bootstrap cached, cheap
// enough that 100 of them on one cluster stay near one read a second.
func TestSubTenantSettledPassIsCheapAndSlow(t *testing.T) {
	w := provisionAccessWorld(t, true)
	qualifyAccessWorld(t, w)
	w.pass(t)
	materializeAccessBindings(t, w)
	w.pass(t)
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") {
		t.Fatalf("world did not settle: %+v", w.sub(t).Status.Conditions)
	}
	counter := &stCounting{Client: w.clients.client}
	w.clients.client = counter
	r := NewSubTenantReconciler(w.deps)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "tenant-acme", Name: stID}}
	pass := func() (time.Duration, int) {
		t.Helper()
		counter.reads = 0
		result, err := r.Reconcile(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		return result.RequeueAfter, counter.reads
	}
	coldDelay, cold := pass()
	warmDelay, warm := pass()
	if coldDelay != subTenantReadyResync || warmDelay != subTenantReadyResync {
		t.Fatalf("settled resync = %s, %s; want %s", coldDelay, warmDelay, subTenantReadyResync)
	}
	if warm >= cold {
		t.Fatalf("the bootstrap cache saved nothing: cold %d reads, warm %d", cold, warm)
	}
	if counter.unfiltered != 0 {
		t.Fatalf("%d unfiltered namespace lists in a steady-state pass", counter.unfiltered)
	}
	if !meta.IsStatusConditionTrue(w.sub(t).Status.Conditions, "AccessReady") {
		t.Fatal("a cached pass closed access")
	}
	// 100 settled sub-tenants of one cluster, each resynced every 10 minutes.
	perSecond := float64(100*warm) / subTenantReadyResync.Seconds()
	t.Logf("cold pass %d reads, warm pass %d reads: %.2f reads/s for 100 sub-tenants", cold, warm, perSecond)
	if perSecond > 4 {
		t.Fatalf("%.2f workload reads a second at rest for 100 sub-tenants", perSecond)
	}
}
