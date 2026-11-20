package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func holdResult(ctx context.Context, r *nodeGroupsResult) {
	ctx.Value(nodeGroupsKey{}).(*nodeGroupsHolder).r = r
}

// status.node_groups lists the system group first (platform managed), its
// phase from the NodeGroup managed resource, then the user groups.
func TestSystemNodeGroupIsFirstWithThePhaseOfItsManagedResource(t *testing.T) {
	user := []*cloudv1.NodeGroupStatus{{Name: "gpu", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 4}}
	ctx := withNodeGroupsHolder(context.Background())
	holdResult(ctx, &nodeGroupsResult{groups: user, system: systemObserved{nodes: 2, counted: true}})
	for _, tt := range []struct {
		name    string
		state   cloudv1.ManagedResourceState
		message string
		phase   cloudv1.NodeGroupPhase
	}{
		{"ready", cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, "ignored", cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY},
		{"waiting", cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING, "creating", cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING},
		{"failed", cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, "boom", cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_FAILED},
		{"deleting", cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING, "", cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_DELETING},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := &cloudv1.ClusterStatus{ManagedResources: []*cloudv1.ManagedResource{
				{Kind: "Cluster", Name: "c", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY},
				{Kind: "NodeGroup", Name: "c-default", State: tt.state, Message: tt.message}}}

			withNodeGroups(ctx, st, &cloudv1.ClusterStatus{})

			require.Len(t, st.NodeGroups, 2)
			system := st.NodeGroups[0]
			require.Equal(t, "system", system.GetName(), "the system group is first")
			require.True(t, system.GetPlatformManaged())
			require.Equal(t, tt.phase, system.GetPhase())
			require.Equal(t, int32(2), system.GetNodes(), "the count the pass read")
			if tt.phase == cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY {
				require.Empty(t, system.GetMessage())
			} else if tt.message != "" {
				require.Contains(t, system.GetMessage(), tt.message)
			}
			require.Equal(t, "gpu", st.NodeGroups[1].GetName())
			require.False(t, st.NodeGroups[1].GetPlatformManaged())
		})
	}
}

// With no pass (a read, or the applier off) the recorded counts and user
// groups stay; a platform group an earlier build recorded is dropped; a
// cluster with no node group object yet has a PENDING system group.
func TestSystemNodeGroupFromTheRecordedStatus(t *testing.T) {
	stored := &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{
		{Name: "platform", PlatformManaged: true, Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 1},
		{Name: "system", PlatformManaged: true, Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 2, NodesNotice: "later"},
		{Name: "gpu", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 5}}}
	st := &cloudv1.ClusterStatus{}

	withNodeGroups(context.Background(), st, stored)

	require.Len(t, st.NodeGroups, 2)
	require.Equal(t, "system", st.NodeGroups[0].GetName())
	require.Equal(t, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING, st.NodeGroups[0].GetPhase(), "no managed resource yet")
	require.Equal(t, int32(2), st.NodeGroups[0].GetNodes())
	require.Equal(t, "later", st.NodeGroups[0].GetNodesNotice())
	require.Equal(t, "gpu", st.NodeGroups[1].GetName())
	require.Equal(t, int32(5), st.NodeGroups[1].GetNodes())
}

// The move off the old platform pool is on the system group's message.
func TestSystemNodeGroupSaysWhenMovingOffTheOldPool(t *testing.T) {
	st := &cloudv1.ClusterStatus{ManagedResources: []*cloudv1.ManagedResource{
		{Kind: "NodeGroup", Name: "c-default", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY}}}
	ctx := withNodeGroupsHolder(context.Background())
	holdResult(ctx, &nodeGroupsResult{system: systemObserved{move: "moving components off the old platform pool: 2 left (a, b)"}})

	withNodeGroups(ctx, st, &cloudv1.ClusterStatus{})

	require.Equal(t, cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, st.NodeGroups[0].GetPhase())
	require.Equal(t, "moving components off the old platform pool: 2 left (a, b)", st.NodeGroups[0].GetMessage())
}

// The cluster's default node group shows the admin's min and max now, not the one
// stored when it was created; an imported cluster keeps its own.
func TestViewShowsTheAdminsSystemNodeGroupSize(t *testing.T) {
	w, row := nodeGroupWorld(t, newFakeWorkload(interceptor.Funcs{}))
	w.svc.deps.NodeGroups.System.MinSize, w.svc.deps.NodeGroups.System.MaxSize = 4, 9

	c, err := w.svc.View(t.Context(), row)

	require.NoError(t, err)
	sc := c.GetSpec().GetDefaultNodePool().GetScalingConfig()
	require.EqualValues(t, 4, sc.GetMinSize())
	require.EqualValues(t, 9, sc.GetMaxSize())
	require.Nil(t, sc.DesiredSize, "the node count is cluster-autoscaler's")
	stored, err := SpecOf(row)
	require.NoError(t, err)
	require.NotEqualValues(t, 9, stored.GetDefaultNodePool().GetScalingConfig().GetMaxSize(), "the stored spec is not rewritten")

	imported := w.cluster(t)
	imported.Imported, imported.State = true, StateImported
	c, err = w.svc.View(t.Context(), imported)
	require.NoError(t, err)
	require.NotEqualValues(t, 9, c.GetSpec().GetDefaultNodePool().GetScalingConfig().GetMaxSize())
}
