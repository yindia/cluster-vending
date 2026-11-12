package aws

import (
	"context"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func (f *fakeEC2) DescribeNetworkAcls(_ context.Context, in *ec2.DescribeNetworkAclsInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkAclsOutput, error) {
	f.record(in.Filters)
	entry := func(n int32, egress bool, protocol string, action types.RuleAction) types.NetworkAclEntry {
		return types.NetworkAclEntry{RuleNumber: awssdk.Int32(n), Egress: awssdk.Bool(egress), Protocol: awssdk.String(protocol),
			RuleAction: action, CidrBlock: awssdk.String("0.0.0.0/0")}
	}
	https := entry(100, false, "6", types.RuleActionAllow)
	https.PortRange = &types.PortRange{From: awssdk.Int32(443), To: awssdk.Int32(443)}
	return &ec2.DescribeNetworkAclsOutput{NetworkAcls: []types.NetworkAcl{
		{NetworkAclId: awssdk.String("acl-private"), IsDefault: awssdk.Bool(false),
			Associations: []types.NetworkAclAssociation{
				{SubnetId: awssdk.String("subnet-0000000000000000d")}, {SubnetId: awssdk.String("subnet-0000000000000000c")}},
			Entries: []types.NetworkAclEntry{
				entry(32767, true, "-1", types.RuleActionDeny), entry(100, true, "-1", types.RuleActionAllow),
				entry(200, false, "-1", types.RuleActionDeny), https, entry(32767, false, "-1", types.RuleActionDeny)},
			Tags: []types.Tag{tag("Name", "private")}},
		{NetworkAclId: awssdk.String("acl-default"), IsDefault: awssdk.Bool(true),
			Entries: []types.NetworkAclEntry{entry(100, false, "-1", types.RuleActionAllow), entry(32767, false, "-1", types.RuleActionDeny)}},
	}}, nil
}

func TestDiscoverNetworkACLs(t *testing.T) {
	n, err := fakeTenant(&fakeEC2{}).DiscoverNetwork(t.Context(), vpcID, platformSubnets)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.NetworkACLs) != 2 || !n.NetworkACLs[0].Default || n.NetworkACLs[1].ID != "acl-private" {
		t.Fatalf("network ACLs = %+v, want the default and the custom one, by ID", n.NetworkACLs)
	}
	if d := n.NetworkACLs[0]; len(d.Rules) != 1 || len(d.SubnetIDs) != 0 {
		t.Errorf("default ACL = %+v, want its one rule (the default deny skipped)", d)
	}
	acl := n.NetworkACLs[1]
	if len(acl.SubnetIDs) != 2 || acl.SubnetIDs[0] != "subnet-0000000000000000c" || acl.Tags["Name"] != "private" {
		t.Errorf("custom ACL = %+v", acl)
	}
	// Ingress first, by rule number; the default deny entries are skipped.
	if len(acl.Rules) != 3 || acl.Rules[0].Number != 100 || acl.Rules[0].Egress || acl.Rules[0].FromPort != 443 ||
		acl.Rules[0].Protocol != "6" || acl.Rules[1].Number != 200 || acl.Rules[1].Action != "deny" || !acl.Rules[2].Egress {
		t.Errorf("rules = %+v", acl.Rules)
	}
}

func TestSubnetRoleFromRoutes(t *testing.T) {
	route := func(cidr, gateway, nat string) Route {
		return Route{DestinationCIDRBlock: cidr, GatewayID: gateway, NATGatewayID: nat}
	}
	local := route("10.0.0.0/16", "local", "")
	for _, tt := range []struct {
		name   string
		routes []Route
		want   SubnetRole
	}{
		{"default route to the internet gateway", []Route{local, route("0.0.0.0/0", "igw-1", "")}, SubnetRolePublic},
		{"specific CIDRs to the internet gateway", []Route{local, route("203.0.113.0/24", "igw-1", ""), route("198.51.100.0/24", "igw-1", "")}, SubnetRolePublic},
		{"default route to a NAT gateway", []Route{local, route("0.0.0.0/0", "", "nat-1")}, SubnetRolePrivate},
		{"NAT default wins over specific internet routes", []Route{route("0.0.0.0/0", "", "nat-1"), route("203.0.113.0/24", "igw-1", "")}, SubnetRolePrivate},
		{"default route elsewhere", []Route{route("0.0.0.0/0", "vgw-1", ""), route("203.0.113.0/24", "igw-1", "")}, SubnetRoleIsolated},
		{"local only", []Route{local}, SubnetRoleIsolated},
	} {
		if got := role(RouteTable{Routes: tt.routes}); got != tt.want {
			t.Errorf("%s: role = %s, want %s", tt.name, got, tt.want)
		}
	}
}
