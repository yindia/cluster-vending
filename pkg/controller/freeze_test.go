package controller

import (
	"testing"
	"time"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestPlatformFreezeExpiryResumesProviders(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	until := metav1.NewTime(now.Add(time.Minute))
	hold := &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName},
		Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "deploy", Until: &until}}
	vpc := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "tenant-acme",
		Labels: map[string]string{crossplane.LabelTenantID: "acme", crossplane.LabelNetwork: "network"}}}
	c := releaseClient(t, hold, vpc)
	r := &ReleaseHoldReconciler{c: c, now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: client.ObjectKey{Name: platform.FreezeName}}
	result, err := r.Reconcile(t.Context(), request)
	if err != nil || result.RequeueAfter != time.Minute {
		t.Fatalf("freeze: %+v, %v", result, err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(vpc), vpc); err != nil {
		t.Fatal(err)
	}
	if !managed.IsPaused(vpc) {
		t.Fatal("provider not paused")
	}
	now = now.Add(2 * time.Minute)
	if _, err := r.Reconcile(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(vpc), vpc); err != nil {
		t.Fatal(err)
	}
	if managed.IsPaused(vpc) {
		t.Fatal("provider still paused after expiry")
	}
}
