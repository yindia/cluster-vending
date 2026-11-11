package cluster

import (
	"testing"

	"connectrpc.com/connect/v2"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestChooseSubnets(t *testing.T) {
	subnets := []privateSubnet{{"subnet-b", "use1-az2", "b"}, {"subnet-a", "use1-az1", "a"}, {"subnet-c", "use1-az4", "c"}}
	tests := []struct {
		name      string
		zones     []string
		wantZones []string
		wantIDs   []string
		wantErr   bool
	}{
		{"every zone by default", nil, []string{"use1-az1", "use1-az2", "use1-az4"}, []string{"subnet-a", "subnet-b", "subnet-c"}, false},
		{"requested zones", []string{"use1-az4", "use1-az1"}, []string{"use1-az4", "use1-az1"}, []string{"subnet-c", "subnet-a"}, false},
		{"zone without subnet", []string{"use1-az1", "use1-az6"}, nil, nil, true},
		{"one zone", []string{"use1-az1"}, nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zones, ids, err := chooseSubnets(subnets, tt.zones)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !equal(zones, tt.wantZones) || !equal(ids, tt.wantIDs) {
				t.Errorf("got %v %v, want %v %v", zones, ids, tt.wantZones, tt.wantIDs)
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

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

// mustResolve is resolveSpec for specs that are valid.
func mustResolve(t *testing.T, in *cloudv1.ClusterSpec, zones []string) *cloudv1.ClusterSpec {
	t.Helper()
	spec, err := resolveSpec(in, zones)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestResolveSpecFillsDefaultsWithoutTouchingInput(t *testing.T) {
	in := minimalSpec()
	spec := mustResolve(t, in, []string{"use1-az1", "use1-az2"})
	if in.GetAws().GetAccessConfig() != nil || len(in.GetNetwork().GetZoneIds()) != 0 {
		t.Fatal("resolveSpec changed its input")
	}
	if got := spec.GetControlPlane().GetEndpointAccess().GetPublicAccessCidrs(); !equal(got, []string{"203.0.113.0/24"}) {
		t.Errorf("public cidrs = %v, want the client's", got)
	}
	in.ControlPlane = nil
	if _, err := resolveSpec(in, []string{"use1-az1", "use1-az2"}); err == nil {
		t.Error("no public access CIDRs: want an error, not a 0.0.0.0/0 default")
	}
	plan := clusterPlan(spec, []string{"subnet-a", "subnet-b"}, crossplaneawsBase())
	if plan.AuthenticationMode != defaultAuthMode || plan.NodePool.CapacityType != defaultCapacityType ||
		plan.NodePool.AMIType != defaultAMIType {
		t.Errorf("plan defaults = %q %q %q", plan.AuthenticationMode, plan.NodePool.CapacityType, plan.NodePool.AMIType)
	}
	if plan.NodePool.Desired != 2 {
		t.Errorf("desired = %d, want min_size 2", plan.NodePool.Desired)
	}
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
		{"all ready", stateProvisioning, []*cloudv1.ManagedResource{ready, ready}, false, cloudv1.ClusterState_CLUSTER_STATE_RUNNING},
		{"ready but add-ons not created yet", stateProvisioning, []*cloudv1.ManagedResource{ready, ready}, true,
			cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING},
		{"in progress", stateProvisioning, []*cloudv1.ManagedResource{ready, creating}, false, cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING},
		{"updating", stateUpdating, []*cloudv1.ManagedResource{ready, creating}, false, cloudv1.ClusterState_CLUSTER_STATE_UPDATING},
		{"failed", stateProvisioning, []*cloudv1.ManagedResource{ready, failed}, true, cloudv1.ClusterState_CLUSTER_STATE_FAILED},
		{"deleting", stateDeleting, []*cloudv1.ManagedResource{failed}, false, cloudv1.ClusterState_CLUSTER_STATE_DELETING},
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

func crossplaneawsBase() crossplaneaws.ClusterPlan {
	return crossplaneaws.ClusterPlan{Namespace: "tenant-acme", ProviderConfigName: "aws-1", TenantID: "acme",
		ClusterID: "id", ClusterName: "prod", Prefix: "prod-12345678"}
}
