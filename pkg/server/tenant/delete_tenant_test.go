package tenant

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	tenantreconcile "github.com/yindia/cluster-vending/pkg/reconcile/tenant"
)

func deleteTenant(t *testing.T, s *TenantService, id string) (*v1.DeleteTenantResponse, error) {
	t.Helper()
	return s.DeleteTenant(directoryContext(t), &v1.DeleteTenantRequest{TenantId: id})
}

func readNamespace(t *testing.T, c client.Client, id string) *corev1.Namespace {
	t.Helper()
	ns := &corev1.Namespace{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: kube.TenantNamespace(id)}, ns); err != nil {
		t.Fatalf("namespace must be kept: %v", err)
	}
	return ns
}

func TestDeleteTenantRefusesWhileObjectsRemain(t *testing.T) {
	ns := kube.TenantNamespace("acme")
	s, c := directoryWorld(t, directoryRegistration("acme"), directoryNamespace("acme"),
		&cloudv1alpha1.CloudAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "aws-111122223333"}},
		&cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "net-a"}},
		&cloudv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "net-b"}},
		&cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "prod"}},
		&cloudv1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "karpenter"}},
		&cloudv1alpha1.SubTenant{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "team-a"}},
	)
	out, err := deleteTenant(t, s, "acme")
	if out != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("DeleteTenant = %v, %v; want FAILED_PRECONDITION", out, err)
	}
	for _, want := range []string{"1 sub-tenant (team-a)", "1 component (karpenter)", "1 cluster (prod)", "2 networks (net-a, net-b)", "1 cloud account (aws-111122223333)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, &cloudv1alpha1.Tenant{}); err != nil {
		t.Fatalf("a refused delete must keep the registration: %v", err)
	}
}

func TestDeleteTenantWithoutFinalizerUnregistersNamespace(t *testing.T) {
	s, c := directoryWorld(t, directoryRegistration("acme"), directoryNamespace("acme"))
	out, err := deleteTenant(t, s, "acme")
	if err != nil || out.GetTenant().GetId() != "acme" {
		t.Fatalf("DeleteTenant = %v, %v", out, err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, &cloudv1alpha1.Tenant{}); !apierrors.IsNotFound(err) {
		t.Fatalf("registration must be gone, got %v", err)
	}
	ns := readNamespace(t, c, "acme")
	if ns.Labels[cloudstore.LabelTenant] != "" || ns.Annotations[tenantreconcile.AnnotationRetainedTenant] != "acme" {
		t.Fatalf("namespace must be unregistered and retained for acme: %v %v", ns.Labels, ns.Annotations)
	}
	// Asked again: the tenant is gone.
	if _, err := deleteTenant(t, s, "acme"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("second DeleteTenant = %v; want NOT_FOUND", err)
	}
}

func TestDeleteTenantWithFinalizerLeavesItToTheController(t *testing.T) {
	registration := directoryRegistration("acme")
	registration.Finalizers = []string{cloudv1alpha1.TenantFinalizer}
	s, c := directoryWorld(t, registration, directoryNamespace("acme"))
	if _, err := deleteTenant(t, s, "acme"); err != nil {
		t.Fatal(err)
	}
	got := &cloudv1alpha1.Tenant{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, got); err != nil || got.DeletionTimestamp == nil {
		t.Fatalf("registration must be deleting, held by its finalizer: %v, %v", got.DeletionTimestamp, err)
	}
	if ns := readNamespace(t, c, "acme"); ns.Labels[cloudstore.LabelTenant] != "acme" {
		t.Fatalf("the controller unregisters the namespace, not the call: %v", ns.Labels)
	}
	// Being deleted answers as deleted.
	if out, err := deleteTenant(t, s, "acme"); err != nil || out.GetTenant().GetId() != "acme" {
		t.Fatalf("DeleteTenant while deleting = %v, %v", out, err)
	}
}

func TestDeleteTenantLegacyNamespace(t *testing.T) {
	s, c := directoryWorld(t, directoryNamespace("legacy"))
	if _, err := deleteTenant(t, s, "legacy"); err != nil {
		t.Fatal(err)
	}
	if ns := readNamespace(t, c, "legacy"); ns.Labels[cloudstore.LabelTenant] != "" {
		t.Fatalf("legacy namespace must be unregistered: %v", ns.Labels)
	}
}

func TestDeleteTenantUnknownAndInvalid(t *testing.T) {
	s, _ := directoryWorld(t)
	if _, err := deleteTenant(t, s, "nobody"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown tenant = %v; want NOT_FOUND", err)
	}
	if _, err := deleteTenant(t, s, "Not_Valid"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid tenant = %v; want INVALID_ARGUMENT", err)
	}
}

func TestDescribeDependents(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e", "f", "g"}
	if got := describeDependents(7, "networks", names); got != "7 networks (a, b, c, d, e, ...)" {
		t.Fatalf("got %q", got)
	}
}
