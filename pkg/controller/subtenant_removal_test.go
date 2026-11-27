package controller

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// removalClient is a fake management cluster that also serves the removed
// SubTenant kind and CustomResourceDefinitions, as unstructured objects.
func removalClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(subTenantGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(subTenantGVK.GroupVersion().WithKind("SubTenantList"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(crdGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(crdGVK.GroupVersion().WithKind("CustomResourceDefinitionList"), &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func subTenantCRD() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(crdGVK)
	u.SetName(subTenantCRDName)
	return u
}

func storedSubTenant(namespace, name string, deleting bool, finalizers ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(subTenantGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetFinalizers(finalizers)
	if deleting {
		u.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	}
	return u
}

// A management cluster upgraded from a build with sub-tenants: the
// SubTenant CRD and objects that still carry the old controller's
// finalizer, one of them already being deleted. Every object is released
// (no finalizer left, nothing done in workload clusters: the client never
// reaches one) and the CRD is deleted, which Kubernetes follows by deleting
// what is left of its objects. Each release is logged.
func TestReleaseRemovedSubTenantsReleasesEveryObjectAndDeletesTheCRD(t *testing.T) {
	c := removalClient(t, subTenantCRD(),
		storedSubTenant("tenant-acme", "team-a", false, "cloud.cluster-vending.io/subtenant"),
		storedSubTenant("tenant-acme", "team-b", true, "cloud.cluster-vending.io/subtenant"),
		storedSubTenant("tenant-zeta", "team-c", false, "cloud.cluster-vending.io/subtenant", "example.com/other"),
		storedSubTenant("tenant-zeta", "plain", false))
	logs := &bytes.Buffer{}

	released, err := ReleaseRemovedSubTenants(t.Context(), c, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{"tenant-acme/team-a", "tenant-acme/team-b", "tenant-zeta/team-c"}; strings.Join(released, ",") != strings.Join(want, ",") {
		t.Errorf("released %v, want %v", released, want)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(subTenantGVK.GroupVersion().WithKind("SubTenantList"))
	if err := c.List(t.Context(), list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if len(item.GetFinalizers()) != 0 {
			t.Errorf("%s/%s still has finalizers %v", item.GetNamespace(), item.GetName(), item.GetFinalizers())
		}
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: subTenantCRDName}, subTenantCRD()); !apierrors.IsNotFound(err) {
		t.Errorf("the SubTenant CRD is still there: %v", err)
	}
	for _, want := range []string{"tenant-acme/team-a", "tenant-zeta/team-c", subTenantCRDName} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not name %s:\n%s", want, logs)
		}
	}

	// Again, as on the next start: nothing left to do.
	again, err := ReleaseRemovedSubTenants(t.Context(), c, slog.New(slog.DiscardHandler))
	if err != nil || len(again) != 0 {
		t.Errorf("second run: released %v, %v; want nothing", again, err)
	}
}

// A management cluster that never had sub-tenants (or was migrated
// already): no CRD, nothing is read or changed.
func TestReleaseRemovedSubTenantsWithoutTheCRDIsANoOp(t *testing.T) {
	cluster := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "prod",
		Finalizers: []string{cloudv1alpha1.ClusterFinalizer}}}
	c := removalClient(t, cluster)

	released, err := ReleaseRemovedSubTenants(t.Context(), c, slog.New(slog.DiscardHandler))
	if err != nil || len(released) != 0 {
		t.Fatalf("released %v, %v; want nothing", released, err)
	}
	got := &cloudv1alpha1.Cluster{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(cluster), got); err != nil || len(got.Finalizers) != 1 {
		t.Errorf("an unrelated object changed: %v, %v", got.Finalizers, err)
	}
}

// The shipped Capsule definition (role NamespaceTenancy, the role removed
// with sub-tenants) left on the management cluster: one catalog.Sync is
// deleting (the in-use finalizer holds it while clusters still have
// Capsule) loses the role, so the finalizer can go once the clusters
// uninstalled it (the CRD now refuses the role on an update). One the
// platform team edited, which catalog.Sync keeps, is also switched off:
// without the role it would otherwise be an ordinary platform component,
// installed on every cluster. Each cluster then removes Capsule through its
// gated catalog change. Other definitions are not touched.
func TestRetireNamespaceTenancyDefinitions(t *testing.T) {
	now := metav1.Now()
	deleting := catalogEntry("capsule", cloudv1alpha1.ComponentDefinitionSpec{Management: cloudv1alpha1.ComponentManagementPlatform,
		Role: "NamespaceTenancy"})
	deleting.DeletionTimestamp, deleting.Finalizers = &now, []string{"cloud.cluster-vending.io/definition-in-use"}
	edited := catalogEntry("tenancy", cloudv1alpha1.ComponentDefinitionSpec{Management: cloudv1alpha1.ComponentManagementPlatform,
		Role: "NamespaceTenancy"})
	edited.Spec.Description = "the platform team's"
	nodes := catalogEntry("karpenter", cloudv1alpha1.ComponentDefinitionSpec{Management: cloudv1alpha1.ComponentManagementPlatform,
		Role: cloudv1alpha1.ComponentRoleNodeProvisioner})
	c := removalClient(t, deleting, edited, nodes)

	retired, err := RetireNamespaceTenancy(t.Context(), c, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(retired, ",") != "capsule,tenancy" {
		t.Errorf("retired %v, want capsule and tenancy", retired)
	}
	get := func(name string) *cloudv1alpha1.ComponentDefinition {
		d := &cloudv1alpha1.ComponentDefinition{}
		if err := c.Get(t.Context(), client.ObjectKey{Name: name}, d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := get("capsule"); d.Spec.Role != "" || d.Spec.Disabled {
		t.Errorf("capsule (being deleted): role %q, disabled %v; want no role, not switched off (it is going)", d.Spec.Role, d.Spec.Disabled)
	}
	if d := get("tenancy"); d.Spec.Role != "" || !d.Spec.Disabled || d.Spec.Description != "the platform team's" {
		t.Errorf("tenancy (edited): %+v; want no role, disabled, the rest kept", d.Spec)
	}
	if d := get("karpenter"); d.Spec.Role != cloudv1alpha1.ComponentRoleNodeProvisioner || d.Spec.Disabled {
		t.Errorf("karpenter changed: %+v", d.Spec)
	}
	if again, err := RetireNamespaceTenancy(t.Context(), c, slog.New(slog.DiscardHandler)); err != nil || len(again) != 0 {
		t.Errorf("second run: %v, %v; want nothing", again, err)
	}
}
