package reconcile_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func TestFreezeSupportsProviderSuppliedManagedKinds(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	kind := schema.GroupVersionKind{Group: "container.gcp.m.upbound.io", Version: "v1beta1", Kind: "Cluster"}
	scheme.AddKnownTypeWithName(kind.GroupVersion().WithKind("ClusterList"), &unstructured.UnstructuredList{})
	resource := &unstructured.Unstructured{}
	resource.SetGroupVersionKind(kind)
	resource.SetName("gke")
	resource.SetNamespace("tenant-acme")
	resource.SetLabels(crossplane.ClusterLabels("acme", "gcp-cluster"))
	hold := &cloud.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: reconcile.FreezeName}, Spec: cloud.ReleaseHoldSpec{Platform: true, Reason: "maintenance", Revision: "provider-test"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(resource, hold).WithStatusSubresource(hold).Build()
	deps := reconcile.Deps{Kube: c, ManagedResourceKinds: []schema.GroupVersionKind{kind}}
	if _, err := reconcile.ConfirmFreeze(t.Context(), deps, metav1.Now().Time); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(resource), resource); err != nil {
		t.Fatal(err)
	}
	if !managed.IsPaused(resource) {
		t.Fatal("GCP provider kind was not paused")
	}
	if err := c.Delete(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile.ConfirmFreeze(t.Context(), deps, metav1.Now().Time); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(resource), resource); err != nil {
		t.Fatal(err)
	}
	if managed.IsPaused(resource) {
		t.Fatal("GCP provider kind did not resume")
	}
}
