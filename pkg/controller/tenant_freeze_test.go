package controller

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/cli"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/admin"
	tenantservice "github.com/yindia/cluster-vending/pkg/server/tenant"
)

func TestTenantFreezeExpirySchedule(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *metav1.Time { v := metav1.NewTime(now.Add(d)); return &v }
	hold := &cloud.ReleaseHold{Spec: cloud.ReleaseHoldSpec{Platform: true, Until: at(3 * time.Minute), Rules: []cloud.FreezeRule{
		{TenantID: "foreign", Frozen: true, Until: at(time.Second)},
		{TenantID: "acme", Frozen: false, Until: at(time.Minute)},
	}}}
	if got := tenantFreezeExpiry(hold, "acme", now); got != time.Minute {
		t.Fatalf("let-through expiry: %s", got)
	}
	if got := tenantFreezeExpiry(hold, "acme", now.Add(2*time.Minute)); got != time.Minute {
		t.Fatalf("platform expiry: %s", got)
	}
	if got := tenantFreezeExpiry(hold, "acme", now.Add(4*time.Minute)); got != resyncDelay {
		t.Fatalf("expired policy busy loop: %s", got)
	}
	hold.Spec.Until = at(time.Millisecond)
	hold.Spec.Rules = nil
	if got := tenantFreezeExpiry(hold, "acme", now); got != time.Second {
		t.Fatalf("minimum requeue: %s", got)
	}
}

func TestTenantFreezeRPCProjection(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cloud.Tenant{}, &cloud.ReleaseHold{}).
		WithObjects(&cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "other", Annotations: map[string]string{managed.PausedAnnotation: "true", "manual-note": "keep"}}}).Build()
	deps := platform.Deps{Kube: c, KubeReader: c, Store: cloudstore.New(c)}
	r := NewTenantReconciler(Deps{Deps: deps, Direct: c})
	ts := tenantservice.NewTenantService(deps)
	var output bytes.Buffer
	clients := cli.Clients{Tenant: ts}
	if err := cli.Run(t.Context(), cli.TenantCreate, clients, cli.Options{ID: "acme"}, &output, cli.OutputJSON); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := cli.Run(t.Context(), cli.TenantList, clients, cli.Options{}, &output, cli.OutputTable); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "acme") {
		t.Fatal("created tenant missing from CLI directory")
	}

	read := func(name string) *cloud.Tenant {
		t.Helper()
		obj := &cloud.Tenant{}
		if err := c.Get(t.Context(), client.ObjectKey{Name: name}, obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}
	reconcile := func(name string) ctrl.Result {
		t.Helper()
		result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	reconcile("acme")
	reconcile("other")
	operator := admin.NewService(deps)
	ctx := auth.WithIdentity(t.Context(), &auth.Identity{Subject: "tenant-user"})
	if err := cli.Run(ctx, cli.TenantFreeze, clients, cli.Options{Tenant: "acme", Reason: "maintenance"}, &output, cli.OutputJSON); err != nil {
		t.Fatal(err)
	}
	// The actual mapper is shared by create/update/delete watches. Only registered
	// tenants are queued, never a foreign ID injected into policy text.
	hold := &cloud.ReleaseHold{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: platform.FreezeName}, hold); err != nil {
		t.Fatal(err)
	}
	requests := r.freezeTenants(t.Context(), hold)
	if len(requests) != 2 {
		t.Fatalf("hold watch queued %v", requests)
	}
	if got := r.freezeTenants(t.Context(), &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: "cluster-release-lock"}}); len(got) != 0 {
		t.Fatal("watched a non-policy hold")
	}
	for _, request := range requests {
		reconcile(request.Name)
	}
	own, other := read("acme"), read("other")
	if own.Status.Freeze == nil || !own.Status.Freeze.Frozen || own.Status.Freeze.Scope != "tenant acme" || own.Annotations[managed.PausedAnnotation] != "true" {
		t.Fatalf("tenant freeze missing: %+v", own)
	}
	if other.Status.Freeze != nil || other.Annotations["cloud.cluster-vending.io/freeze-paused"] != "" {
		t.Fatal("tenant freeze changed unrelated tenant")
	}
	if err := cli.Run(ctx, cli.TenantUnfreeze, clients, cli.Options{Tenant: "acme"}, &output, cli.OutputJSON); err != nil {
		t.Fatal(err)
	}
	reconcile("acme")
	own = read("acme")
	if own.Annotations[managed.PausedAnnotation] != "" || own.Status.Freeze != nil && own.Status.Freeze.Frozen {
		t.Fatal("tenant did not resume")
	}
	// A platform hold projects onto both registrations; a tenant RPC cannot bypass it.
	if _, err := operator.Freeze(t.Context(), &v1.FreezeRequest{Reason: "platform maintenance", ResetExceptions: true}); err != nil {
		t.Fatal(err)
	}
	reconcile("acme")
	reconcile("other")
	if _, err := ts.Unfreeze(ctx, &v1.TenantUnfreezeRequest{TenantId: "acme"}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("tenant bypassed platform hold: %v", err)
	}
	own = read("acme")
	if !own.Status.Freeze.Frozen || own.Status.Freeze.Scope != "platform" {
		t.Fatal("platform hold missing")
	}
	// Operator exceptions remain canonical and are reflected as Frozen:false.
	if _, err := operator.Unfreeze(t.Context(), &v1.UnfreezeRequest{TenantId: "acme"}); err != nil {
		t.Fatal(err)
	}
	reconcile("acme")
	own = read("acme")
	if own.Status.Freeze == nil || own.Status.Freeze.Frozen || own.Annotations[managed.PausedAnnotation] != "" {
		t.Fatal("operator exception ignored")
	}
	if _, err := operator.Unfreeze(t.Context(), &v1.UnfreezeRequest{}); err != nil {
		t.Fatal(err)
	}
	// A deleted hold still maps to registrations and clears only owned pauses.
	for _, request := range r.freezeTenants(t.Context(), hold) {
		reconcile(request.Name)
	}
	own, other = read("acme"), read("other")
	if own.Status.Freeze != nil || other.Status.Freeze != nil || other.Annotations[managed.PausedAnnotation] != "true" || other.Annotations["manual-note"] != "keep" {
		t.Fatal("unfreeze removed a manual pause or retained stale status")
	}
	// Timed policies schedule a pass before normal resync, then clear at expiry.
	until := metav1.NewTime(time.Now().Add(time.Minute))
	hold = &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Until: &until, Revision: "timed"}}
	if err := c.Create(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	result := reconcile("acme")
	if result.RequeueAfter <= 0 || result.RequeueAfter > time.Minute {
		t.Fatalf("expiry was not scheduled: %+v", result)
	}
	expired := metav1.NewTime(time.Now().Add(-time.Second))
	hold.Spec.Until = &expired
	if err := c.Update(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	reconcile("acme")
	own = read("acme")
	if own.Status.Freeze != nil || own.Annotations[managed.PausedAnnotation] != "" {
		t.Fatal("expired policy did not resume tenant")
	}
}

func TestTenantFreezeNamespaceFailure(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cloud.Tenant{}).WithObjects(
		&cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}},
		&cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Reason: "maintenance", Revision: "one"}},
	).Build()
	r := NewTenantReconciler(Deps{Direct: c})
	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	tenant := &cloud.Tenant{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > failedDelay {
		t.Fatal(result)
	}
	if !meta.IsStatusConditionFalse(tenant.Status.Conditions, "Ready") || tenant.Status.Freeze == nil || !tenant.Status.Freeze.Frozen || tenant.Annotations[managed.PausedAnnotation] != "true" {
		t.Fatalf("namespace failure hid policy: %+v", tenant)
	}
}

