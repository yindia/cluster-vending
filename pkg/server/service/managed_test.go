package service

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// managed is a managed resource with the given conditions (type, status,
// message).
func managed(conds ...[3]string) *unstructured.Unstructured {
	list := []any{}
	for _, c := range conds {
		list = append(list, map[string]any{"type": c[0], "status": c[1], "reason": "R", "message": c[2]})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "x", "annotations": map[string]any{"crossplane.io/external-name": "rtb-1"},
			"labels": map[string]any{"cluster-vending.io/zone": "use1-az1"}},
		"spec":   map[string]any{"forProvider": map[string]any{}},
		"status": map[string]any{"conditions": list},
	}}
}

func TestViewStates(t *testing.T) {
	deleting := managed([3]string{"Ready", "True", ""})
	deleting.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	providerConfig := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "aws-1"}, "spec": map[string]any{}}}
	for name, tc := range map[string]struct {
		obj  *unstructured.Unstructured
		want cloudv1.ManagedResourceState
	}{
		"ready":           {managed([3]string{"Ready", "True", ""}, [3]string{"Synced", "True", ""}), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY},
		"waiting":         {managed([3]string{"Synced", "False", "cannot resolve references: mg.Spec.ForProvider.NATGatewayID"}), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING},
		"failed":          {managed([3]string{"Ready", "False", ""}, [3]string{"Synced", "False", "UnauthorizedOperation"}), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED},
		"creating":        {managed([3]string{"Ready", "False", "Creating"}, [3]string{"Synced", "True", ""}), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING},
		"not seen yet":    {managed(), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING},
		"deleting":        {deleting, cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING},
		"provider config": {providerConfig, cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY},
	} {
		if got := view(model.ManagedResource{}, tc.obj); got.GetState() != tc.want {
			t.Errorf("%s: state = %s, want %s", name, got.GetState(), tc.want)
		}
	}
	v := view(model.ManagedResource{Kind: "RouteTable", Name: "rt"}, managed([3]string{"Synced", "False", "UnauthorizedOperation"}))
	if v.GetKind() != "RouteTable" || v.GetName() != "rt" || v.GetExternalName() != "rtb-1" || v.GetZoneId() != "use1-az1" ||
		v.GetMessage() != "UnauthorizedOperation" || v.GetSynced() != cloudv1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Errorf("view = %v", v)
	}
}

func TestObserveMissingObjects(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme"}}
	d := Deps{Kube: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()}
	views, err := d.Observe(t.Context(), []model.ManagedResource{
		{APIVersion: "v1", Kind: "Namespace", Name: "tenant-acme"},
		{APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "VPC", Namespace: "tenant-acme", Name: "gone"},
		{APIVersion: "example.io/v1", Kind: "Unknown", Namespace: "tenant-acme", Name: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []cloudv1.ManagedResourceState{
		cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY,
		cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING,
		cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING,
	}
	for i, w := range want {
		if views[i].GetState() != w {
			t.Errorf("view %d (%s) = %s, want %s", i, views[i].GetKind(), views[i].GetState(), w)
		}
	}
}

func TestFreshErrorsAreRetried(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	failing := func(since time.Time) *unstructured.Unstructured {
		u := managed()
		_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Synced", "status": "False", "reason": "ReconcileError",
			"message": "ProviderConfigUsage already exists", "lastTransitionTime": since.Format(time.RFC3339)}}, "status", "conditions")
		return u
	}
	fresh := view(model.ManagedResource{}, failing(at.Add(-30*time.Second)))
	if fresh.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING ||
		fresh.GetMessage() != "retrying: ProviderConfigUsage already exists" {
		t.Errorf("30s old error = %s %q, want CREATING retrying", fresh.GetState(), fresh.GetMessage())
	}
	if old := view(model.ManagedResource{}, failing(at.Add(-5*time.Minute))); old.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED {
		t.Errorf("5m old error = %s, want FAILED", old.GetState())
	}
}
