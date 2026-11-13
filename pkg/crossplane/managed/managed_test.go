package managed

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
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
		if got := View(Ref{}, tc.obj); got.GetState() != tc.want {
			t.Errorf("%s: state = %s, want %s", name, got.GetState(), tc.want)
		}
	}
	v := View(Ref{Kind: "RouteTable", Name: "rt"}, managed([3]string{"Synced", "False", "UnauthorizedOperation"}))
	if v.GetKind() != "RouteTable" || v.GetName() != "rt" || v.GetExternalName() != "rtb-1" || v.GetZoneId() != "use1-az1" ||
		v.GetMessage() != "UnauthorizedOperation" || v.GetSynced() != cloudv1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Errorf("view = %v", v)
	}
}

func TestFreshErrorsAreRetried(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	// The condition always looks fresh: every retry carries a new AWS
	// request ID, so lastTransitionTime is "now". Only the object's age
	// counts.
	failing := func(created time.Time) *unstructured.Unstructured {
		u := managed()
		u.SetCreationTimestamp(metav1.NewTime(created))
		_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Synced", "status": "False", "reason": "ReconcileError",
			"message": "ProviderConfigUsage already exists", "lastTransitionTime": at.Format(time.RFC3339)}}, "status", "conditions")
		return u
	}
	fresh := View(Ref{}, failing(at.Add(-30*time.Second)))
	if fresh.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING ||
		fresh.GetMessage() != "retrying: ProviderConfigUsage already exists" {
		t.Errorf("30s old object = %s %q, want CREATING retrying", fresh.GetState(), fresh.GetMessage())
	}
	if old := View(Ref{}, failing(at.Add(-6*time.Hour))); old.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED {
		t.Errorf("6h old object failing with a fresh condition = %s, want FAILED", old.GetState())
	}
}

// A paused resource (crossplane.io/paused) reports Synced=False with the
// reason ReconcilePaused. That is an operator's choice, not a failure: it
// must never turn its network or cluster FAILED, however long it lasts.
func TestViewOfPausedResources(t *testing.T) {
	paused := func(u *unstructured.Unstructured) *unstructured.Unstructured {
		a := u.GetAnnotations()
		a[PausedAnnotation] = "true"
		u.SetAnnotations(a)
		// Long past the grace a fresh error gets.
		u.SetCreationTimestamp(metav1.Time{Time: time.Now().Add(-24 * time.Hour)})
		return u
	}
	deleting := paused(managed([3]string{"Ready", "True", ""}, [3]string{"Synced", "False", ""}))
	deleting.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	tests := []struct {
		name   string
		obj    *unstructured.Unstructured
		want   cloudv1.ManagedResourceState
		paused bool
	}{
		{"ready and paused stays ready", paused(managed([3]string{"Ready", "True", ""}, [3]string{"Synced", "False", ""})),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, true},
		{"not ready and paused waits", paused(managed([3]string{"Ready", "False", ""}, [3]string{"Synced", "False", ""})),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING, true},
		{"paused before the provider saw it", paused(managed()), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING, true},
		{"deleting and paused is still deleting", deleting, cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING, true},
		{"not paused fails as before", func() *unstructured.Unstructured {
			u := managed([3]string{"Ready", "False", ""}, [3]string{"Synced", "False", "UnauthorizedOperation"})
			u.SetCreationTimestamp(metav1.Time{Time: time.Now().Add(-24 * time.Hour)})
			return u
		}(), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := View(Ref{Kind: "NodeGroup", Name: "ng"}, tt.obj)

			if v.GetState() != tt.want || v.GetPaused() != tt.paused {
				t.Fatalf("state %s paused %v (%s), want %s paused %v", v.GetState(), v.GetPaused(), v.GetMessage(), tt.want, tt.paused)
			}
			if tt.paused && v.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY && v.GetReason() != ReasonPaused {
				t.Errorf("reason %q, want %s", v.GetReason(), ReasonPaused)
			}
		})
	}
}
