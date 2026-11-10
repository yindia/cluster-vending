package idl_test

import (
	"testing"

	"buf.build/go/protovalidate"
	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func subnet(role cloudv1.SubnetRole, cidr, az string) *cloudv1.Subnet {
	return &cloudv1.Subnet{Role: role, CidrBlock: cidr, ZoneId: az}
}

const (
	public  = cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC
	private = cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE
)

func twoZoneLayout() []*cloudv1.Subnet {
	return []*cloudv1.Subnet{
		subnet(public, "10.0.0.0/20", "use1-az1"),
		subnet(public, "10.0.16.0/20", "use1-az2"),
		subnet(private, "10.0.64.0/19", "use1-az1"),
		subnet(private, "10.0.96.0/19", "use1-az2"),
	}
}

func twoZoneNetwork() *cloudv1.NetworkLayout {
	return &cloudv1.NetworkLayout{
		CidrBlock:      "10.0.0.0/16",
		NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE,
		Subnets:        twoZoneLayout(),
	}
}

func TestNetworkLayoutValidation(t *testing.T) {
	type layout = cloudv1.NetworkLayout
	tests := []struct {
		name    string
		region  string
		mutate  func(n *layout)
		wantErr bool
	}{
		{name: "two zones per-zone nat", mutate: func(*layout) {}},
		{name: "default layout", mutate: func(n *layout) { *n = layout{} }},
		{name: "zone count only", mutate: func(n *layout) { *n = layout{CidrBlock: "10.40.0.0/16", AvailabilityZoneCount: 3} }},
		{name: "single nat with public subnet in one zone only", mutate: func(n *layout) {
			n.NatGatewayMode = cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_SINGLE
			n.Subnets = twoZoneLayout()[1:]
			n.Subnets = append(n.Subnets, subnet(private, "10.0.128.0/19", "use1-az3"))
		}},

		{name: "aws: bad region", region: "eastus", wantErr: true, mutate: func(*layout) {}},
		{name: "cidr too small", wantErr: true, mutate: func(n *layout) { n.CidrBlock = "10.0.0.0/28" }},
		{name: "cidr host bits set", wantErr: true, mutate: func(n *layout) { n.CidrBlock = "10.0.0.1/16" }},
		{name: "zone count out of range", wantErr: true, mutate: func(n *layout) { n.Subnets, n.AvailabilityZoneCount = nil, 1 }},
		{name: "zonal subnets and zone count", wantErr: true, mutate: func(n *layout) { n.AvailabilityZoneCount = 3 }},
		{name: "duplicate subnet cidrs", wantErr: true, mutate: func(n *layout) {
			n.Subnets = append(n.Subnets, subnet(private, "10.0.96.0/19", "use1-az3"))
		}},
		{name: "subnet without role", wantErr: true, mutate: func(n *layout) {
			n.Subnets = append(n.Subnets, subnet(cloudv1.SubnetRole_SUBNET_ROLE_UNSPECIFIED, "10.0.128.0/19", "use1-az3"))
		}},
		{name: "only public subnets", wantErr: true, mutate: func(n *layout) { n.Subnets = twoZoneLayout()[:2] }},
		{name: "aws: zone name instead of zone id", wantErr: true, mutate: func(n *layout) {
			n.Subnets[0].ZoneId = "us-east-1a"
		}},
		{name: "aws: subnet without zone", wantErr: true, mutate: func(n *layout) {
			n.Subnets = append(n.Subnets, subnet(private, "10.0.128.0/19", ""))
		}},
		{name: "aws: private subnets in one zone", wantErr: true, mutate: func(n *layout) {
			n.Subnets = []*cloudv1.Subnet{
				subnet(public, "10.0.0.0/20", "use1-az1"),
				subnet(private, "10.0.64.0/19", "use1-az1"),
				subnet(private, "10.0.96.0/19", "use1-az1"),
			}
		}},
		{name: "aws: no public subnet", wantErr: true, mutate: func(n *layout) {
			n.NatGatewayMode = cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_SINGLE
			n.Subnets = twoZoneLayout()[2:]
		}},
		{name: "aws: per-zone nat missing public subnet", wantErr: true, mutate: func(n *layout) {
			n.Subnets = append(n.Subnets, subnet(private, "10.0.128.0/19", "use1-az3"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := proto.Clone(validNetwork()).(*cloudv1.CreateNetworkRequest)
			if tt.region != "" {
				r.Network.Spec.Region = tt.region
			}
			tt.mutate(r.Network.Spec.Layout)
			if err := protovalidate.Validate(r); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func validNetwork() *cloudv1.CreateNetworkRequest {
	return &cloudv1.CreateNetworkRequest{TenantId: "acme", Network: &cloudv1.Network{
		Name:        "shared-use1",
		DisplayName: "Shared us-east-1",
		Spec: &cloudv1.NetworkSpec{
			Region:      "us-east-1",
			BootstrapId: "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d",
			Layout:      twoZoneNetwork(),
			Tags:        map[string]string{"team": "platform"},
			Provider:    &cloudv1.NetworkSpec_Aws{Aws: &cloudv1.AwsNetworkSpec{}},
		},
	}}
}

func TestCreateNetworkRequestValidation(t *testing.T) {
	with := func(f func(r *cloudv1.CreateNetworkRequest)) *cloudv1.CreateNetworkRequest {
		r := proto.Clone(validNetwork()).(*cloudv1.CreateNetworkRequest)
		f(r)
		return r
	}
	tests := []struct {
		name    string
		req     *cloudv1.CreateNetworkRequest
		wantErr bool
	}{
		{name: "valid", req: validNetwork()},
		{name: "missing layout", wantErr: true, req: with(func(r *cloudv1.CreateNetworkRequest) { r.Network.Spec.Layout = nil })},
		{name: "missing provider", wantErr: true, req: with(func(r *cloudv1.CreateNetworkRequest) { r.Network.Spec.Provider = nil })},
		{name: "missing account", wantErr: true, req: with(func(r *cloudv1.CreateNetworkRequest) { r.Network.Spec.BootstrapId = "" })},
		{name: "account is not a bootstrap id", wantErr: true, req: with(func(r *cloudv1.CreateNetworkRequest) {
			r.Network.Spec.BootstrapId = "444455556666"
		})},
		{name: "bad name", wantErr: true, req: with(func(r *cloudv1.CreateNetworkRequest) { r.Network.Name = "Shared_1" })},
		{name: "no name", wantErr: true, req: with(func(r *cloudv1.CreateNetworkRequest) { r.Network.Name = "" })},
		{name: "aws reserved tag", wantErr: true, req: with(func(r *cloudv1.CreateNetworkRequest) {
			r.Network.Spec.Tags = map[string]string{"aws:x": "y"}
		})},
		{name: "tenant too long", wantErr: true, req: with(func(r *cloudv1.CreateNetworkRequest) {
			r.TenantId = "a234567890123456789012345678901234567890123456789012345678"
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := protovalidate.Validate(tt.req); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestImportNetworkRequestValidation(t *testing.T) {
	valid := func() *cloudv1.ImportNetworkRequest {
		return &cloudv1.ImportNetworkRequest{
			TenantId:    "acme",
			Name:        "tf-prod",
			Region:      "us-east-1",
			BootstrapId: "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d",
			Provider: &cloudv1.ImportNetworkRequest_Aws{Aws: &cloudv1.AwsImportNetwork{
				VpcId:     "vpc-0f1e2d3c4b5a69788",
				SubnetIds: []string{"subnet-0aaaaaaaaaaaaaaaa", "subnet-0bbbbbbbbbbbbbbbb"},
			}},
		}
	}
	tests := []struct {
		name    string
		mutate  func(r *cloudv1.ImportNetworkRequest)
		wantErr bool
	}{
		{name: "valid", mutate: func(*cloudv1.ImportNetworkRequest) {}},
		{name: "not a vpc id", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) { r.GetAws().VpcId = "subnet-0f1e2d3c4b5a69788" }},
		{name: "missing provider", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) { r.Provider = nil }},
		{name: "single subnet", mutate: func(r *cloudv1.ImportNetworkRequest) {
			r.GetAws().SubnetIds = []string{"subnet-0aaaaaaaaaaaaaaaa"}
		}},
		{name: "no subnets", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) { r.GetAws().SubnetIds = nil }},
		{name: "no account", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) { r.BootstrapId = "" }},
		{name: "account is not a bootstrap id", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) { r.BootstrapId = "4444" }},
		{name: "duplicate subnet", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) {
			r.GetAws().SubnetIds = []string{"subnet-0aaaaaaaaaaaaaaaa", "subnet-0aaaaaaaaaaaaaaaa"}
		}},
		{name: "not a subnet id", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) {
			r.GetAws().SubnetIds = []string{"rtb-0aaaaaaaaaaaaaaaa"}
		}},
		{name: "aws bad region", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) { r.Region = "eastus" }},
		{name: "bad network name", wantErr: true, mutate: func(r *cloudv1.ImportNetworkRequest) { r.Name = "TF_Prod" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid()
			tt.mutate(r)
			if err := protovalidate.Validate(r); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
