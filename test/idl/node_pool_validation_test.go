package idl_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func validNodePool() *cloudv1.CreateNodePoolRequest {
	return &cloudv1.CreateNodePoolRequest{
		TenantId:  "acme",
		ClusterId: "0190f1a2-0000-7000-8000-0000000000c1",
		NodePool: &cloudv1.NodePool{
			Id: "gpu",
			Spec: &cloudv1.NodePoolSpec{
				InstanceTypes: []string{"g5.xlarge"},
				ScalingConfig: &cloudv1.ScalingConfig{MinSize: 0, MaxSize: 4},
				Taints:        []*cloudv1.Taint{{Key: "nvidia.com/gpu", Effect: cloudv1.TaintEffect_TAINT_EFFECT_NO_SCHEDULE}},
			},
			Provider: &cloudv1.NodePool_Aws{Aws: &cloudv1.AwsNodePoolSpec{
				AmiType: cloudv1.AwsAmiType_AWS_AMI_TYPE_AL2023_X86_64_NVIDIA,
			}},
		},
	}
}

func TestCreateNodePoolRequestValidation(t *testing.T) {
	type req = cloudv1.CreateNodePoolRequest
	tests := []struct {
		name    string
		mutate  func(r *req)
		wantErr bool
	}{
		{name: "valid", mutate: func(*req) {}},
		{name: "spot with several instance types", mutate: func(r *req) {
			r.NodePool.Spec.InstanceTypes = []string{"m6i.large", "m7i.large"}
			r.NodePool.Spec.CapacityType = cloudv1.CapacityType_CAPACITY_TYPE_SPOT
		}},

		{name: "id default is reserved", wantErr: true, mutate: func(r *req) { r.NodePool.Id = "default" }},
		{name: "bad id", wantErr: true, mutate: func(r *req) { r.NodePool.Id = "GPU_pool" }},
		{name: "missing provider", wantErr: true, mutate: func(r *req) { r.NodePool.Provider = nil }},
		{name: "missing spec", wantErr: true, mutate: func(r *req) { r.NodePool.Spec = nil }},
		{name: "aws: bad instance type", wantErr: true, mutate: func(r *req) {
			r.NodePool.Spec.InstanceTypes = []string{"Standard_D4s_v5"}
		}},
		{name: "min above max", wantErr: true, mutate: func(r *req) {
			r.NodePool.Spec.ScalingConfig = &cloudv1.ScalingConfig{MinSize: 5, MaxSize: 4}
		}},
		{name: "missing cluster id", wantErr: true, mutate: func(r *req) { r.ClusterId = "" }},
		{name: "tenant not dns label", wantErr: true, mutate: func(r *req) { r.TenantId = "Acme_Corp" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := proto.Clone(validNodePool()).(*req)
			tt.mutate(r)
			if err := protovalidate.Validate(r); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestUpdateNodePoolRequestRequiresPoolAndMask(t *testing.T) {
	if err := protovalidate.Validate(&cloudv1.UpdateNodePoolRequest{TenantId: "acme", ClusterId: "prod"}); err == nil {
		t.Fatal("update without node_pool and update_mask accepted")
	}
}

func TestNetworkRequestsUseTheTenantRule(t *testing.T) {
	for _, msg := range []proto.Message{
		&cloudv1.GetNetworkRequest{TenantId: "Acme_Corp", NetworkId: "n"},
		&cloudv1.TakeOverNetworkRequest{TenantId: "Acme_Corp", NetworkId: "n"},
		&cloudv1.DeleteNetworkRequest{TenantId: "Acme_Corp", NetworkId: "n"},
		&cloudv1.ListNetworksRequest{TenantId: "Acme_Corp"},
	} {
		if err := protovalidate.Validate(msg); err == nil {
			t.Errorf("%T accepted a tenant id that is not a DNS label", msg)
		}
	}
}
