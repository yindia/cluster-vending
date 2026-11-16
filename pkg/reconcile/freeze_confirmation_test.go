package reconcile_test

import (
	"context"
	"testing"
	"time"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestFreezeConfirmationDrainsLateWrites(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	hold := platformFreeze(nil)
	hold.Spec.Revision = "request-1"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hold).WithStatusSubresource(&cloudv1alpha1.ReleaseHold{}).Build()
	barrier := &reconcile.FreezeBarrier{}
	leave := barrier.Enter()
	released := false
	defer func() {
		if !released {
			leave()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := reconcile.ConfirmFreeze(t.Context(), reconcile.Deps{Kube: c, FreezeBarrier: barrier}, time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("confirmed before active reconciliation drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	late := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Name: "late", Namespace: "tenant-acme", Labels: crossplane.NetworkLabels("acme", "network")}}
	if err := c.Create(t.Context(), late); err != nil {
		t.Fatal(err)
	}
	leave()
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("confirmation did not complete")
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(late), late); err != nil {
		t.Fatal(err)
	}
	if !managed.IsPaused(late) {
		t.Fatal("late resource escaped freeze")
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: reconcile.FreezeName}, &hold); err != nil {
		t.Fatal(err)
	}
	if hold.Status.FreezePhase != "Frozen" || hold.Status.PausedResources != 1 || hold.Status.FreezeRevision != "request-1" {
		t.Fatal(hold.Status)
	}
}

func TestFreezeConfirmationCannotAcknowledgeANewerRequest(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	hold := platformFreeze(nil)
	hold.Spec.Revision = "old"
	resource := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "tenant-acme", Labels: crossplane.NetworkLabels("acme", "network")}}
	changed := false
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&hold, resource).WithStatusSubresource(&cloudv1alpha1.ReleaseHold{}).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if !changed {
				changed = true
				current := &cloudv1alpha1.ReleaseHold{}
				if err := c.Get(ctx, client.ObjectKey{Name: reconcile.FreezeName}, current); err != nil {
					return err
				}
				current.Spec.Revision = "new"
				if err := c.Update(ctx, current); err != nil {
					return err
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		}}).Build()
	if _, err := reconcile.ConfirmFreeze(t.Context(), reconcile.Deps{Kube: c}, time.Now()); err == nil {
		t.Fatal("stale confirmation succeeded")
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: reconcile.FreezeName}, &hold); err != nil {
		t.Fatal(err)
	}
	if hold.Status.FreezeRevision == "new" || hold.Status.FreezePhase == "Frozen" {
		t.Fatal("new request incorrectly acknowledged")
	}
}
