package tenant

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

func namespaceClient(t *testing.T, objects ...client.Object) client.Client {
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

func registration() *cloudv1alpha1.Tenant {
	return &cloudv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme", UID: types.UID("tenant-uid"), Annotations: map[string]string{AnnotationCreatedAt: "2024-02-03T04:05:06Z"}}, Spec: cloudv1alpha1.TenantSpec{DisplayName: "Acme production"}}
}

func TestNamespaceCreationAndNoOp(t *testing.T) {
	c := namespaceClient(t)
	obj := registration()
	ns, err := EnsureNamespace(t.Context(), c, obj)
	if err != nil {
		t.Fatal(err)
	}
	if ns.Name != "tenant-acme" || ns.Labels[LabelTenant] != "acme" || ns.Annotations[AnnotationRegistration] != "tenant-uid" || ns.Annotations[AnnotationDisplayName] != "Acme production" || ns.Annotations[AnnotationCreatedAt] != "2024-02-03T04:05:06Z" || len(ns.OwnerReferences) != 0 {
		t.Fatalf("namespace = %#v", ns)
	}
	version := ns.ResourceVersion
	ns, err = EnsureNamespace(t.Context(), c, obj)
	if err != nil || ns.ResourceVersion != version {
		t.Fatalf("no-op wrote namespace: %s -> %s, %v", version, ns.ResourceVersion, err)
	}
	obj.Spec.DisplayName = "Renamed"
	ns, err = EnsureNamespace(t.Context(), c, obj)
	if err != nil || ns.Annotations[AnnotationDisplayName] != "Renamed" || ns.Annotations[AnnotationCreatedAt] != "2024-02-03T04:05:06Z" {
		t.Fatalf("display update = %#v, %v", ns, err)
	}
}

func TestNamespaceCollisionNeverAdoptsForeignOrUnmarked(t *testing.T) {
	for name, ns := range map[string]*corev1.Namespace{
		"unmarked":                  {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}},
		"other tenant":              {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{LabelTenant: "other"}}},
		"foreign owner":             {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{LabelTenant: "acme"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Namespace", Name: "foreign", UID: "foreign"}}}},
		"phase terminating":         {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{LabelTenant: "acme"}}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating}},
		"retained for other tenant": {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Annotations: map[string]string{AnnotationRetainedTenant: "other"}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := namespaceClient(t, ns)
			before := &corev1.Namespace{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(ns), before); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureNamespace(t.Context(), c, registration()); !errors.Is(err, ErrNamespaceCollision) {
				t.Fatalf("collision = %v", err)
			}
			after := &corev1.Namespace{}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(ns), after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("collision mutated namespace")
			}
		})
	}
}

func TestMigrationPreservesLegacyMetadataAndDoesNotWriteNamespace(t *testing.T) {
	created := metav1.NewTime(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", CreationTimestamp: created, Labels: map[string]string{LabelTenant: "acme", "keep": "value"}, Annotations: map[string]string{AnnotationDisplayName: "Old Acme", AnnotationCreatedAt: "2021-02-03T00:00:00Z"}}}
	c := namespaceClient(t, ns)
	before := &corev1.Namespace{}
	_ = c.Get(t.Context(), client.ObjectKeyFromObject(ns), before)
	obj, err := MigrateLegacy(t.Context(), c, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Spec.DisplayName != "Old Acme" || obj.Annotations[AnnotationCreatedAt] != "2021-02-03T00:00:00Z" || obj.Namespace != "" {
		t.Fatalf("migration = %#v", obj)
	}
	after := &corev1.Namespace{}
	_ = c.Get(t.Context(), client.ObjectKeyFromObject(ns), after)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed namespace ownership or metadata")
	}
	if _, err := MigrateLegacy(t.Context(), c, "acme"); err != nil {
		t.Fatalf("migration retry = %v", err)
	}
}

func TestMigrationRejectsUntrustedAndRegisteredNamespaces(t *testing.T) {
	for name, ns := range map[string]*corev1.Namespace{
		"unmarked":                {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}},
		"foreign":                 {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{LabelTenant: "globex"}}},
		"registered then deleted": {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{LabelTenant: "acme"}, Annotations: map[string]string{AnnotationRegistration: "deleted-uid"}}},
		"retained":                {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{LabelTenant: "acme"}, Annotations: map[string]string{AnnotationRetainedTenant: "acme"}}},
		"terminating":             {ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{LabelTenant: "acme"}}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating}},
	} {
		t.Run(name, func(t *testing.T) {
			c := namespaceClient(t, ns)
			if _, err := MigrateLegacy(t.Context(), c, "acme"); !errors.Is(err, ErrNamespaceCollision) {
				t.Fatalf("migration = %v", err)
			}
			if err := c.Get(t.Context(), client.ObjectKey{Name: "acme"}, &cloudv1alpha1.Tenant{}); !apierrors.IsNotFound(err) {
				t.Fatalf("untrusted migration created CR: %v", err)
			}
		})
	}
}

func TestRetainedNamespaceAllowsOnlyProvenSameTenantRegistration(t *testing.T) {
	c := namespaceClient(t)
	obj := registration()
	if _, err := EnsureNamespace(t.Context(), c, obj); err != nil {
		t.Fatal(err)
	}
	if err := RetainNamespace(t.Context(), c, "acme"); err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "tenant-acme"}, ns); err != nil {
		t.Fatal(err)
	}
	if ns.Labels[LabelTenant] != "" || ns.Annotations[AnnotationRetainedTenant] != "acme" || len(ns.OwnerReferences) > 0 {
		t.Fatalf("retained namespace = %#v", ns)
	}
	version := ns.ResourceVersion
	if err := RetainNamespace(t.Context(), c, "acme"); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(t.Context(), client.ObjectKeyFromObject(ns), ns)
	if ns.ResourceVersion != version {
		t.Fatal("repeated retain wrote namespace")
	}
	obj.UID = "new-registration"
	if ns, err := EnsureNamespace(t.Context(), c, obj); err != nil || ns.Labels[LabelTenant] != "acme" || ns.Annotations[AnnotationRetainedTenant] != "" || ns.Annotations[AnnotationRegistration] != "new-registration" {
		t.Fatalf("re-registration = %#v, %v", ns, err)
	}
}

func TestTenantIDValidation(t *testing.T) {
	for _, id := range []string{"", "Upper", "a.b", "-bad", strings.Repeat("a", 57)} {
		if ValidID(id) {
			t.Errorf("valid invalid tenant ID %q", id)
		}
	}
	if !ValidID("tenant-42") || !ValidID(strings.Repeat("a", 56)) {
		t.Fatal("valid tenant IDs rejected")
	}
}
