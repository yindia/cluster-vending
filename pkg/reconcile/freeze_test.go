package reconcile_test

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func platformFreeze(until *time.Time, except ...string) cloudv1alpha1.ReleaseHold {
	h := cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: reconcile.FreezeName,
		CreationTimestamp: metav1.NewTime(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))},
		Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "provider upgrade", By: "alice", Except: except}}
	if until != nil {
		at := metav1.NewTime(*until)
		h.Spec.Until = &at
	}
	return h
}

// The platform freeze holds every cluster and network but those it lets
// through; a freeze whose until passed, a cluster's own hold, and no
// freeze hold nothing.
func TestFreezeOf(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	clusterHold := cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: "lock"},
		Spec: cloudv1alpha1.ReleaseHoldSpec{ClusterID: "c1", Reason: "lock"}}
	tests := []struct {
		name       string
		holds      []cloudv1alpha1.ReleaseHold
		ids        []string
		frozen     bool
		letThrough bool
	}{
		{name: "not frozen"},
		{name: "a cluster's hold is no freeze", holds: []cloudv1alpha1.ReleaseHold{clusterHold}, ids: []string{"c1"}},
		{name: "frozen", holds: []cloudv1alpha1.ReleaseHold{platformFreeze(&future)}, ids: []string{"c1"}, frozen: true},
		{name: "let through", holds: []cloudv1alpha1.ReleaseHold{platformFreeze(nil, "n1")}, ids: []string{"n1"}, frozen: true, letThrough: true},
		{name: "another let through", holds: []cloudv1alpha1.ReleaseHold{platformFreeze(nil, "c2")}, ids: []string{"c1"}, frozen: true},
		{name: "until passed", holds: []cloudv1alpha1.ReleaseHold{platformFreeze(&past)}, ids: []string{"c1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reconcile.FreezeOf(tt.holds, now, tt.ids...)

			if (got != nil) != tt.frozen {
				t.Fatalf("FreezeOf = %+v, want frozen %v", got, tt.frozen)
			}
			if got == nil {
				return
			}
			if got.LetThrough != tt.letThrough || got.By != "alice" || got.Reason != "provider upgrade" || got.Since.IsZero() {
				t.Errorf("FreezeOf = %+v, want by alice, provider upgrade, let through %v", got, tt.letThrough)
			}
		})
	}
}

func TestScopedFreezePrecedenceIsolationAndExpiry(t *testing.T) {
	now := time.Now()
	until := metav1.NewTime(now.Add(time.Minute))
	hold := &cloudv1alpha1.ReleaseHold{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Scoped: true,
		Rules: []cloudv1alpha1.FreezeRule{
			{TenantID: "acme", Frozen: true},
			{NetworkID: "net", Frozen: false},
			{ClusterID: "one", Frozen: true, Until: &until},
		}}}
	for _, tt := range []struct {
		name   string
		target reconcile.FreezeTarget
		at     time.Time
		want   bool
	}{
		{"tenant freeze", reconcile.FreezeTarget{TenantID: "acme", NetworkID: "other"}, now, true},
		{"network exception", reconcile.FreezeTarget{TenantID: "acme", NetworkID: "net", ClusterID: "two"}, now, false},
		{"newer cluster freeze", reconcile.FreezeTarget{TenantID: "acme", NetworkID: "net", ClusterID: "one"}, now, true},
		{"expiry restores ancestor exception", reconcile.FreezeTarget{TenantID: "acme", NetworkID: "net", ClusterID: "one"}, now.Add(2 * time.Minute), false},
		{"another tenant", reconcile.FreezeTarget{TenantID: "beta", NetworkID: "beta-net", ClusterID: "three"}, now, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := reconcile.FreezeForTarget(hold, tt.at, tt.target).Holds(); got != tt.want {
				t.Fatalf("held=%v, want %v", got, tt.want)
			}
		})
	}
}

// A freeze starting, ending or letting the resource through is said once;
// one that goes on says nothing.
func TestFreezeNotes(t *testing.T) {
	held := &cloudv1.Freeze{By: "alice", Reason: "provider upgrade"}
	through := &cloudv1.Freeze{By: "alice", Reason: "provider upgrade", LetThrough: true}
	tests := []struct {
		name          string
		before, after *cloudv1.Freeze
		want          string
	}{
		{name: "never frozen"},
		{name: "frozen", after: held, want: reconcile.ReasonPlatformFrozen},
		{name: "goes on", before: held, after: held},
		{name: "unfrozen", before: held, want: reconcile.ReasonPlatformUnfrozen},
		{name: "let through", before: held, after: through, want: reconcile.ReasonLetThrough},
		{name: "let through from the start", after: through, want: reconcile.ReasonLetThrough},
		{name: "let through, then unfrozen", before: through, want: reconcile.ReasonPlatformUnfrozen},
		{name: "let through goes on", before: through, after: through},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reconcile.FreezeNotes(tt.before, tt.after)

			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("notes = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Reason != tt.want || got[0].Type != reconcile.EventNormal || got[0].Message == "" {
				t.Fatalf("notes = %+v, want one %s", got, tt.want)
			}
		})
	}
}

