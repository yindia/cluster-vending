package cluster

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// Karpenter's stage is fixed (it is removed after the node groups): it is
// sequence 0 by name, whatever a catalog says.
func TestKarpenterIsSequenceZeroByNameFirst(t *testing.T) {
	o := definitionOrder{sequence: map[string]int32{karpenterComponent: 50, "base": 100}}

	require.Equal(t, int32(0), o.sequenceOf(karpenterComponent))
	require.Equal(t, int32(100), o.sequenceOf("base"))
	require.Equal(t, int32(100), o.sequenceOf("unknown"))
}

// The sweep of what a deletion left finds the Helm Releases and the
// ProviderConfig of the cluster that no record names, and no other
// cluster's.
func TestSweepFindsUnrecordedHelmObjects(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	obj := func(kind, name, cluster string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK.GroupVersion().WithKind(kind))
		u.SetNamespace(worldNS)
		u.SetName(name)
		u.SetLabels(map[string]string{crossplane.LabelCluster: cluster})
		return u
	}
	mine, theirs, pc := obj("Release", "leftover", w.row.ID), obj("Release", "other-cluster", "c-other"), obj("ProviderConfig", "leftover-helm", w.row.ID)
	for _, u := range []*unstructured.Unstructured{mine, theirs, pc} {
		require.NoError(t, w.kube.Create(t.Context(), u))
	}

	found, err := w.svc.deleteUnrecorded(t.Context(), w.cluster(t), worldNS)

	require.NoError(t, err)
	require.GreaterOrEqual(t, found, 2, "the Release and the ProviderConfig, besides the cluster's own objects")
	for _, u := range []*unstructured.Unstructured{mine, pc} {
		err := w.kube.Get(t.Context(), client.ObjectKeyFromObject(u), u.DeepCopy())
		require.Error(t, err, "%s %s was deleted", u.GetKind(), u.GetName())
	}
	require.NoError(t, w.kube.Get(t.Context(), client.ObjectKeyFromObject(theirs), theirs.DeepCopy()), "another cluster's Release stays")
}

// The node group stage's memory of a failing API goes whenever the stage is
// left, not only when it ran to its end; and the deletion's end forgets what
// the passes remembered.
func TestDeleteClocksArePrunedOnEveryExit(t *testing.T) {
	s := New(reconcile.Deps{})
	row := newDelOrder(t).row
	for name, run := range map[string]func(){
		"not reachable": func() { s.deleteNodeGroups(t.Context(), row, &clusterRemoval{}, false) },
		"no teardowns":  func() { s.deleteNodeGroups(t.Context(), row, &clusterRemoval{}, true) },
	} {
		nodeStageErrors.Store(row.ID, time.Now())
		run()
		_, left := nodeStageErrors.Load(row.ID)
		require.False(t, left, name)
	}

	d := newDelOrder(t)
	nodeStageErrors.Store(d.row.ID, time.Now())
	apiDownSince.Store(d.row.ID, time.Now())
	d.run(80)
	require.True(t, d.gone())
	_, errs := nodeStageErrors.Load(d.row.ID)
	_, down := apiDownSince.Load(d.row.ID)
	require.False(t, errs || down, "the finished deletion forgot its clocks")
}
