package cluster

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/config"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// withPlatformPool gives the world the platform's default node group layout,
// its platform pool among it.
func withPlatformPool(d *recon.Deps) { d.NodeGroups = config.DefaultNodeGroups() }

func TestListClusterNodeGroupManifests(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil, withPlatformPool)

	out, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	require.NoError(t, err)
	require.Len(t, out.GetNodeGroups(), 2, "the system group, then the platform pool: tenants define none")
	system, pool := out.GetNodeGroups()[0], out.GetNodeGroups()[1]
	require.Equal(t, "system", system.GetName())
	require.True(t, system.GetPlatformManaged())
	require.Empty(t, system.GetEc2NodeClassYaml(), "the system group is an EKS managed node group, not Karpenter objects")
	require.Empty(t, system.GetNodePoolYaml())
	require.Equal(t, crossplaneaws.PoolPlatform, pool.GetName())
	require.True(t, pool.GetPlatformManaged())
	require.Contains(t, pool.GetEc2NodeClassYaml(), "kind: EC2NodeClass")
	require.Contains(t, pool.GetNodePoolYaml(), "kind: NodePool")
	require.Contains(t, pool.GetNodePoolYaml(), "name: "+crossplaneaws.PoolPlatform)

	// Byte for byte what the controller applies: the reconciler's objects
	// as deterministic YAML.
	objs, err := w.svc.NodeGroupObjects(t.Context(), w.cluster(t))
	require.NoError(t, err)
	require.Len(t, objs, 2)
	for i, want := range []string{pool.GetEc2NodeClassYaml(), pool.GetNodePoolYaml()} {
		raw, err := yaml.Marshal(objs[i].Object)
		require.NoError(t, err)
		require.Equal(t, string(raw), want)
	}

	// Rendering twice gives the same text.
	again, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	require.NoError(t, err)
	require.Equal(t, pool.GetNodePoolYaml(), again.GetNodeGroups()[1].GetNodePoolYaml())
	require.False(t, strings.HasPrefix(pool.GetNodePoolYaml(), "---"))
}

func TestListClusterNodeGroupManifestsWithoutAPlatformPool(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	out, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	require.NoError(t, err)
	require.Len(t, out.GetNodeGroups(), 1, "only the system group")
	require.Equal(t, "system", out.GetNodeGroups()[0].GetName())
}

func TestListClusterNodeGroupManifestsIsTenantScoped(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	_, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: "other", ClusterId: w.row.ID})
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err), "%v", err)
}

func TestListClusterNodeGroupManifestsOfAnImportedClusterIsEmpty(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil, withPlatformPool)
	row := w.cluster(t)
	row.Imported, row.State = true, reconcilecluster.StateImported
	require.NoError(t, w.store.Clusters().Update(t.Context(), row))
	out, err := w.svc.ListClusterNodeGroupManifests(t.Context(), &cloudv1.ListClusterNodeGroupManifestsRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	require.NoError(t, err)
	require.Empty(t, out.GetNodeGroups())
}
