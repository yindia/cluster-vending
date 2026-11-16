package controller

import (
	"strings"
	"testing"
	"time"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
)

var operatorPause = &model.Freeze{By: "alice", Since: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), Reason: "provider upgrade"}

func (w world) pauseTenant(t *testing.T, p *model.Freeze) {
	t.Helper()
	hold := &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: platform.FreezeName}}
	if p == nil {
		w.must(t, client.IgnoreNotFound(w.k.Delete(t.Context(), hold)))
		return
	}
	hold.CreationTimestamp = metav1.NewTime(p.Since)
	hold.Spec = cloudv1alpha1.ReleaseHoldSpec{Platform: true, By: p.By, Reason: p.Reason}
	w.must(t, w.k.Create(t.Context(), hold))
}

// A network created while its tenant is paused is stored and not acted
// on: no provider object exists, and its status says who paused. After
// the resume the next pass applies it.
func TestNetworkOfAPausedTenantWaitsForTheResume(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.pauseTenant(t, operatorPause)
	api := w.networkAPI(t)
	created, err := api.CreateNetwork(ctx, &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{
		Name: "prod", Spec: &cloudv1.NetworkSpec{Region: "us-east-1", BootstrapId: w.account.ID, Layout: &cloudv1.NetworkLayout{}}}})
	w.must(t, err)
	id := created.GetNetwork().GetId()
	r := NewNetworkReconciler(w.deps)

	for range 2 {
		if _, err := r.Reconcile(ctx, request("prod")); err != nil {
			t.Fatal(err)
		}
	}

	if n := w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id); n != 0 {
		t.Fatalf("%d VPC objects of a paused network", n)
	}
	obj := w.network(t, "prod")

	if obj.Status.Phase != cloudv1alpha1.NetworkPhaseProvisioning ||
		!strings.Contains(obj.Status.Message, "frozen by alice since 2026-11-14T09:00:00Z (provider upgrade)") {
		t.Fatalf("phase %q, message %q", obj.Status.Phase, obj.Status.Message)
	}

	w.pauseTenant(t, nil)
	if _, err := r.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id); n != 1 {
		t.Fatalf("%d VPC objects after the resume, want 1", n)
	}
	if obj := w.network(t, "prod"); strings.Contains(obj.Status.Message, "paused") {
		t.Errorf("message after the resume: %q", obj.Status.Message)
	}
}

// A delete asked for while the network is paused is held: its objects
// stay until the resume, then they go.
func TestNetworkDeleteWhilePausedIsHeld(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	api := w.networkAPI(t)
	created, err := api.CreateNetwork(ctx, &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{
		Name: "prod", Spec: &cloudv1.NetworkSpec{Region: "us-east-1", BootstrapId: w.account.ID, Layout: &cloudv1.NetworkLayout{}}}})
	w.must(t, err)
	id := created.GetNetwork().GetId()
	r := NewNetworkReconciler(w.deps)
	if _, err := r.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	w.pauseTenant(t, operatorPause)
	if _, err := api.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: id}); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if _, err := r.Reconcile(ctx, request("prod")); err != nil {
			t.Fatal(err)
		}
	}

	if n := w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id); n != 1 {
		t.Fatalf("%d VPC objects while the delete is held, want 1", n)
	}

	w.pauseTenant(t, nil)
	for range 10 {
		if _, err := r.Reconcile(ctx, request("prod")); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.count(t, &awsec2.VPCList{}, crossplane.LabelNetwork, id); n != 0 {
		t.Fatalf("%d VPC objects after the resume, want none", n)
	}
}

// A paused account's ProviderConfig is not applied until the resume.
func TestPausedAccountIsNotApplied(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.pauseTenant(t, operatorPause)
	r := NewCloudAccountReconciler(w.deps)
	req := ctrl.Request{NamespacedName: client.ObjectKey{Namespace: namespace, Name: "aws-444455556666"}}
	pc := client.ObjectKey{Namespace: namespace, Name: "aws-0a1b2c3d"}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	if err := w.k.Get(ctx, pc, &awsv1beta1.ProviderConfig{}); client.IgnoreNotFound(err) != nil || err == nil {
		t.Fatalf("the ProviderConfig of a paused account: %v, want none", err)
	}

	w.pauseTenant(t, nil)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	w.must(t, w.k.Get(ctx, pc, &awsv1beta1.ProviderConfig{}))
}
