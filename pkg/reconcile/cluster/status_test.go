package cluster

import (
	"testing"

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

func TestStatus(t *testing.T) {
	ready := &cloudv1.ManagedResource{Kind: "Role", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY}
	creating := &cloudv1.ManagedResource{Kind: "Cluster", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING}
	failed := &cloudv1.ManagedResource{Kind: "NodeGroup", Name: "x", Message: "boom",
		State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED}
	tests := []struct {
		name        string
		state       string
		views       []*cloudv1.ManagedResource
		awaitingEKS bool
		want        cloudv1.ClusterState
	}{
		{"all ready", StateProvisioning, []*cloudv1.ManagedResource{ready, ready}, false, cloudv1.ClusterState_CLUSTER_STATE_RUNNING},
		{"ready but add-ons not created yet", StateProvisioning, []*cloudv1.ManagedResource{ready, ready}, true,
			cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING},
		{"in progress", StateProvisioning, []*cloudv1.ManagedResource{ready, creating}, false, cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING},
		{"updating", StateUpdating, []*cloudv1.ManagedResource{ready, creating}, false, cloudv1.ClusterState_CLUSTER_STATE_UPDATING},
		{"failed", StateProvisioning, []*cloudv1.ManagedResource{ready, failed}, true, cloudv1.ClusterState_CLUSTER_STATE_FAILED},
		{"deleting", StateDeleting, []*cloudv1.ManagedResource{failed}, false, cloudv1.ClusterState_CLUSTER_STATE_DELETING},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := &model.Cluster{Resource: model.Resource{State: tt.state}}
			if got := status(row, tt.views, tt.awaitingEKS).GetState(); got != tt.want {
				t.Errorf("state = %v, want %v", got, tt.want)
			}
		})
	}
}
