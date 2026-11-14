package managed

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// eks is a ready EKS object of kind with spec.forProvider and
// status.atProvider set as given.
func eks(kind string, forProvider, atProvider map[string]any) *unstructured.Unstructured {
	u := managed([3]string{"Ready", "True", ""}, [3]string{"Synced", "True", ""})
	u.SetAPIVersion("eks.aws.m.upbound.io/v1beta1")
	u.SetKind(kind)
	u.Object["spec"] = map[string]any{"forProvider": forProvider}
	status := u.Object["status"].(map[string]any)
	status["atProvider"] = atProvider
	return u
}

// release is a ready Helm Release (provider-helm) at generation, whose
// Synced condition the provider wrote at observed, in Helm state.
func release(generation, observed int64, state string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "cluster-autoscaler", "generation": generation},
		"spec":     map[string]any{"forProvider": map[string]any{"chart": map[string]any{"version": "9.60.0"}}},
		"status": map[string]any{
			"atProvider": map[string]any{"state": state},
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True"},
				map[string]any{"type": "Synced", "status": "True", "observedGeneration": observed},
			},
		},
	}}
	u.SetAPIVersion("helm.m.crossplane.io/v1beta1")
	u.SetKind("Release")
	return u
}

// A platform component's Release is UPDATING while Helm applies a new
// chart version or values: the provider has not taken the new spec yet,
// or Helm reports a pending state.
func helmCases() []struct {
	name        string
	obj         *unstructured.Unstructured
	want        cloudv1.ManagedResourceState
	wantMessage string
} {
	return []struct {
		name        string
		obj         *unstructured.Unstructured
		want        cloudv1.ManagedResourceState
		wantMessage string
	}{
		{"release deployed at its spec", release(3, 3, "deployed"), cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, ""},
		{"release with a new spec not applied yet", release(4, 3, "deployed"),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING, "updating: applying chart 9.60.0 with new values"},
		{"release Helm is upgrading", release(4, 4, "pending-upgrade"),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING, "updating: Helm is pending-upgrade"},
		{"release whose provider writes no observed generation", release(4, 0, "deployed"),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, ""},
	}
}

// A ready object the provider reports at another version than its spec
// asks for (a node group whose upgrade EKS runs) is UPDATING, not READY.
func TestViewOfAnObjectChangingVersion(t *testing.T) {
	tests := []struct {
		name        string
		obj         *unstructured.Unstructured
		want        cloudv1.ManagedResourceState
		wantMessage string
	}{
		{"node group behind its spec", eks("NodeGroup", map[string]any{"version": "1.37"}, map[string]any{"version": "1.36", "status": "ACTIVE"}),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING, "updating: 1.36 → 1.37"},
		{"node group on its spec", eks("NodeGroup", map[string]any{"version": "1.37"}, map[string]any{"version": "1.37", "status": "ACTIVE"}),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, ""},
		{"patch version reported", eks("NodeGroup", map[string]any{"version": "1.37"}, map[string]any{"version": "1.37.2"}),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, ""},
		{"EKS says UPDATING", eks("NodeGroup", map[string]any{"version": "1.37"}, map[string]any{"version": "1.37", "status": "UPDATING"}),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING, "updating"},
		{"control plane behind its spec", eks("Cluster", map[string]any{"version": "1.37"}, map[string]any{"version": "1.36", "status": "ACTIVE"}),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING, "updating: 1.36 → 1.37"},
		{"add-on behind its spec", eks("Addon", map[string]any{"addonVersion": "v1.20.0-eksbuild.1"}, map[string]any{"addonVersion": "v1.19.0-eksbuild.4"}),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING, "updating: v1.19.0-eksbuild.4 → v1.20.0-eksbuild.1"},
		{"add-on with no version asked", eks("Addon", map[string]any{}, map[string]any{"addonVersion": "v1.19.0-eksbuild.4"}),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, ""},
		{"nothing reported yet", eks("NodeGroup", map[string]any{"version": "1.37"}, map[string]any{}),
			cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, ""},
	}
	tests = append(tests, helmCases()...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := View(Ref{Kind: tt.obj.GetKind()}, tt.obj)

			if v.GetState() != tt.want || v.GetMessage() != tt.wantMessage {
				t.Errorf("state = %s %q, want %s %q", v.GetState(), v.GetMessage(), tt.want, tt.wantMessage)
			}
		})
	}
}
