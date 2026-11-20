package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// PROP-03 phase 2: the platform pool, built and empty.

const platformPool = crossplaneaws.PoolPlatform

// withPlatformPool turns the pool on, as the loaded config does.
func withPlatformPool(w world) {
	w.svc.deps.NodeGroups = config.DefaultNodeGroups()
}

// setObjectReady sets an object's Ready condition, with no node counted.
func setObjectReady(t *testing.T, fw *fakeWorkload, kind, name string, ready bool) {
	t.Helper()
	u := fw.get(t, kind, "", name)
	status := "False"
	if ready {
		status = "True"
	}
	u.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": status, "message": kind + " not ready"}}}
	require.NoError(t, fw.c.Update(t.Context(), u))
}

func TestPlatformPoolIsAppliedOnceKarpenterIsReadyWithoutUserGroups(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	pool, class := fw.get(t, "NodePool", "", platformPool), fw.get(t, "EC2NodeClass", "", platformPool)
	require.NotNil(t, pool)
	require.NotNil(t, class)
	require.Equal(t, crossplaneaws.ManagedByPlatform, pool.GetLabels()[crossplaneaws.LabelManagedBy])
	require.Equal(t, crossplaneaws.ManagedByPlatform, class.GetLabels()[crossplaneaws.LabelManagedBy])
	// The pool is reported as a platform-managed group of its own.
	st := res.platformPool
	require.NotNil(t, st)
	require.Equal(t, platformPool, st.GetName())
	require.True(t, st.GetPlatformManaged())
	require.Equal(t, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, st.GetPhase(), "Karpenter has not reported it yet")
	require.Empty(t, res.groups, "it is not one of the user groups")
}

// Nothing is applied, and the pool is PENDING, until Karpenter is READY.
func TestPlatformPoolWaitsForKarpenter(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING))

	require.Zero(t, fw.opened, "the workload cluster is not touched before Karpenter is Ready")
	require.Nil(t, fw.get(t, "NodePool", "", platformPool))
	require.NotNil(t, res.platformPool)
	require.Equal(t, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, res.platformPool.GetPhase())
	require.Contains(t, res.platformPool.GetMessage(), "waiting for Karpenter to be Ready")
}

// The class names Karpenter's node role: the pool waits for its IAM chain
// even when the cluster has no user groups.
func TestPlatformPoolWaitsForKarpenterIAM(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row, _ := nodeGroupWorldSrc(t, fw) // no IAM seeded
	withPlatformPool(w)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	require.Nil(t, fw.get(t, "NodePool", "", platformPool), "applied before Karpenter's IAM was ready")
	require.NotNil(t, res.platformPool)
	require.Contains(t, res.platformPool.GetMessage(), "waiting for Karpenter's IAM")
}

// Ready is the objects Ready, not nodes: a pool with no node is READY.
func TestPlatformPoolIsReadyWhenItsObjectsAreReadyWithNoNodes(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	passOf(w, row, karpenter)

	// Only the pool is Ready: the class is not, so the pool is not.
	setObjectReady(t, fw, "NodePool", platformPool, true)
	setObjectReady(t, fw, "EC2NodeClass", platformPool, false)
	res := passOf(w, row, karpenter)
	require.Equal(t, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, res.platformPool.GetPhase())
	require.Contains(t, res.platformPool.GetMessage(), "EC2NodeClass not ready")

	setObjectReady(t, fw, "EC2NodeClass", platformPool, true)
	setObjectReady(t, fw, "NodePool", platformPool, false)
	res = passOf(w, row, karpenter)
	require.Equal(t, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, res.platformPool.GetPhase())

	setObjectReady(t, fw, "NodePool", platformPool, true)
	res = passOf(w, row, karpenter)
	require.Equal(t, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, res.platformPool.GetPhase())
	require.Zero(t, res.platformPool.GetNodes(), "no pod needs a node yet: none exists")
	require.Empty(t, res.platformPool.GetMessage())
}

func TestPlatformPoolApplyFailureIsItsFailedStatus(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		if u, ok := obj.(interface{ GetName() string }); ok && u.GetName() == platformPool {
			return context.DeadlineExceeded
		}
		return c.Apply(ctx, obj, opts...)
	}})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})
	withPlatformPool(w)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	require.Equal(t, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED, res.platformPool.GetPhase())
	require.NotNil(t, fw.get(t, "NodePool", "", "gpu"), "the user group goes on")
}

// A pass that is not given the config renders no pool (the zero value).
func TestNoPlatformPoolWithoutItsConfig(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)

	res := passOf(w, row, karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY))

	require.Nil(t, fw.get(t, "NodePool", "", platformPool))
	require.Nil(t, res.platformPool)
}

