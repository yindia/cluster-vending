package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
)

func TestTenantFreezeProjection(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	if err := cloud.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	a := &cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", Generation: 3}, Status: cloud.TenantStatus{Namespace: "tenant-acme", ObservedGeneration: 3, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "NamespaceReady", LastTransitionTime: metav1.NewTime(now)}}}}
	b := &cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "other", Annotations: map[string]string{managed.PausedAnnotation: "true", "manual": "keep"}}}
	hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Scoped: true, Revision: "one", Rules: []cloud.FreezeRule{{TenantID: "acme", Frozen: true, Reason: "maintenance", Since: metav1.NewTime(now)}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cloud.Tenant{}).WithObjects(a, b, hold).Build()
	d := Deps{Kube: c}
	sync := func() {
		t.Helper()
		if err := d.syncTenantFreezeAt(t.Context(), "acme", now); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, a); err != nil {
			t.Fatal(err)
		}
	}
	update := func() {
		t.Helper()
		if err := c.Update(t.Context(), hold); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	if a.Status.Freeze == nil || !a.Status.Freeze.Frozen || a.Status.Freeze.Revision != "one" || a.Status.Freeze.Scope != "tenant acme" || a.Status.Freeze.Reason != "maintenance" || a.Annotations[freezePausedAnnotation] != "true" {
		t.Fatalf("freeze not projected: %+v", a)
	}
	if !meta.IsStatusConditionTrue(a.Status.Conditions, "Ready") || a.Status.Namespace != "tenant-acme" || a.Status.ObservedGeneration != 3 {
		t.Fatal("namespace status overwritten")
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "other"}, b); err != nil {
		t.Fatal(err)
	}
	if b.Status.Freeze != nil || b.Annotations[freezePausedAnnotation] != "" {
		t.Fatal("targeted sync modified another tenant")
	}
	before := a.ResourceVersion
	sync()
	if a.ResourceVersion != before {
		t.Fatal("unchanged state was written again")
	}
	// A broad platform hold supersedes the previous scoped tenant freeze.
	hold.Spec.Scoped = false
	hold.Spec.Rules = nil
	hold.Spec.Revision = "two"
	hold.Spec.Reason = "platform maintenance"
	update()
	sync()
	if !a.Status.Freeze.Frozen || a.Status.Freeze.Scope != "platform" || a.Status.Freeze.Revision != "two" {
		t.Fatal(a.Status.Freeze)
	}
	// Canonical operator let-through exceptions must be reflected, not treated as holds.
	hold.Spec.Rules = []cloud.FreezeRule{{TenantID: "acme", Frozen: false, Since: metav1.NewTime(now)}}
	update()
	sync()
	if a.Status.Freeze == nil || a.Status.Freeze.Frozen || a.Annotations[managed.PausedAnnotation] != "" || meta.IsStatusConditionTrue(a.Status.Conditions, ConditionFrozen) {
		t.Fatalf("let-through retained pause: %+v", a)
	}
	// Clearing the policy removes only freeze-owned annotations; manual pauses survive.
	if err := d.SyncTenantFreezes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "other"}, b); err != nil {
		t.Fatal(err)
	}
	if b.Annotations[freezePausedAnnotation] != "" || b.Annotations[managed.PausedAnnotation] != "true" {
		t.Fatal("manual pause claimed")
	}
	if err := c.Delete(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	if err := d.SyncTenantFreezes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "other"}, b); err != nil {
		t.Fatal(err)
	}
	if b.Annotations[managed.PausedAnnotation] != "true" || b.Annotations["manual"] != "keep" || b.Status.Freeze != nil {
		t.Fatal("manual pause cleared")
	}
	sync()
	if a.Status.Freeze != nil {
		t.Fatal("removed policy retained")
	}
}

func TestTenantFreezeExpiry(t *testing.T) {
	now := time.Now()
	until := metav1.NewTime(now.Add(time.Minute))
	scheme := runtime.NewScheme()
	if err := cloud.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, scoped := range []bool{false, true} {
		hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Scoped: scoped, Until: &until, Revision: "expiry"}}
		if scoped {
			hold.Spec.Rules = []cloud.FreezeRule{{TenantID: "acme", Frozen: true, Until: &until}}
		}
		tenant := &cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hold, tenant).WithStatusSubresource(&cloud.Tenant{}).Build()
		d := Deps{Kube: c}
		if err := d.syncTenantFreezeAt(t.Context(), "acme", now); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
			t.Fatal(err)
		}
		if tenant.Status.Freeze == nil || tenant.Status.Freeze.Until == nil || !tenant.Status.Freeze.Frozen {
			t.Fatal("active timed hold missing")
		}
		if err := d.syncTenantFreezeAt(t.Context(), "acme", now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
			t.Fatal(err)
		}
		if tenant.Status.Freeze != nil || tenant.Annotations[managed.PausedAnnotation] != "" {
			t.Fatal("expired hold still paused")
		}
	}
}

// Deleting a tenant concurrently is harmless.
func TestTenantFreezeMissingTenant(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cloud.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	if err := (Deps{Kube: c}).SyncTenantFreeze(context.Background(), "absent"); err != nil {
		t.Fatal(err)
	}
}

// A failed policy read must leave existing status and pause intact.
func TestTenantFreezeReadFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cloud.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	tenant := &cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", Annotations: map[string]string{managed.PausedAnnotation: "true", freezePausedAnnotation: "true"}}, Status: cloud.TenantStatus{Freeze: &cloud.TenantFreezeStatus{Frozen: true, Revision: "old"}}}
	denied := errors.New("hold read unavailable")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tenant).WithStatusSubresource(&cloud.Tenant{}).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*cloud.ReleaseHold); ok {
				return denied
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	if err := (Deps{Kube: c}).SyncTenantFreeze(t.Context(), "acme"); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Status.Freeze.Revision != "old" || !tenant.Status.Freeze.Frozen || tenant.Annotations[managed.PausedAnnotation] != "true" {
		t.Fatal("failed read thawed tenant")
	}
}
