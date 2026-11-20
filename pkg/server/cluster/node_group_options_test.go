package cluster

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func allowlist(families, types []string) func(*recon.Deps) {
	return func(d *recon.Deps) { d.NodeGroupInstanceFamilies, d.NodeGroupInstanceTypes = families, types }
}

func TestAllowedInstance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		families []string
		types    []string
		group    *cloudv1.NodeGroupSpec
		wantErr  string
	}{
		{name: "no allowlist allows anything", group: &cloudv1.NodeGroupSpec{Name: "a", InstanceFamilies: []string{"x1e"}, InstanceTypes: []string{"x1e.xlarge"}}},
		{name: "no allowlist allows the defaults", group: &cloudv1.NodeGroupSpec{Name: "a"}},
		{name: "allowed family", families: []string{"m7i", "c7i"}, group: &cloudv1.NodeGroupSpec{Name: "a", InstanceFamilies: []string{"m7i"}}},
		{name: "family outside", families: []string{"m7i", "c7i"}, group: &cloudv1.NodeGroupSpec{Name: "batch", InstanceFamilies: []string{"m7i", "p5"}},
			wantErr: `node group "batch": instance family "p5" is not allowed; allowed families: m7i, c7i`},
		{name: "type of an allowed family", families: []string{"m7i"}, group: &cloudv1.NodeGroupSpec{Name: "a", InstanceTypes: []string{"m7i.large"}}},
		{name: "type listed", families: []string{"m7i"}, types: []string{"r7i.large"}, group: &cloudv1.NodeGroupSpec{Name: "a", InstanceTypes: []string{"r7i.large"}}},
		{name: "type outside", families: []string{"m7i"}, types: []string{"r7i.large"}, group: &cloudv1.NodeGroupSpec{Name: "web", InstanceTypes: []string{"r7i.xlarge"}},
			wantErr: `node group "web": instance type "r7i.xlarge" is not allowed; allowed families: m7i; allowed types: r7i.large`},
		{name: "types only: a type outside", types: []string{"r7i.large"}, group: &cloudv1.NodeGroupSpec{Name: "web", InstanceTypes: []string{"m7i.large"}},
			wantErr: `instance type "m7i.large" is not allowed`},
		{name: "types only leaves families free", types: []string{"r7i.large"}, group: &cloudv1.NodeGroupSpec{Name: "a", InstanceFamilies: []string{"anything"}}},
		{name: "families set: defaults are refused", families: []string{"m7i"}, group: &cloudv1.NodeGroupSpec{Name: "bare"},
			wantErr: `node group "bare": name instance_families or instance_types from the allowed list`},
		{name: "family prefix must match exactly", families: []string{"m7"}, group: &cloudv1.NodeGroupSpec{Name: "a", InstanceTypes: []string{"m7i.large"}},
			wantErr: `instance type "m7i.large" is not allowed`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkNodeGroupAllowlist([]*cloudv1.NodeGroupSpec{tc.group}, nil, tc.families, tc.types)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestCreateClusterRefusesNodeGroupsOutsideTheAllowlist(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil, allowlist([]string{"m7i", "c7i"}, nil))
	req := createRequest(w, "outside")
	req.Cluster.Spec.NodeGroups = []*cloudv1.NodeGroupSpec{{Name: "gpu", InstanceFamilies: []string{"p5"}}}
	_, err := w.svc.CreateCluster(t.Context(), req)
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
	require.Contains(t, err.Error(), `"gpu"`)
	require.Contains(t, err.Error(), `"p5"`)
	require.Contains(t, err.Error(), "m7i, c7i")

	req = createRequest(w, "inside")
	req.Cluster.Spec.NodeGroups = []*cloudv1.NodeGroupSpec{{Name: "web", InstanceFamilies: []string{"c7i"}}}
	_, err = w.svc.CreateCluster(t.Context(), req)
	require.NoError(t, err)
}

func TestUpdateClusterChecksOnlyNewOrChangedNodeGroups(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	old := &cloudv1.NodeGroupSpec{Name: "old", InstanceFamilies: []string{"x1e"}}
	_, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{old}}, "spec.node_groups"))
	require.NoError(t, err)

	// The allowlist is set later: the existing group stays, as is.
	w.svc.deps.NodeGroupInstanceFamilies = []string{"m7i"}
	keep := &cloudv1.NodeGroupSpec{Name: "old", InstanceFamilies: []string{"x1e"}}
	_, err = w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{keep, {Name: "web", InstanceFamilies: []string{"m7i"}}}}, "spec.node_groups"))
	require.NoError(t, err)

	// A new group outside is refused, and so is a changed one.
	_, err = w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{keep, {Name: "web", InstanceFamilies: []string{"m7i"}}, {Name: "gpu", InstanceFamilies: []string{"p5"}}}}, "spec.node_groups"))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
	_, err = w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{{Name: "old", InstanceFamilies: []string{"x1e", "x2e"}}}}, "spec.node_groups"))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "%v", err)
}

