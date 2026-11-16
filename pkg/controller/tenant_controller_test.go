package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	tenantlife "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
)

func tenantControllerClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := cloudv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(&cloudv1alpha1.Tenant{}, &corev1.Namespace{}).Build()
}

func TestTenantControllerCreatesNamespaceFromYAMLAndSettles(t *testing.T) {
	obj := &cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "yaml-tenant", Generation: 3}, Spec: cloudv1alpha1.TenantSpec{DisplayName: "YAML tenant"}}
	c := tenantControllerClient(t, obj)
	r := NewTenantReconciler(Deps{Direct: c})
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: obj.Name}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	stored := &cloudv1alpha1.Tenant{}
	if err := c.Get(t.Context(), req.NamespacedName, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Namespace != "tenant-yaml-tenant" || stored.Status.ObservedGeneration != 3 || !meta.IsStatusConditionTrue(stored.Status.Conditions, "Ready") {
		t.Fatalf("status = %#v", stored.Status)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: stored.Status.Namespace}, ns); err != nil {
		t.Fatal(err)
	}
	if len(ns.OwnerReferences) > 0 {
		t.Fatal("namespace must not be garbage-collected with tenant")
	}
	tenantVersion, namespaceVersion := stored.ResourceVersion, ns.ResourceVersion
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(t.Context(), req.NamespacedName, stored)
	_ = c.Get(t.Context(), client.ObjectKey{Name: ns.Name}, ns)
	if stored.ResourceVersion != tenantVersion || ns.ResourceVersion != namespaceVersion {
		t.Fatalf("settled reconcile wrote objects: tenant %s -> %s, namespace %s -> %s", tenantVersion, stored.ResourceVersion, namespaceVersion, ns.ResourceVersion)
	}
}

func TestTenantControllerMigrationAndNoResurrection(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-legacy", Labels: map[string]string{tenantlife.LabelTenant: "legacy"}, Annotations: map[string]string{tenantlife.AnnotationDisplayName: "Legacy", tenantlife.AnnotationCreatedAt: "2020-01-02T03:04:05Z"}}}
	c := tenantControllerClient(t, ns)
	r := NewTenantReconciler(Deps{Direct: c})
	requests := r.namespaceTenant(t.Context(), ns)
	if len(requests) != 1 || requests[0].Name != "legacy" {
		t.Fatalf("namespace mapping = %v", requests)
	}
	if _, err := r.Reconcile(t.Context(), requests[0]); err != nil {
		t.Fatal(err)
	}
	obj := &cloudv1alpha1.Tenant{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "legacy"}, obj); err != nil {
		t.Fatal(err)
	}
	if obj.Spec.DisplayName != "Legacy" || obj.Annotations[tenantlife.AnnotationCreatedAt] != "2020-01-02T03:04:05Z" {
		t.Fatalf("legacy metadata = %#v", obj)
	}
	// Simulate deliberate removal of the registered CR even if cleanup was skipped.
	obj.Finalizers = nil
	if err := c.Update(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), requests[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "legacy"}, &cloudv1alpha1.Tenant{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted registration resurrected: %v", err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: ns.Name}, ns); err != nil || ns.Annotations[tenantlife.AnnotationRegistration] == "" {
		t.Fatalf("namespace marker = %#v, %v", ns, err)
	}
}

func TestTenantControllerReportsCollisionAndRetainsNamespaceOnDelete(t *testing.T) {
	obj := &cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}}
	c := tenantControllerClient(t, obj, foreign)
	r := NewTenantReconciler(Deps{Direct: c})
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: "acme"}}
	if result, err := r.Reconcile(t.Context(), req); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("collision result = %v,%v", result, err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(obj.Status.Conditions, "Ready")
	if condition == nil || condition.Reason != "NamespaceCollision" || condition.Status != metav1.ConditionFalse {
		t.Fatalf("condition = %#v", condition)
	}
	// A new namespace owned by this tenant can reconcile and is retained on delete.
	if err := c.Delete(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "tenant-acme"}, foreign); err != nil || foreign.Labels[tenantlife.LabelTenant] != "" || foreign.Annotations[tenantlife.AnnotationRetainedTenant] != "acme" {
		t.Fatalf("namespace retention = %#v,%v", foreign, err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, &cloudv1alpha1.Tenant{}); !apierrors.IsNotFound(err) {
		t.Fatalf("registration remains: %v", err)
	}
}

func TestTenantControllerDoesNotDeleteDependentNetworks(t *testing.T) {
	obj := &cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	c := tenantControllerClient(t, obj)
	r := NewTenantReconciler(Deps{Direct: c})
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: "acme"}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	network := &cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "tenant-acme"}}
	if err := c.Create(t.Context(), network); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(t.Context(), req.NamespacedName, obj)
	if err := c.Delete(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	if result, err := r.Reconcile(t.Context(), req); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("dependent result = %v,%v", result, err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(network), network); err != nil {
		t.Fatal("tenant deletion removed dependent network", err)
	}
	if err := c.Get(t.Context(), req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
}
