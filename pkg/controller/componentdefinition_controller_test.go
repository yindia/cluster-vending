package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/component"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func definitionClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&cloudv1alpha1.ComponentDefinition{}).Build()
}

func catalogEntry(name string, spec cloudv1alpha1.ComponentDefinitionSpec) *cloudv1alpha1.ComponentDefinition {
	spec.Description = "The " + name + " component."
	return &cloudv1alpha1.ComponentDefinition{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
}

// A definition keeps a finalizer, says what uses it, and goes once nothing
// does: a tenant Component holds it; a cluster that has a Platform one
// installed holds it until the cluster removed it.
func TestComponentDefinitionInUse(t *testing.T) {
	ctx := t.Context()
	scanner := catalogEntry("scanner", cloudv1alpha1.ComponentDefinitionSpec{})
	lb := catalogEntry("lb", cloudv1alpha1.ComponentDefinitionSpec{Management: cloudv1alpha1.ComponentManagementPlatform})
	use := &cloudv1alpha1.Component{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "prod-scanner",
		Labels: map[string]string{"cloud.cluster-vending.io/tenant": "acme"}},
		Spec: cloudv1alpha1.ComponentSpec{ClusterID: "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33", Component: "scanner"}}
	cluster := &cloudv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "prod"},
		Status: cloudv1alpha1.ClusterStatus{PlatformComponents: []cloudv1alpha1.PlatformComponentStatus{{Name: "lb", Phase: "Ready"}}}}
	c := definitionClient(t, scanner, lb, use, cluster)
	r := &ComponentDefinitionReconciler{c: c}

	for _, name := range []string{"scanner", "lb"} {
		reconcileOnce(t, r, name)
		got := &cloudv1alpha1.ComponentDefinition{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatal(err)
		}
		if !controllerutil.ContainsFinalizer(got, component.DefinitionFinalizer) {
			t.Fatalf("%s has no finalizer", name)
		}
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionDefinitionInUse)
		if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "prod") {
			t.Errorf("%s InUse = %+v", name, cond)
		}
		if err := c.Delete(ctx, got); err != nil {
			t.Fatal(err)
		}
		reconcileOnce(t, r, name)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, got); err != nil {
			t.Fatalf("%s went while in use: %v", name, err)
		}
		cond = meta.FindStatusCondition(got.Status.Conditions, ConditionDefinitionInUse)
		if cond == nil || cond.Reason != "DeletionBlocked" {
			t.Errorf("%s while deleting: %+v", name, cond)
		}
	}

	if err := c.Delete(ctx, use); err != nil {
		t.Fatal(err)
	}
	cluster.Status.PlatformComponents = nil
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"scanner", "lb"} {
		reconcileOnce(t, r, name)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &cloudv1alpha1.ComponentDefinition{}); err == nil {
			t.Errorf("%s is still there once nothing uses it", name)
		}
	}
}
