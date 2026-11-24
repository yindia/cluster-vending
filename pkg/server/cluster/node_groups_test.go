package cluster

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func batchGroup() *cloudv1.NodeGroupSpec {
	return &cloudv1.NodeGroupSpec{
		Name:             "batch",
		InstanceFamilies: []string{"m7i", "c7g"},
		Architectures:    []cloudv1.Architecture{cloudv1.Architecture_ARCHITECTURE_ARM64},
		CapacityTypes:    []cloudv1.CapacityType{cloudv1.CapacityType_CAPACITY_TYPE_SPOT},
		Labels:           map[string]string{"team": "data"},
		Taints:           []*cloudv1.Taint{{Key: "dedicated", Value: "batch", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE}},
		Limits:           &cloudv1.NodeGroupLimits{Cpu: ptr.To(int32(100)), MemoryGib: ptr.To(int32(400))},
	}
}

func TestCreateClusterStoresNodeGroups(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	req := createRequest(w, "with-groups")
	req.Cluster.Spec.NodeGroups = []*cloudv1.NodeGroupSpec{batchGroup(), {Name: "web"}}

	out, err := w.svc.CreateCluster(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, out.GetCluster().GetSpec().GetNodeGroups(), 2)
	require.Equal(t, "batch", out.GetCluster().GetSpec().GetNodeGroups()[0].GetName())

	obj := &cloudv1alpha1.Cluster{}
	require.NoError(t, w.storeKube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: req.Cluster.Name}, obj))
	require.Len(t, obj.Spec.NodeGroups, 2)
	require.Equal(t, cloudv1alpha1.ArchitectureArm64, obj.Spec.NodeGroups[0].Architectures[0])
	require.Equal(t, cloudv1alpha1.CapacityTypeSpot, obj.Spec.NodeGroups[0].CapacityTypes[0])
	require.Equal(t, int32(100), *obj.Spec.NodeGroups[0].Limits.CPU)
}

func TestCreateClusterWithoutDefaultNodePoolIsAccepted(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	req := createRequest(w, "no-default-pool")
	req.Cluster.Spec.DefaultNodePool = nil

	_, err := w.svc.CreateCluster(t.Context(), req)
	require.NoError(t, err, "an omitted default_node_pool is filled by the platform, not refused")
}

func TestCreateClusterRefusesInvalidNodeGroups(t *testing.T) {
	many := make([]*cloudv1.NodeGroupSpec, 21)
	for i := range many {
		many[i] = &cloudv1.NodeGroupSpec{Name: "g" + strings.Repeat("a", i%5) + string(rune('a'+i))}
	}
	taints := make([]*cloudv1.Taint, 51)
	for i := range taints {
		taints[i] = &cloudv1.Taint{Key: "k" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE}
	}
	tests := map[string][]*cloudv1.NodeGroupSpec{
		"reserved platform": {{Name: "platform"}},
		"reserved default":  {{Name: "default"}},
		"reserved system":   {{Name: "system"}},
		"reserved pool":     {{Name: "platform-pool"}},
		"duplicate names":   {{Name: "a"}, {Name: "a"}},
		"upper case":        {{Name: "Batch"}},
		"starts with digit": {{Name: "1batch"}},
		"too long":          {{Name: strings.Repeat("a", 41)}},
		"empty name":        {{}},
		"more than 20":      many,
		"zero cpu limit":    {{Name: "a", Limits: &cloudv1.NodeGroupLimits{Cpu: ptr.To(int32(0))}}},
		"negative memory":   {{Name: "a", Limits: &cloudv1.NodeGroupLimits{MemoryGib: ptr.To(int32(-1))}}},
		"bad label key":     {{Name: "a", Labels: map[string]string{"bad key": "v"}}},
		"bad label value":   {{Name: "a", Labels: map[string]string{"k": "bad value!"}}},
		"51 taints":         {{Name: "a", Taints: taints}},
		"taint no effect":   {{Name: "a", Taints: []*cloudv1.Taint{{Key: "k"}}}},
		"bad instance type": {{Name: "a", InstanceTypes: []string{"large"}}},
		"duplicate arch": {{Name: "a", Architectures: []cloudv1.Architecture{
			cloudv1.Architecture_ARCHITECTURE_AMD64, cloudv1.Architecture_ARCHITECTURE_AMD64}}},
	}
	for name, groups := range tests {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			req := createRequest(w, "invalid")
			req.Cluster.Spec.NodeGroups = groups
			_, err := w.svc.CreateCluster(t.Context(), req)
			require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
		})
	}
}

