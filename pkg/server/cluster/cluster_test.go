package cluster

import (
	"testing"

	"connectrpc.com/connect/v2"

	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func minimalSpec() *cloudv1.ClusterSpec {
	return &cloudv1.ClusterSpec{
		Region:       "us-east-1",
		Network:      &cloudv1.ClusterNetwork{NetworkId: "0190a000-0000-7000-8000-000000000001"},
		ControlPlane: &cloudv1.ControlPlaneSpec{EndpointAccess: &cloudv1.EndpointAccess{PublicAccessCidrs: []string{"203.0.113.0/24"}}},
		DefaultNodePool: &cloudv1.NodePoolSpec{InstanceTypes: []string{"m6i.large"},
			ScalingConfig: &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 4}},
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}},
	}
}

// mustResolve is corecluster.ResolveSpec for specs that are valid.
func mustResolve(t *testing.T, in *cloudv1.ClusterSpec, zones []string) *cloudv1.ClusterSpec {
	t.Helper()
	spec, err := corecluster.ResolveSpec(in, zones)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestApplyMask(t *testing.T) {
	newSpec := func() *cloudv1.ClusterSpec { return mustResolve(t, minimalSpec(), []string{"use1-az1", "use1-az2"}) }
	row := &model.Cluster{Name: "prod"}
	in := &cloudv1.Cluster{DisplayName: "Prod", Spec: minimalSpec()}
	in.Spec.DefaultNodePool.ScalingConfig.MaxSize = 9

	changed, err := applyMask(row, newSpec(), in, []string{"display_name"})
	if err != nil || changed || row.DisplayName != "Prod" {
		t.Errorf("display_name: changed=%v err=%v name=%q", changed, err, row.DisplayName)
	}
	spec := newSpec()
	changed, err = applyMask(row, spec, in, []string{"spec.default_node_pool.scaling_config.max_size"})
	if err != nil || !changed || spec.GetDefaultNodePool().GetScalingConfig().GetMaxSize() != 9 {
		t.Errorf("max_size: changed=%v err=%v", changed, err)
	}
	in.Spec.DefaultNodePool.ScalingConfig.MaxSize = 1 // below min_size 2
	if _, err := applyMask(row, newSpec(), in, []string{"spec.default_node_pool.scaling_config.max_size"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("max < min: err = %v, want invalid_argument", err)
	}
	for path, code := range map[string]connect.Code{"spec.region": connect.CodeInvalidArgument,
		"spec.existing_cluster": connect.CodeUnimplemented} {
		if _, err := applyMask(row, newSpec(), in, []string{path}); connect.CodeOf(err) != code {
			t.Errorf("%s: err = %v, want %v", path, err, code)
		}
	}
}
