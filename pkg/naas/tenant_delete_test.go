package naas

import (
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func withdrawalFixture(t *testing.T) (client.Client, *unstructured.Unstructured) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(TenantGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(TenantGVK.GroupVersion().WithKind("GlobalTenantResource"), &unstructured.Unstructured{})
	var objects []client.Object
	for _, obj := range mustLoad(t).Objects() {
		obj.SetUID(types.UID("bootstrap-" + obj.GetName()))
		obj.SetGeneration(1)
		if obj.GetKind() == "GlobalTenantResource" {
			obj.Object["status"] = map[string]any{"observedGeneration": int64(1), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
		}
		objects = append(objects, obj)
	}
	remote, err := RenderTenant(allocationFixture(), "1.35", "revision")
	if err != nil {
		t.Fatal(err)
	}
	remote.SetUID("remote-uid")
	remote.SetGeneration(1)
	remote = WithdrawTenantGeneration(remote)
	remote.Object["status"] = map[string]any{"observedGeneration": remote.GetGeneration()}
	objects = append(objects, remote)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(remote), remote); err != nil {
		t.Fatal(err)
	}
	fence, err := CaptureTenantDeletionFence(t.Context(), c, remote)
	if err != nil {
		t.Fatal(err)
	}
	annotations := remote.GetAnnotations()
	annotations[DeletionFenceAnnotation] = fence
	remote.SetAnnotations(annotations)
	if err := c.Update(t.Context(), remote); err != nil {
		t.Fatal(err)
	}
	return c, remote
}

func TestTenantDeletionAcknowledgesUnchangedGTRStatus(t *testing.T) {
	c, remote := withdrawalFixture(t)
	before, err := tenantGTRs(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := TenantDeletionAcknowledged(t.Context(), c, remote); err != nil {
			t.Fatal("unchanged, deselected clean GTR did not acknowledge", err)
		}
	}
	after, err := tenantGTRs(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	for i := range before {
		if before[i].GetResourceVersion() != after[i].GetResourceVersion() {
			t.Fatal("acknowledgement mutated a shared GTR")
		}
	}
}

func TestTenantDeletionAcknowledgementIsTenantAndIdentityBound(t *testing.T) {
	for _, test := range []string{"selected", "processed", "unattributed", "stale-tenant", "stale-gtr", "foreign-tenant", "replaced-gtr", "reopened-generation", "legacy-rv", "other-tenant-items"} {
		t.Run(test, func(t *testing.T) {
			c, remote := withdrawalFixture(t)
			gtrs, err := tenantGTRs(t.Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			target := gtrs[0]
			switch test {
			case "selected":
				_ = unstructured.SetNestedStringSlice(target.Object, []string{remote.GetName()}, "status", "selectedTenants")
			case "processed":
				_ = unstructured.SetNestedSlice(target.Object, []any{map[string]any{"tenant": remote.GetName(), "name": "tracked"}}, "status", "processedItems")
			case "unattributed":
				_ = unstructured.SetNestedSlice(target.Object, []any{map[string]any{"name": "unknown"}}, "status", "processedItems")
			case "other-tenant-items":
				_ = unstructured.SetNestedSlice(target.Object, []any{map[string]any{"tenant": "some-other-tenant", "name": "tracked"}}, "status", "processedItems")
			case "stale-tenant":
				_ = unstructured.SetNestedField(remote.Object, remote.GetGeneration()-1, "status", "observedGeneration")
			case "stale-gtr":
				_ = unstructured.SetNestedField(target.Object, int64(0), "status", "observedGeneration")
			case "foreign-tenant":
				remote.SetUID("replacement-uid")
			case "replaced-gtr":
				target.SetUID("replacement-gtr")
			case "reopened-generation":
				_ = unstructured.SetNestedSlice(remote.Object, []any{"app"}, "spec", "data", "naas", "namespaceKeys")
			case "legacy-rv":
				a := remote.GetAnnotations()
				a[DeletionFenceAnnotation] = fmt.Sprintf(`{%q:"999"}`, target.GetName())
				remote.SetAnnotations(a)
			}
			if err := c.Update(t.Context(), target); err != nil {
				t.Fatal(err)
			}
			err = TenantDeletionAcknowledged(t.Context(), c, remote)
			if test == "other-tenant-items" {
				if err != nil {
					t.Fatal("unrelated Tenant items block this withdrawal", err)
				}
			} else if err == nil {
				t.Fatal("unsafe acknowledgement accepted")
			}
		})
	}
}

func TestTenantWithdrawalPreservesAppliedConfiguration(t *testing.T) {
	_, remote := withdrawalFixture(t)
	applied, err := DeletionAppliedTenant(remote)
	if err != nil {
		t.Fatal(err)
	}
	keys, _, _ := unstructured.NestedStringSlice(applied.Object, "spec", "data", "naas", "namespaceKeys")
	if len(keys) != 1 || keys[0] != "app" {
		t.Fatal("applied keys were lost", keys)
	}
	liveKeys, _, _ := unstructured.NestedSlice(remote.Object, "spec", "data", "naas", "namespaceKeys")
	if len(liveKeys) != 0 {
		t.Fatal("generation still active")
	}
	first := remote.GetAnnotations()[DeletionAppliedSpecAnnotation]
	again := WithdrawTenantGeneration(remote)
	if again.GetAnnotations()[DeletionAppliedSpecAnnotation] != first {
		t.Fatal("retry overwrote applied snapshot")
	}
	again.SetUID("foreign")
	if _, err := DeletionAppliedTenant(again); err == nil {
		t.Fatal("accepted another Tenant's snapshot")
	}
}
