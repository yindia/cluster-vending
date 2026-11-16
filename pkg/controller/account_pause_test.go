package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

// A paused account's own status says who paused it, since when and why
// (the Paused condition), and its pause and resume are one event each.
func TestPausedAccountStatusAndEvents(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	rec := &platform.RecordedEvents{}
	w.deps.Events = rec
	r := NewCloudAccountReconciler(w.deps)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}}
	reconcile := func(times int) *metav1.Condition {
		t.Helper()
		for range times {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
		obj := &cloudv1alpha1.CloudAccount{}
		w.must(t, w.k.Get(ctx, req.NamespacedName, obj))
		return meta.FindStatusCondition(obj.Status.Conditions, platform.ConditionFrozen)
	}

	if c := reconcile(1); c != nil {
		t.Fatalf("an account that is not paused has %v", c)
	}
	w.pauseTenant(t, operatorPause)
	c := reconcile(3)
	if c == nil || c.Status != metav1.ConditionTrue || !strings.HasPrefix(c.Message, "frozen by alice since 2026-11-14T09:00:00Z (provider upgrade)") {
		t.Fatalf("Paused condition of a paused account = %+v", c)
	}
	w.pauseTenant(t, nil)
	if c := reconcile(3); c != nil {
		t.Fatalf("after the resume: %+v", c)
	}

	var got []string
	for _, e := range rec.All() {
		if e.Reason == platform.ReasonPlatformFrozen || e.Reason == platform.ReasonPlatformUnfrozen {
			got = append(got, e.Reason)
		}
	}
	if strings.Join(got, ",") != platform.ReasonPlatformFrozen+","+platform.ReasonPlatformUnfrozen {
		t.Fatalf("pause events = %v, want one Paused then one Resumed", got)
	}
}