func TestUpdateClusterChangesNodeGroupsAsOneMaskPath(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	before := w.cluster(t)

	_, err := w.svc.UpdateCluster(t.Context(), updateRequest(w,
		&cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{batchGroup()}}, "spec.node_groups"))
	require.NoError(t, err)
	after := w.cluster(t)
	require.Equal(t, before.Generation+1, after.Generation)
	require.Equal(t, reconcilecluster.StateUpdating, after.State)
	spec, err := reconcilecluster.SpecOf(after)
	require.NoError(t, err)
	require.Len(t, spec.GetNodeGroups(), 1)
	require.Equal(t, "batch", spec.GetNodeGroups()[0].GetName())

	// An empty list in the mask removes every node group.
	_, err = w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{}, "spec.node_groups"))
	require.NoError(t, err)
	spec, err = reconcilecluster.SpecOf(w.cluster(t))
	require.NoError(t, err)
	require.Empty(t, spec.GetNodeGroups())
}

func TestUpdateClusterRefusesInvalidNodeGroups(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	// platform-pool is the platform's NodePool; the renderer refuses it.
	for _, name := range []string{"system", "platform-pool"} {
		_, err := w.svc.UpdateCluster(t.Context(), updateRequest(w,
			&cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{{Name: name}}}, "spec.node_groups"))
		require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%s: %v", name, err)
	}
}

func TestUpdateClusterRefusesDefaultNodeGroupChanges(t *testing.T) {
	spec := &cloudv1.ClusterSpec{DefaultNodePool: &cloudv1.NodePoolSpec{
		Labels:        map[string]string{"a": "b"},
		Taints:        []*cloudv1.Taint{{Key: "k", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE}},
		ScalingConfig: &cloudv1.ScalingConfig{MinSize: 1, MaxSize: 9},
	}}
	for _, path := range []string{
		"spec.default_node_pool.taints",
		"spec.default_node_pool.labels",
		"spec.default_node_pool.scaling_config.min_size",
		"spec.default_node_pool.scaling_config.max_size",
	} {
		t.Run(path, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			before := w.cluster(t)
			_, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, spec, path))
			require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err), "%v", err)
			require.Contains(t, err.Error(), "the default node group is managed by the platform")
			require.Equal(t, before.Generation, w.cluster(t).Generation, "a refused update stores nothing")
		})
	}
}

func TestPlanShowsNodeGroupChanges(t *testing.T) {
	w := appliedWorld(t)
	first := batchGroup()
	_, err := update(t, w, updateRequest(w, &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{first, {Name: "old"}}}, "spec.node_groups"))
	require.NoError(t, err)

	changed := batchGroup()
	changed.Labels = map[string]string{"team": "ml"}
	res, err := update(t, w, planRequest(w, &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{changed, {Name: "fresh"}}}, "spec.node_groups"))
	require.NoError(t, err)
	got := map[string]cloudv1.PlannedAction{}
	for _, c := range res.GetPlan().GetChanges() {
		if c.GetSpecPath() == "spec.node_groups" {
			require.Equal(t, "NodePool", c.GetKind())
			got[c.GetName()] = c.GetAction()
		}
	}
	require.Equal(t, map[string]cloudv1.PlannedAction{
		"batch": cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE,
		"fresh": cloudv1.PlannedAction_PLANNED_ACTION_CREATE,
		"old":   cloudv1.PlannedAction_PLANNED_ACTION_DELETE,
	}, got)
	require.True(t, res.GetPlan().GetApplicable())
}
