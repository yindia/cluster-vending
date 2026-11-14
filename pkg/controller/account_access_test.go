package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/reconcile/account"
)

// accessFake answers every access check with report.
type accessFake struct {
	report awscloud.AccessReport
	calls  int
}

func (f *accessFake) CheckAccess(context.Context, awscloud.Access, []string) (awscloud.AccessReport, error) {
	f.calls++
	return f.report, nil
}

func accessReport(region awscloud.RegionAccess, transient bool) awscloud.AccessReport {
	return awscloud.AccessReport{PrincipalARN: "arn:aws:sts::444455556666:assumed-role/cluster-vending-provisioner/s",
		Regions: []awscloud.RegionAccess{{Region: "us-east-1", OK: true}, region}, Transient: transient}
}

// The access check decides the account's phase and its AccessVerified
// condition; a transition is one event, and a transient failure keeps the
// last verdict.
func TestAccountAccessStatusAndEvents(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	events := &platform.RecordedEvents{}
	w.deps.Events = events
	check := &accessFake{report: accessReport(awscloud.RegionAccess{Region: "us-west-2", Reason: awscloud.ReasonDeniedBySCP,
		Message: "explicit deny in a service control policy"}, false)}
	w.deps.AWSAccessCheck = check
	r := NewCloudAccountReconciler(w.deps)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}}
	run := func(times int) *cloudv1alpha1.CloudAccount {
		t.Helper()
		for range times {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
		obj := &cloudv1alpha1.CloudAccount{}
		w.must(t, w.k.Get(ctx, req.NamespacedName, obj))
		return obj
	}
	ask := func() {
		t.Helper()
		obj := run(0)
		w.must(t, w.deps.Store.Tenants().RequestAccessCheck(ctx, obj.Labels[cloudstore.LabelTenant],
			obj.Labels[cloudstore.LabelID], time.Now().Add(time.Second)))
	}

	obj := run(3)
	c := meta.FindStatusCondition(obj.Status.Conditions, account.ConditionAccessVerified)
	if obj.Status.Phase != cloudv1alpha1.CloudAccountPhaseFailed || c == nil || c.Status != metav1.ConditionFalse ||
		c.Reason != awscloud.ReasonDeniedBySCP || obj.Status.Access == nil || obj.Status.Access.Verified {
		t.Fatalf("denied: phase %s, condition %+v, access %+v", obj.Status.Phase, c, obj.Status.Access)
	}
	if check.calls != 1 {
		t.Errorf("%d checks in 3 passes, want 1: the next is due later", check.calls)
	}

	check.report = accessReport(awscloud.RegionAccess{Region: "us-west-2", OK: true}, false)
	ask()
	obj = run(1)
	c = meta.FindStatusCondition(obj.Status.Conditions, account.ConditionAccessVerified)
	if obj.Status.Phase != cloudv1alpha1.CloudAccountPhaseReady || c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("verified: phase %s, condition %+v", obj.Status.Phase, c)
	}

	check.report = accessReport(awscloud.RegionAccess{Region: "us-west-2", Reason: awscloud.ReasonUnavailable, Message: "Rate exceeded"}, true)
	ask()
	obj = run(1)
	c = meta.FindStatusCondition(obj.Status.Conditions, account.ConditionAccessVerified)
	if obj.Status.Phase != cloudv1alpha1.CloudAccountPhaseReady || c == nil || c.Status != metav1.ConditionTrue || !obj.Status.Access.Stale {
		t.Fatalf("transient: phase %s, condition %+v, access %+v", obj.Status.Phase, c, obj.Status.Access)
	}

	var got []string
	for _, e := range events.All() {
		if e.Reason == platform.ReasonAccessVerified || e.Reason == platform.ReasonAccessDenied {
			got = append(got, e.Reason)
		}
	}
	if strings.Join(got, ",") != platform.ReasonAccessDenied+","+platform.ReasonAccessVerified {
		t.Errorf("access events = %v, want one AccessDenied then one AccessVerified", got)
	}
}

// A paused account is not checked.
func TestPausedAccountIsNotChecked(t *testing.T) {
	w := newWorld(t)
	check := &accessFake{report: accessReport(awscloud.RegionAccess{Region: "us-west-2", OK: true}, false)}
	w.deps.AWSAccessCheck = check
	w.pauseTenant(t, operatorPause)
	r := NewCloudAccountReconciler(w.deps)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}

	if check.calls != 0 {
		t.Errorf("a paused account was checked %d times", check.calls)
	}
}