func TestTenantFreezeMissingPolicyResync(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cloud.Tenant{}).WithObjects(
		&cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", Annotations: map[string]string{managed.PausedAnnotation: "true", "cloud.cluster-vending.io/freeze-paused": "true"}}, Status: cloud.TenantStatus{Freeze: &cloud.TenantFreezeStatus{Frozen: true}}},
	).Build()
	r := NewTenantReconciler(Deps{Direct: c})
	delay, err := r.syncTenantFreeze(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if delay != resyncDelay {
		t.Fatalf("missing-policy safety resync: %s", delay)
	}
	tenant := &cloud.Tenant{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Status.Freeze != nil || tenant.Annotations[managed.PausedAnnotation] != "" {
		t.Fatal("deleted policy retained stale projection")
	}
}

func TestTenantFreezeLetThroughExpiry(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	until := metav1.NewTime(time.Now().Add(time.Minute))
	hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Revision: "one", Rules: []cloud.FreezeRule{{TenantID: "acme", Frozen: false, Until: &until}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cloud.Tenant{}).WithObjects(&cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}, hold).Build()
	r := NewTenantReconciler(Deps{Direct: c})
	delay, err := r.syncTenantFreeze(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if delay <= 0 || delay > time.Minute {
		t.Fatal("let-through expiry not scheduled")
	}
	tenant := &cloud.Tenant{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Status.Freeze == nil || tenant.Status.Freeze.Frozen || tenant.Annotations[managed.PausedAnnotation] != "" {
		t.Fatal("active exception ignored")
	}
	expired := metav1.NewTime(time.Now().Add(-time.Second))
	hold.Spec.Rules[0].Until = &expired
	if err := c.Update(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	if _, err := r.syncTenantFreeze(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
		t.Fatal(err)
	}
	if !tenant.Status.Freeze.Frozen || tenant.Annotations[managed.PausedAnnotation] != "true" {
		t.Fatal("expired exception did not restore platform hold")
	}
}