// The pool is not a user group: removeNodeGroups must keep it although no
// spec group names it, and a changed object is put back.
func TestPlatformPoolIsKeptAndRevertedNotRemovedAsUnwanted(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw)
	withPlatformPool(w)
	karpenter := karpenterOf(cloudv1.ComponentPhase_COMPONENT_PHASE_READY)
	for range 3 {
		passOf(w, row, karpenter)
		require.NotNil(t, fw.get(t, "NodePool", "", platformPool), "removed as unwanted")
		require.NotNil(t, fw.get(t, "EC2NodeClass", "", platformPool))
	}
	require.NoError(t, fw.c.Delete(t.Context(), fw.get(t, "NodePool", "", platformPool)))
	passOf(w, row, karpenter)
	require.NotNil(t, fw.get(t, "NodePool", "", platformPool), "a deleted pool is put back")
}

// The legacy gate removes the old pool named "platform" and never the new
// "platform-pool", whatever the gate decides.
func TestLegacyGateLeavesThePlatformPoolAlone(t *testing.T) {
	require.NotEqual(t, legacyPlatformPool, platformPool)
	lw := newLegacyWorld(t)
	withPlatformPool(lw.world)
	// Not settled: the old pool stays; the new one is applied and stays.
	lw.pass(t, lw.platformComponents("base"))
	require.NotNil(t, lw.legacyObject(t, "NodePool"))
	require.NotNil(t, lw.fw.get(t, "NodePool", "", platformPool))
	// Settled: the old pool and its class go, the new ones stay.
	lw.pass(t, lw.platformComponents())
	lw.pass(t, lw.platformComponents())
	require.NoError(t, lw.fw.c.Delete(t.Context(), karpenterNode("old-1", "platform", true)), "the old node goes")
	res := lw.pass(t, lw.platformComponents())
	require.Nil(t, lw.legacyObject(t, "NodePool"), "the old pool is removed")
	require.Nil(t, lw.legacyObject(t, "EC2NodeClass"), "and its class")
	require.NotNil(t, lw.fw.get(t, "NodePool", "", platformPool), "the platform pool is not touched by the gate")
	require.NotNil(t, lw.fw.get(t, "EC2NodeClass", "", platformPool))
	require.NotNil(t, res.platformPool)
	for _, g := range res.groups {
		require.NotEqual(t, platformPool, g.GetName())
	}
}

// Nodes of the pool are counted under its name, and the pool's nodes do not
// count as the old pool's pods.
func TestPlatformPoolNodesAreCountedAndNotTheLegacyPoolsPods(t *testing.T) {
	lw := newLegacyWorld(t)
	withPlatformPool(lw.world)
	require.NoError(t, lw.fw.c.Create(t.Context(), karpenterNode("pp-1", platformPool, true)))
	require.NoError(t, lw.fw.c.Create(t.Context(), podOn("someone", "pp-1", "ReplicaSet")))
	lw.pass(t, lw.platformComponents()) // settles the old pool away
	res := lw.pass(t, lw.platformComponents())
	require.Equal(t, int32(1), res.platformPool.GetNodes())
	require.NotContains(t, res.system.move, "pods on its nodes")
}

// The API shows the pool's objects first, as the pass applies them.
func TestNodeGroupObjectsShowThePlatformPoolFirst(t *testing.T) {
	fw := newFakeWorkload(interceptor.Funcs{})
	w, row := nodeGroupWorld(t, fw, &cloudv1.NodeGroupSpec{Name: "gpu"})
	withPlatformPool(w)
	row.Status = mustStatus(t, &cloudv1.ClusterStatus{PlatformRelease: "2026-11-a"})

	shown, err := w.svc.NodeGroupObjects(t.Context(), row)

	require.NoError(t, err)
	var names []string
	for _, o := range shown {
		names = append(names, o.GetKind()+"/"+o.GetName())
	}
	require.Equal(t, "EC2NodeClass/platform-pool NodePool/platform-pool EC2NodeClass/gpu NodePool/gpu", strings.Join(names, " "))
}

// The status lists the platform's groups first: system, then platform-pool.
func TestStatusListsThePlatformPoolAfterTheSystemGroup(t *testing.T) {
	ctx := withNodeGroupsHolder(context.Background())
	holder, _ := ctx.Value(nodeGroupsKey{}).(*nodeGroupsHolder)
	holder.r = &nodeGroupsResult{
		groups:       []*cloudv1.NodeGroupStatus{groupStatus("gpu", false, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, "", 1)},
		platformPool: groupStatus(platformPool, true, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, "", 0)}
	st := &cloudv1.ClusterStatus{}

	withNodeGroups(ctx, st, &cloudv1.ClusterStatus{})

	var names []string
	for _, g := range st.GetNodeGroups() {
		names = append(names, g.GetName())
	}
	require.Equal(t, []string{systemNodeGroup, platformPool, "gpu"}, names)

	// Without a pass the recorded pool stays, and a user group is not
	// confused with it.
	stored := &cloudv1.ClusterStatus{NodeGroups: st.GetNodeGroups()}
	again := &cloudv1.ClusterStatus{}
	withNodeGroups(context.Background(), again, stored)
	names = nil
	for _, g := range again.GetNodeGroups() {
		names = append(names, g.GetName())
	}
	require.Equal(t, []string{systemNodeGroup, platformPool, "gpu"}, names)
}
