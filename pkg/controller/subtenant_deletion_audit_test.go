package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/naas"
)

func newDeletionWorld(t *testing.T) *stWorld {
	t.Helper()
	w := newSubTenantWorld(t)
	for _, obj := range bootstrapObjects(t) {
		if obj.GetKind() != "GlobalTenantResource" {
			continue
		}
		if err := w.remote.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		obj.SetUID(types.UID("bootstrap-" + obj.GetName()))
		if err := w.remote.Update(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func TestSubTenantDeletionProvisionsAndOffboardsWithoutRecordingPrefixNeighbor(t *testing.T) {
	w := newDeletionWorld(t)
	sub := w.sub(t)
	sub.Spec.Name = "foo"
	if err := w.management.Update(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	sibling := w.sub(t).DeepCopy()
	sibling.Name = "00000000-0000-4000-8000-000000000022"
	sibling.UID = "sibling-subtenant-uid"
	sibling.Spec.Name = "foo-bar"
	sibling.Status = cloud.SubTenantStatus{}
	foreign, err := naas.RenderTenant(sibling, "1.35", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	foreign.SetUID("foreign-remote-uid")
	if err := w.remote.Create(t.Context(), foreign); err != nil {
		t.Fatal(err)
	}
	neighbor := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "foo-bar-app", UID: "neighbor-uid",
		Labels:          map[string]string{naas.LabelTenant: "acme", naas.LabelSubTenantID: sibling.Name, "capsule.clastix.io/tenant": foreign.GetName(), "cloud.cluster-vending.io/naas-ready": "true"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: naas.TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: foreign.GetName(), UID: foreign.GetUID()}},
	}}
	if err := w.remote.Create(t.Context(), neighbor); err != nil {
		t.Fatal(err)
	}
	// Exercise the normal case: the sibling already exists when the current
	// allocation is first observed. Its namespace must not poison our history.
	w.provision(t)
	for _, observed := range w.sub(t).Status.Namespaces {
		if observed.Name == neighbor.Name {
			t.Fatal("provisioning recorded a verified foreign prefix neighbor in our inventory")
		}
	}
	getRemote := func() *unstructured.Unstructured {
		t.Helper()
		remote := &unstructured.Unstructured{}
		remote.SetGroupVersionKind(naas.TenantGVK)
		if err := w.remote.Get(t.Context(), client.ObjectKey{Name: "foo"}, remote); err != nil {
			t.Fatal(err)
		}
		return remote
	}
	remote := getRemote()
	// Model Capsule creating the allocated namespace. No management writer
	// creates it, and no isolation/access readiness is synthesized.
	owned := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "foo-app", UID: "owned-namespace-uid",
		Labels:          map[string]string{naas.LabelTenant: "acme", naas.LabelSubTenantID: stID, "capsule.clastix.io/tenant": "foo", "cloud.cluster-vending.io/naas-ready": "false"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: naas.TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: "foo", UID: remote.GetUID()}},
	}}
	if err := w.remote.Create(t.Context(), owned); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	foundOwned := false
	for _, observed := range w.sub(t).Status.Namespaces {
		if observed.Name == neighbor.Name {
			t.Fatal("foreign neighbor poisoned observed history")
		}
		if observed.Name == owned.Name && observed.UID == string(owned.UID) {
			foundOwned = true
		}
	}
	if !foundOwned {
		t.Fatal("our actual Capsule namespace was not observed")
	}
	if err := w.management.Delete(t.Context(), w.sub(t)); err != nil {
		t.Fatal(err)
	}
	completed := false
	for range 12 {
		current := &cloud.SubTenant{}
		err := w.management.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: stID}, current)
		if apierrors.IsNotFound(err) {
			completed = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		// Simulate only Capsule's supported Tenant generation/cordon
		// observations. Leave all unrelated shared GTR versions unchanged.
		observed := &unstructured.Unstructured{}
		observed.SetGroupVersionKind(naas.TenantGVK)
		if err := w.remote.Get(t.Context(), client.ObjectKey{Name: "foo"}, observed); err == nil {
			observed.Object["status"] = map[string]any{"observedGeneration": observed.GetGeneration()}
			if err := w.remote.Update(t.Context(), observed); err != nil {
				t.Fatal(err)
			}
			if cordoned, _, _ := unstructured.NestedBool(observed.Object, "spec", "cordoned"); cordoned {
				namespace := &corev1.Namespace{}
				if err := w.remote.Get(t.Context(), client.ObjectKey{Name: owned.Name}, namespace); err == nil {
					namespace.Labels["projectcapsule.dev/cordoned"] = "true"
					if err := w.remote.Update(t.Context(), namespace); err != nil {
						t.Fatal(err)
					}
				} else if !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}
			}
		} else if !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		w.pass(t)
	}
	if !completed {
		t.Fatal("foo did not complete offboarding", w.sub(t).Status.Message)
	}
	if err := w.remote.Get(t.Context(), client.ObjectKey{Name: owned.Name}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatal("owned empty namespace survived cleanup", err)
	}
	remote = &unstructured.Unstructured{}
	remote.SetGroupVersionKind(naas.TenantGVK)
	if err := w.remote.Get(t.Context(), client.ObjectKey{Name: "foo"}, remote); !apierrors.IsNotFound(err) {
		t.Fatal("owned Tenant survived cleanup", err)
	}
	remaining := &corev1.Namespace{}
	if err := w.remote.Get(t.Context(), client.ObjectKeyFromObject(neighbor), remaining); err != nil || remaining.UID != neighbor.UID {
		t.Fatal("foreign namespace was changed/deleted", err)
	}
	remainingTenant := &unstructured.Unstructured{}
	remainingTenant.SetGroupVersionKind(naas.TenantGVK)
	if err := w.remote.Get(t.Context(), client.ObjectKeyFromObject(foreign), remainingTenant); err != nil || remainingTenant.GetUID() != foreign.GetUID() {
		t.Fatal("foreign Tenant was changed/deleted", err)
	}
}

func TestSubTenantDeletionDoesNotRequireUnchangedGTRStatusWrites(t *testing.T) {
	w := newDeletionWorld(t)
	w.provision(t)
	if err := w.management.Delete(t.Context(), w.sub(t)); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	w.pass(t)
	remote := w.tenant(t)
	remote.Object["status"] = map[string]any{"observedGeneration": remote.GetGeneration()}
	if err := w.remote.Update(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	// Shared GTRs were already Ready and never selected this access-disabled
	// Tenant. Do not synthesize GTR writes to make this test progress.
	w.pass(t)
	remote = w.tenant(t)
	remote.Object["status"] = map[string]any{"observedGeneration": remote.GetGeneration()}
	if err := w.remote.Update(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	w.pass(t)
	w.pass(t)
	if err := w.management.Get(t.Context(), client.ObjectKey{Namespace: "tenant-acme", Name: stID}, &cloud.SubTenant{}); !apierrors.IsNotFound(err) {
		t.Fatal("unchanged GTRs left deletion stuck", err)
	}
}
