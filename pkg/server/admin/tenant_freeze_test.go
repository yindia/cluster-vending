package admin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestFreezeProjectionFailureReportsSavedPolicy(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	unavailable := true
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cloud.Tenant{}, &cloud.ReleaseHold{}).
		WithObjects(&cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := obj.(*cloud.Tenant); ok && unavailable {
				return errors.New("status unavailable")
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		}}).Build()
	svc := NewService(reconcile.Deps{Kube: c})
	_, err = svc.Freeze(t.Context(), &v1.FreezeRequest{Reason: "maintenance"})
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "policy was saved") {
		t.Fatalf("ambiguous result: %v", err)
	}
	hold := &cloud.ReleaseHold{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: reconcile.FreezeName}, hold); err != nil {
		t.Fatal(err)
	}
	if !hold.Spec.Platform || hold.Spec.Reason != "maintenance" {
		t.Fatal("projection failure changed canonical policy")
	}
	unavailable = false
	if _, err := svc.Freeze(t.Context(), &v1.FreezeRequest{Reason: "maintenance"}); err != nil {
		t.Fatal(err)
	}
	tenant := &cloud.Tenant{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Status.Freeze == nil || !tenant.Status.Freeze.Frozen || tenant.Status.Freeze.Revision != hold.Spec.Revision {
		t.Fatal("retry did not repair projection")
	}
	// Deleting canonical policy may also fail projection. The error still says
	// saved, and an idempotent retry clears stale Tenant state.
	unavailable = true
	_, err = svc.Unfreeze(t.Context(), &v1.UnfreezeRequest{})
	if connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "policy was saved") {
		t.Fatal(err)
	}
	unavailable = false
	if _, err := svc.Unfreeze(t.Context(), &v1.UnfreezeRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, tenant); err != nil {
		t.Fatal(err)
	}
	if tenant.Status.Freeze != nil || tenant.Annotations["crossplane.io/paused"] != "" {
		t.Fatal("retry did not clear stale projection")
	}
}
