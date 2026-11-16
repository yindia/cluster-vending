package planning

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func resource(apiVersion, namespace, name string) *unstructured.Unstructured {
	body := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"forProvider": map[string]any{"initialNodeCount": int64(2)}}}}
	body.SetAPIVersion(apiVersion)
	body.SetKind("Cluster")
	body.SetNamespace(namespace)
	body.SetName(name)
	return body
}

func TestProviderPolicyUsesFullGVK(t *testing.T) {
	live := resource("container.gcp.m.upbound.io/v1beta1", "tenant-acme", "shared")
	dry := live.DeepCopy()
	if err := unstructured.SetNestedField(dry.Object, int64(3), "spec", "forProvider", "initialNodeCount"); err != nil {
		t.Fatal(err)
	}
	unknown := FieldChanges(live, dry, Policy{})
	if len(unknown) != 1 || unknown[0].Action != v1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE {
		t.Fatal(unknown)
	}
	policy := Policy{Classify: func(kind schema.GroupVersionKind, path string, _ bool) v1.PlannedAction {
		if kind.Group == "container.gcp.m.upbound.io" && path == "initialNodeCount" {
			return v1.PlannedAction_PLANNED_ACTION_DISRUPTIVE
		}
		return v1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE
	}}
	changes := FieldChanges(live, dry, policy)
	if len(changes) != 1 || changes[0].Action != v1.PlannedAction_PLANNED_ACTION_DISRUPTIVE {
		t.Fatal(changes)
	}
}

func TestInventoryRemovalDoesNotConfuseProvidersAndBindsLiveVersions(t *testing.T) {
	aws := resource("eks.aws.m.upbound.io/v1beta1", "tenant-acme", "shared")
	gcp := resource("container.gcp.m.upbound.io/v1beta1", "tenant-acme", "shared")
	gcp.SetAnnotations(map[string]string{managed.RetainAnnotation: "true"})
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(aws, gcp).Build()
	engine := Snapshot{Client: c}
	entry := Entry{Ref: managed.Ref{APIVersion: gcp.GetAPIVersion(), Kind: gcp.GetKind(), Namespace: gcp.GetNamespace(), Name: gcp.GetName()}}
	plan := &v1.Plan{}
	versions, err := engine.Removals(t.Context(), []Entry{entry}, map[string]bool{ObjectKey(aws.GetAPIVersion(), aws.GetKind(), aws.GetNamespace(), aws.GetName()): true}, plan)
	if err != nil || len(plan.Changes) != 1 || !plan.Changes[0].RetainsCloudResource || plan.Changes[0].ApiVersion != gcp.GetAPIVersion() {
		t.Fatalf("cross-provider removal: %+v %v", plan, err)
	}
	first, err := ID(&v1.UpdateNetworkRequest{}, versions)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(gcp), gcp); err != nil {
		t.Fatal(err)
	}
	gcp.SetAnnotations(map[string]string{managed.RetainAnnotation: "true", "changed": "true"})
	if err := c.Update(t.Context(), gcp); err != nil {
		t.Fatal(err)
	}
	versions, err = engine.Removals(t.Context(), []Entry{entry}, nil, &v1.Plan{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ID(&v1.UpdateNetworkRequest{}, versions)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("removal target changed without invalidating plan")
	}
}