func TestGetNodeGroupOptions(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil, allowlist([]string{"m7i", "c7i"}, []string{"r7i.large"}))
	out, err := w.svc.GetNodeGroupOptions(t.Context(), &cloudv1.GetNodeGroupOptionsRequest{TenantId: worldTenant})
	require.NoError(t, err)
	require.Equal(t, []string{"m7i", "c7i"}, out.GetInstanceFamilies())
	require.Equal(t, []string{"r7i.large"}, out.GetInstanceTypes())
	require.Equal(t, []cloudv1.Architecture{cloudv1.Architecture_ARCHITECTURE_AMD64, cloudv1.Architecture_ARCHITECTURE_ARM64}, out.GetArchitectures())
	require.Equal(t, []cloudv1.CapacityType{cloudv1.CapacityType_CAPACITY_TYPE_ON_DEMAND, cloudv1.CapacityType_CAPACITY_TYPE_SPOT}, out.GetCapacityTypes())

	_, err = w.svc.GetNodeGroupOptions(t.Context(), &cloudv1.GetNodeGroupOptionsRequest{TenantId: "Not A Tenant"})
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestListClusterNodeGroupManifests(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil, allowlist([]string{"m7i"}, nil))
	_, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{NodeGroups: []*cloudv1.NodeGroupSpec{
		{Name: "batch", InstanceFamilies: []string{"m7i"}}}}, "spec.node_groups"))
	require.NoError(t, err)

	out, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	require.NoError(t, err)
	require.Len(t, out.GetNodeGroups(), 2)
	platform, batch := out.GetNodeGroups()[0], out.GetNodeGroups()[1]
	require.Equal(t, "platform", platform.GetName())
	require.True(t, platform.GetPlatformManaged())
	require.Equal(t, "batch", batch.GetName())
	require.False(t, batch.GetPlatformManaged())
	require.Contains(t, platform.GetEc2NodeClassYaml(), "kind: EC2NodeClass")
	require.Contains(t, platform.GetNodePoolYaml(), "kind: NodePool")
	require.Contains(t, platform.GetNodePoolYaml(), "karpenter.k8s.aws/instance-family")
	require.Contains(t, batch.GetNodePoolYaml(), "name: batch")

	// Byte for byte what the controller applies: the reconciler's objects
	// as deterministic YAML.
	objs, err := w.svc.NodeGroupObjects(t.Context(), w.cluster(t))
	require.NoError(t, err)
	require.Len(t, objs, 4)
	for i, want := range []string{platform.GetEc2NodeClassYaml(), platform.GetNodePoolYaml(), batch.GetEc2NodeClassYaml(), batch.GetNodePoolYaml()} {
		raw, err := yaml.Marshal(objs[i].Object)
		require.NoError(t, err)
		require.Equal(t, string(raw), want)
	}

	// Rendering twice gives the same text.
	again, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	require.NoError(t, err)
	require.Equal(t, out.GetNodeGroups()[1].GetNodePoolYaml(), again.GetNodeGroups()[1].GetNodePoolYaml())
	require.False(t, strings.HasPrefix(platform.GetNodePoolYaml(), "---"))
}

func TestListClusterNodeGroupManifestsIsTenantScoped(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	_, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: "other", ClusterId: w.row.ID})
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "%v", err)
}

func TestListClusterNodeGroupManifestsOfAnImportedClusterIsEmpty(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	row := w.cluster(t)
	row.Imported, row.State = true, reconcilecluster.StateImported
	require.NoError(t, w.store.Clusters().Update(t.Context(), row))
	out, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	require.NoError(t, err)
	require.Empty(t, out.GetNodeGroups())
}
