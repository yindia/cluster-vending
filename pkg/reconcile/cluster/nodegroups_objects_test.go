package cluster

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The objects the API shows (NodeGroupObjects) are the ones a pass applies:
// the platform pool's class and pool.
func TestNodeGroupObjectsAreWhatThePassApplies(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{PlatformRelease: "2026-11-a"})

	passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))
	shown, err := w.svc.NodeGroupObjects(t.Context(), row)
	require.NoError(t, err)
	require.Len(t, shown, 2, "the platform pool's class and pool")

	for _, o := range shown {
		require.Equal(t, platformPool, o.GetName())
		got := fw.get(t, o.GetKind(), "", o.GetName())
		require.NotNil(t, got, "%s/%s", o.GetKind(), o.GetName())
		require.Equal(t, o.Object["spec"], got.Object["spec"], "%s/%s", o.GetKind(), o.GetName())
		require.Equal(t, o.GetLabels(), got.GetLabels())
	}
	// The AMI pin of the release is in the class.
	require.Contains(t, shown[0].Object["spec"].(map[string]any)["amiSelectorTerms"].([]any)[0], "alias")
}

func TestNodeGroupObjectsOfAnImportedClusterAreNone(t *testing.T) {
	w, row := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}))
	withPlatformPool(w)
	row.Imported, row.State = true, StateImported
	objs, err := w.svc.NodeGroupObjects(t.Context(), row)
	require.NoError(t, err)
	require.Empty(t, objs)
}

func TestNodeGroupObjectsWithoutAReleaseSourceUseTheDefaultPin(t *testing.T) {
	w, row := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}))
	withPlatformPool(w)
	w.svc.deps.Platform = nil
	w.svc.deps.Releases = nil
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{PlatformRelease: "2026-11-a"})
	objs, err := w.svc.NodeGroupObjects(t.Context(), row)
	require.NoError(t, err, "without a release source the pin is the default, not an error")
	require.Len(t, objs, 2)
}

func TestNodeGroupObjectsWithoutAPlatformPoolAreNone(t *testing.T) {
	w, row := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}))
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{PlatformRelease: "2026-11-a"})
	objs, err := w.svc.NodeGroupObjects(t.Context(), row)
	require.NoError(t, err)
	require.Empty(t, objs)
}
