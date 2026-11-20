package cluster

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The objects the API shows (NodeGroupObjects) are the ones a pass applies.
func TestNodeGroupObjectsAreWhatThePassApplies(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	group := &cloudv1.NodeGroupSpec{Name: "gpu", InstanceFamilies: []string{"g5"}, Labels: map[string]string{"a": "b"}}
	w, row := nodeGroupWorld(t, fw, group)
	w.svc.deps.NodeGroups.Karpenter.InstanceFamilies = []string{"m7i", "c7i"} // checked by the server, not by the render
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{PlatformRelease: "2026-11-a"})

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	shown, err := w.svc.NodeGroupObjects(t.Context(), row)
	require.NoError(t, err)
	require.Len(t, shown, 2, "a class and a pool for the one user group: no platform group")

	for _, o := range shown {
		got := fw.get(t, o.GetKind(), "", o.GetName())
		require.NotNil(t, got, "%s/%s", o.GetKind(), o.GetName())
		require.Equal(t, o.Object["spec"], got.Object["spec"], "%s/%s", o.GetKind(), o.GetName())
		require.Equal(t, o.GetLabels(), got.GetLabels())
	}
	pool := shown[1]
	reqs, _, _ := nestedRequirements(pool.Object)
	require.Contains(t, reqs, "karpenter.k8s.aws/instance-family")
	require.Equal(t, []any{"g5"}, requirementValues(pool.Object, "karpenter.k8s.aws/instance-family"))
	// The AMI pin of the release is in the class.
	require.Contains(t, shown[0].Object["spec"].(map[string]any)["amiSelectorTerms"].([]any)[0], "alias")
}

func TestNodeGroupObjectsOfAnImportedClusterAreNone(t *testing.T) {
	w, row := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}))
	row.Imported, row.State = true, StateImported
	objs, err := w.svc.NodeGroupObjects(t.Context(), row)
	require.NoError(t, err)
	require.Empty(t, objs)
}

func TestNodeGroupObjectsWithoutAReleaseSourceUseTheDefaultPin(t *testing.T) {
	w, row := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}), &cloudv1.NodeGroupSpec{Name: "gpu"})
	w.svc.deps.Platform = nil
	w.svc.deps.Releases = nil
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{PlatformRelease: "2026-11-a"})
	objs, err := w.svc.NodeGroupObjects(t.Context(), row)
	require.NoError(t, err, "without a release source the pin is the default, not an error")
	require.Len(t, objs, 2)
}

func TestNodeGroupObjectsWithoutUserGroupsAreNone(t *testing.T) {
	w, row := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}))
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{PlatformRelease: "2026-11-a"})
	objs, err := w.svc.NodeGroupObjects(t.Context(), row)
	require.NoError(t, err)
	require.Empty(t, objs, "the platform runs on the system node group: nothing is applied")
}

func nestedRequirements(obj map[string]any) (map[string]bool, bool, error) {
	out := map[string]bool{}
	spec, _ := obj["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	ts, _ := tpl["spec"].(map[string]any)
	reqs, _ := ts["requirements"].([]any)
	for _, r := range reqs {
		out[r.(map[string]any)["key"].(string)] = true
	}
	return out, true, nil
}

func requirementValues(obj map[string]any, key string) any {
	spec, _ := obj["spec"].(map[string]any)
	tpl, _ := spec["template"].(map[string]any)
	ts, _ := tpl["spec"].(map[string]any)
	reqs, _ := ts["requirements"].([]any)
	for _, r := range reqs {
		if m, ok := r.(map[string]any); ok && m["key"] == key {
			return m["values"]
		}
	}
	return nil
}
