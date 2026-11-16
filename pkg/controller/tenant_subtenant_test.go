package controller

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

func TestTenantDeletionRetainsRegistrationForOrphanedSubTenant(t *testing.T) {
	obj := &cloud.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	c := tenantControllerClient(t, obj)
	r := NewTenantReconciler(Deps{Direct: c})
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: obj.Name}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	child := &cloud.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "tenant-acme"}}
	if err := c.Create(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if result, err := r.Reconcile(t.Context(), req); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("dependent result = %v, %v", result, err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, obj); err != nil {
		t.Fatal("tenant registration was removed with a remaining SubTenant", err)
	}
	if condition := meta.FindStatusCondition(obj.Status.Conditions, "Ready"); condition == nil || condition.Reason != "HasDependents" {
		t.Fatalf("missing dependency condition: %v", condition)
	}
	if err := c.Delete(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, obj); !apierrors.IsNotFound(err) {
		t.Fatalf("empty tenant registration remains: %v", err)
	}
}
