package network

import (
	"errors"
	"testing"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestDefaultLayout(t *testing.T) {
	got, err := planAWSLayout("us-east-1", &cloudv1.NetworkLayout{})
	if err != nil {
		t.Fatal(err)
	}
	if got.CIDRBlock != DefaultCIDRBlock || got.NAT != crossplaneaws.NATSingle {
		t.Errorf("defaults = %s %s", got.CIDRBlock, got.NAT)
	}
	want := []crossplaneaws.SubnetPlan{
		{Role: "public", ZoneID: "use1-az1", CIDR: "10.0.224.0/22"},
		{Role: "private", ZoneID: "use1-az1", CIDR: "10.0.0.0/19"},
		{Role: "public", ZoneID: "use1-az2", CIDR: "10.0.228.0/22"},
		{Role: "private", ZoneID: "use1-az2", CIDR: "10.0.32.0/19"},
		{Role: "public", ZoneID: "use1-az3", CIDR: "10.0.232.0/22"},
		{Role: "private", ZoneID: "use1-az3", CIDR: "10.0.64.0/19"},
	}
	if len(got.Subnets) != len(want) {
		t.Fatalf("subnets = %+v", got.Subnets)
	}
	for i := range want {
		if got.Subnets[i] != want[i] {
			t.Errorf("subnet %d = %+v, want %+v", i, got.Subnets[i], want[i])
		}
	}
}

func TestLayoutChoices(t *testing.T) {
	got, err := planAWSLayout("eu-west-1", &cloudv1.NetworkLayout{
		CidrBlock: "172.16.0.0/22", AvailabilityZoneCount: 2,
		NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.NAT != crossplaneaws.NATPerZone || len(got.Subnets) != 4 ||
		got.Subnets[0].CIDR != "172.16.3.128/28" || got.Subnets[1].CIDR != "172.16.0.0/25" {
		t.Errorf("layout = %+v", got)
	}
}

func TestExplicitSubnets(t *testing.T) {
	sub := func(role cloudv1.SubnetRole, cidr, zone string) *cloudv1.Subnet {
		return &cloudv1.Subnet{Role: role, CidrBlock: cidr, ZoneId: zone}
	}
	pub, priv := cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC, cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE
	ok := &cloudv1.NetworkLayout{CidrBlock: "10.1.0.0/16", Subnets: []*cloudv1.Subnet{
		sub(pub, "10.1.0.0/24", "use1-az1"), sub(priv, "10.1.16.0/20", "use1-az1"), sub(priv, "10.1.32.0/20", "use1-az2"),
	}}
	got, err := planAWSLayout("us-east-1", ok)
	if err != nil || len(got.Subnets) != 3 || got.Subnets[0].Role != crossplaneaws.SubnetPublic {
		t.Fatalf("explicit = %+v, %v", got, err)
	}

	for name, l := range map[string]*cloudv1.NetworkLayout{
		"outside cidr": {CidrBlock: "10.1.0.0/16", Subnets: []*cloudv1.Subnet{sub(priv, "10.2.0.0/24", "use1-az1")}},
		"overlap": {CidrBlock: "10.1.0.0/16", Subnets: []*cloudv1.Subnet{
			sub(priv, "10.1.0.0/20", "use1-az1"), sub(priv, "10.1.8.0/24", "use1-az2")}},
		"zone of another region": {Subnets: []*cloudv1.Subnet{sub(priv, "10.0.0.0/24", "euw1-az1")}},
		"too small":              {Subnets: []*cloudv1.Subnet{sub(priv, "10.0.0.0/29", "use1-az1")}},
		"default carve on /24":   {CidrBlock: "10.0.0.0/24"},
	} {
		if _, err := planAWSLayout("us-east-1", l); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := planAWSLayout("eu-west-1", &cloudv1.NetworkLayout{AvailabilityZoneCount: 6}); err == nil {
		t.Error("more zones than eu-west-1 has accepted")
	}
	_, err = planAWSLayout("us-east-1", &cloudv1.NetworkLayout{NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_REGIONAL})
	if !errors.Is(err, errUnsupported) {
		t.Errorf("regional NAT = %v, want errUnsupported", err)
	}
}