// A held resource's status says who froze the platform, why and that
// nothing changes; a let-through one says nothing, its resources paused
// by hand still counted.
func TestRowFreeze(t *testing.T) {
	since := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	freeze := &model.Freeze{By: "alice", Since: since, Reason: "provider upgrade"}
	paused := []*cloudv1.ManagedResource{{Kind: "NodeGroup", Paused: true}, {Kind: "Role"}}

	cond, note := reconcile.RowFreeze(freeze, paused)
	if cond == nil || cond.GetType() != reconcile.ConditionFrozen || cond.GetReason() != managed.ReasonPaused ||
		!strings.Contains(cond.GetMessage(), "frozen by alice since 2026-11-15T09:00:00Z (provider upgrade)") ||
		!strings.Contains(cond.GetMessage(), "admin unfreeze") || note == "" {
		t.Fatalf("held: condition %v, note %q", cond, note)
	}
	cond, note = reconcile.RowFreeze(&model.Freeze{By: "alice", LetThrough: true}, paused)
	if cond == nil || !strings.HasPrefix(cond.GetMessage(), "1 of 2 resources are paused") || note != "; 1 resources paused" {
		t.Fatalf("let through: condition %v, note %q", cond, note)
	}
	if cond, note = reconcile.RowFreeze(nil, nil); cond != nil || note != "" {
		t.Fatalf("not frozen: condition %v, note %q", cond, note)
	}
	if p := reconcile.FreezeProto(freeze); p.GetBy() != "alice" || !p.GetSince().AsTime().Equal(since) || p.GetLetThrough() {
		t.Errorf("FreezeProto = %v", p)
	}
	if reconcile.FreezeProto(nil) != nil {
		t.Error("FreezeProto(nil) is not nil")
	}
}

func TestScopedFreezeMessageAndRemoval(t *testing.T) {
	frozen := &model.Freeze{Scope: "network net-1", By: "alice", Reason: "maintenance"}
	condition, note := reconcile.RowFreeze(frozen, nil)
	if !strings.Contains(condition.GetMessage(), "network net-1 is frozen") || strings.Contains(condition.GetMessage(), "the platform is frozen") {
		t.Fatal(condition)
	}
	before := reconcile.FreezeProto(frozen)
	if got := reconcile.WithoutFreezeMessage("Ready"+note, before); got != "Ready" {
		t.Fatalf("stale scope message remains: %q", got)
	}
}

var nodeGroupGVK = schema.GroupVersionKind{Group: "eks.aws.m.upbound.io", Version: "v1beta1", Kind: "NodeGroup"}

func nodeGroup(name, tenant string, labels map[string]string, paused bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(nodeGroupGVK)
	u.SetNamespace(kube.TenantNamespace(tenant))
	u.SetName(name)
	l := crossplane.Labels(tenant)
	for k, v := range labels {
		l[k] = v
	}
	u.SetLabels(l)
	if paused {
		u.SetAnnotations(map[string]string{managed.PausedAnnotation: "true"})
	}
	return u
}

// The platform's pause covers every tenant's managed resources but those
// of what is let through; one cluster's or network's covers its own.
// Patching again changes nothing and counts nothing.
func TestSetProvidersPaused(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{
		nodeGroup("acme-c1", "acme", map[string]string{crossplane.LabelCluster: "c1"}, false),
		nodeGroup("acme-c2", "acme", map[string]string{crossplane.LabelCluster: "c2"}, false),
		nodeGroup("globex-n1", "globex", map[string]string{crossplane.LabelNetwork: "n1"}, false),
	}
	kc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	d := reconcile.Deps{Kube: kc}
	ctx := context.Background()
	paused := func(name, tenant string) bool {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(nodeGroupGVK)
		if err := kc.Get(ctx, client.ObjectKey{Namespace: kube.TenantNamespace(tenant), Name: name}, u); err != nil {
			t.Fatal(err)
		}
		return managed.IsPaused(u)
	}

	n, err := reconcile.SetProvidersPaused(ctx, d, reconcile.PauseScope{Except: []string{"c2"}}, true)
	if err != nil || n != 2 || !paused("acme-c1", "acme") || paused("acme-c2", "acme") || !paused("globex-n1", "globex") {
		t.Fatalf("platform pause: %d, %v", n, err)
	}
	if n, err = reconcile.SetProvidersPaused(ctx, d, reconcile.PauseScope{Except: []string{"c2"}}, true); err != nil || n != 0 {
		t.Fatalf("again: %d, %v; want 0", n, err)
	}
	if n, err = reconcile.SetProvidersPaused(ctx, d, reconcile.PauseScope{TenantID: "globex", NetworkID: "n1"}, false); err != nil || n != 1 ||
		paused("globex-n1", "globex") || !paused("acme-c1", "acme") {
		t.Fatalf("one network: %d, %v", n, err)
	}
	if n, err = reconcile.SetProvidersPaused(ctx, d, reconcile.PauseScope{}, false); err != nil || n != 1 || paused("acme-c1", "acme") {
		t.Fatalf("lift: %d, %v", n, err)
	}
}
