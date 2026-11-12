package aws

import (
	"context"
	"errors"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

const vpcID = "vpc-0a1b2c3d4e5f60718"

// fakeEC2 serves a two-zone VPC like the platform builds: public subnets
// routed to an internet gateway, private ones to a NAT gateway each, plus
// one database subnet on the isolated main route table, and a subnet of
// another VPC. The first page of subnets holds two.
type fakeEC2 struct {
	vpcErr error
	// filters records the filters of every call that has them.
	filters     []string
	subnetCalls int
	// flowLogs are the region's flow logs.
	flowLogs []types.FlowLog
}

func (f *fakeEC2) record(filters []types.Filter) {
	for _, flt := range filters {
		f.filters = append(f.filters, awssdk.ToString(flt.Name)+"="+flt.Values[0])
	}
}

func tag(k, v string) types.Tag { return types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)} }

func (f *fakeEC2) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	if f.vpcErr != nil {
		return nil, f.vpcErr
	}
	assoc := func(cidr string, state types.VpcCidrBlockStateCode) types.VpcCidrBlockAssociation {
		return types.VpcCidrBlockAssociation{CidrBlock: awssdk.String(cidr), CidrBlockState: &types.VpcCidrBlockState{State: state}}
	}
	return &ec2.DescribeVpcsOutput{Vpcs: []types.Vpc{{
		VpcId:     awssdk.String(in.VpcIds[0]),
		CidrBlock: awssdk.String("10.0.0.0/16"),
		CidrBlockAssociationSet: []types.VpcCidrBlockAssociation{
			assoc("10.0.0.0/16", types.VpcCidrBlockStateCodeAssociated),
			assoc("100.64.0.0/16", types.VpcCidrBlockStateCodeAssociated),
			assoc("10.9.0.0/16", types.VpcCidrBlockStateCodeDisassociated),
		},
		Tags: []types.Tag{tag("Name", "prod"), tag("terraform", "true")},
	}}}, nil
}

func subnet(id, cidr, zoneID, zone string) types.Subnet {
	return types.Subnet{SubnetId: awssdk.String(id), CidrBlock: awssdk.String(cidr),
		AvailabilityZoneId: awssdk.String(zoneID), AvailabilityZone: awssdk.String(zone), VpcId: awssdk.String(vpcID)}
}

var allSubnets = map[string]types.Subnet{
	"subnet-0000000000000000b": subnet("subnet-0000000000000000b", "10.0.0.0/20", "use1-az1", "us-east-1a"),
	"subnet-0000000000000000a": subnet("subnet-0000000000000000a", "10.0.16.0/20", "use1-az2", "us-east-1b"),
	"subnet-0000000000000000c": subnet("subnet-0000000000000000c", "10.0.128.0/19", "use1-az1", "us-east-1a"),
	"subnet-0000000000000000d": subnet("subnet-0000000000000000d", "10.0.160.0/19", "use1-az2", "us-east-1b"),
	"subnet-0000000000000000e": subnet("subnet-0000000000000000e", "10.0.200.0/24", "use1-az1", "us-east-1a"),
	"subnet-00000000000000aaa": func() types.Subnet {
		s := subnet("subnet-00000000000000aaa", "172.16.0.0/24", "use1-az1", "us-east-1a")
		s.VpcId = awssdk.String("vpc-00000000000000bbb")
		return s
	}(),
}

// DescribeSubnets answers by ID like EC2: one unknown ID fails the call.
func (f *fakeEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	f.subnetCalls++
	var found []types.Subnet
	for _, id := range in.SubnetIds {
		s, ok := allSubnets[id]
		if !ok {
			return nil, &smithy.GenericAPIError{Code: "InvalidSubnetID.NotFound", Message: "The subnet ID '" + id + "' does not exist"}
		}
		found = append(found, s)
	}
	// Page 1 holds two subnets, page 2 the rest.
	if in.NextToken != nil {
		return &ec2.DescribeSubnetsOutput{Subnets: found[min(2, len(found)):]}, nil
	}
	out := &ec2.DescribeSubnetsOutput{Subnets: found[:min(2, len(found))]}
	if len(found) > 2 {
		out.NextToken = awssdk.String("page2")
	}
	return out, nil
}

func association(id, subnetID string, main bool) types.RouteTableAssociation {
	a := types.RouteTableAssociation{RouteTableAssociationId: awssdk.String(id), Main: awssdk.Bool(main),
		AssociationState: &types.RouteTableAssociationState{State: types.RouteTableAssociationStateCodeAssociated}}
	if subnetID != "" {
		a.SubnetId = awssdk.String(subnetID)
	}
	return a
}

func local() types.Route {
	return types.Route{DestinationCidrBlock: awssdk.String("10.0.0.0/16"), GatewayId: awssdk.String("local"),
		Origin: types.RouteOriginCreateRouteTable, State: types.RouteStateActive}
}

func (f *fakeEC2) DescribeRouteTables(_ context.Context, in *ec2.DescribeRouteTablesInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	f.record(in.Filters)
	return &ec2.DescribeRouteTablesOutput{RouteTables: []types.RouteTable{
		{RouteTableId: awssdk.String("rtb-main"), Associations: []types.RouteTableAssociation{association("rtbassoc-main", "", true)},
			Routes: []types.Route{local()}},
		{RouteTableId: awssdk.String("rtb-public"), Associations: []types.RouteTableAssociation{
			association("rtbassoc-pub-a", "subnet-0000000000000000b", false),
			association("rtbassoc-pub-b", "subnet-0000000000000000a", false),
			// A gateway (edge) association: not a subnet, skipped.
			{RouteTableAssociationId: awssdk.String("rtbassoc-edge"), GatewayId: awssdk.String("igw-1"), Main: awssdk.Bool(false)},
		}, Routes: []types.Route{local(), {DestinationCidrBlock: awssdk.String("0.0.0.0/0"), GatewayId: awssdk.String("igw-1"),
			Origin: types.RouteOriginCreateRoute, State: types.RouteStateActive}}},
		{RouteTableId: awssdk.String("rtb-private-a"), Associations: []types.RouteTableAssociation{association("rtbassoc-priv-a", "subnet-0000000000000000c", false)},
			Routes: []types.Route{local(), {DestinationCidrBlock: awssdk.String("0.0.0.0/0"), NatGatewayId: awssdk.String("nat-a")}}},
		{RouteTableId: awssdk.String("rtb-private-b"), Associations: []types.RouteTableAssociation{association("rtbassoc-priv-b", "subnet-0000000000000000d", false)},
			Routes: []types.Route{local(), {DestinationCidrBlock: awssdk.String("0.0.0.0/0"), NatGatewayId: awssdk.String("nat-b")}}},
	}}, nil
}

func (f *fakeEC2) DescribeInternetGateways(_ context.Context, in *ec2.DescribeInternetGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error) {
	f.record(in.Filters)
	return &ec2.DescribeInternetGatewaysOutput{InternetGateways: []types.InternetGateway{{InternetGatewayId: awssdk.String("igw-1")}}}, nil
}

func natAddr(alloc, ip string, primary bool) types.NatGatewayAddress {
	return types.NatGatewayAddress{AllocationId: awssdk.String(alloc), PublicIp: awssdk.String(ip), IsPrimary: awssdk.Bool(primary)}
}

func (f *fakeEC2) DescribeNatGateways(_ context.Context, in *ec2.DescribeNatGatewaysInput, _ ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error) {
	f.record(in.Filter)
	return &ec2.DescribeNatGatewaysOutput{NatGateways: []types.NatGateway{
		{NatGatewayId: awssdk.String("nat-b"), SubnetId: awssdk.String("subnet-0000000000000000a"), State: types.NatGatewayStateAvailable,
			ConnectivityType: types.ConnectivityTypePublic, NatGatewayAddresses: []types.NatGatewayAddress{natAddr("eipalloc-b", "3.3.3.3", true)}},
		{NatGatewayId: awssdk.String("nat-a"), SubnetId: awssdk.String("subnet-0000000000000000b"), State: types.NatGatewayStateAvailable,
			ConnectivityType: types.ConnectivityTypePublic, NatGatewayAddresses: []types.NatGatewayAddress{
				natAddr("eipalloc-a2", "2.2.2.2", false), natAddr("eipalloc-a1", "1.1.1.1", true)}},
		{NatGatewayId: awssdk.String("nat-old"), State: types.NatGatewayStateDeleted},
	}}, nil
}

func (f *fakeEC2) DescribeSecurityGroups(_ context.Context, in *ec2.DescribeSecurityGroupsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	f.record(in.Filters)
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []types.SecurityGroup{
		{GroupId: awssdk.String("sg-1"), GroupName: awssdk.String("default"), Description: awssdk.String("default VPC security group")},
	}}, nil
}

func fakeTenant(api EC2API) *Tenant {
	return &Tenant{AccountID: tenantAccount, Region: "us-east-1", ec2: api, logs: fakeLogs{}, iam: fakeIAM{}}
}

var platformSubnets = []string{
	"subnet-0000000000000000b", "subnet-0000000000000000a", "subnet-0000000000000000c",
	"subnet-0000000000000000d", "subnet-0000000000000000e",
}

func TestDiscoverNetwork(t *testing.T) {
	api := &fakeEC2{}
	n, err := fakeTenant(api).DiscoverNetwork(t.Context(), vpcID, platformSubnets)
	if err != nil {
		t.Fatalf("DiscoverNetwork: %v", err)
	}

	if n.VPC.CIDRBlock != "10.0.0.0/16" || len(n.VPC.SecondaryCIDRBlocks) != 1 || n.VPC.SecondaryCIDRBlocks[0] != "100.64.0.0/16" {
		t.Errorf("VPC CIDRs = %s %v", n.VPC.CIDRBlock, n.VPC.SecondaryCIDRBlocks)
	}
	if n.VPC.Tags["terraform"] != "true" {
		t.Errorf("VPC tags = %v", n.VPC.Tags)
	}

	wantSubnets := []struct {
		id, routeTable string
		role           SubnetRole
	}{
		{"subnet-0000000000000000a", "rtb-public", SubnetRolePublic},
		{"subnet-0000000000000000b", "rtb-public", SubnetRolePublic},
		{"subnet-0000000000000000c", "rtb-private-a", SubnetRolePrivate},
		{"subnet-0000000000000000d", "rtb-private-b", SubnetRolePrivate},
		{"subnet-0000000000000000e", "rtb-main", SubnetRoleIsolated}, // no explicit association
	}
	if len(n.Subnets) != len(wantSubnets) {
		t.Fatalf("got %d subnets (both pages?), want %d", len(n.Subnets), len(wantSubnets))
	}
	for i, w := range wantSubnets {
		s := n.Subnets[i]
		if s.ID != w.id || s.RouteTableID != w.routeTable || s.Role != w.role {
			t.Errorf("subnet %d = %+v, want %+v", i, s, w)
		}
	}
	if z := n.Subnets[1]; z.ZoneID != "use1-az1" || z.ZoneName != "us-east-1a" {
		t.Errorf("zone = %s %s", z.ZoneID, z.ZoneName)
	}

	var public RouteTable
	for _, rt := range n.RouteTables {
		if rt.ID == "rtb-public" {
			public = rt
		}
	}
	if len(public.Associations) != 2 || public.Associations[0].ID != "rtbassoc-pub-a" {
		t.Errorf("public associations = %+v, want the two subnet associations", public.Associations)
	}

	if len(n.NATGateways) != 2 {
		t.Fatalf("NAT gateways = %+v, want 2 (deleted skipped)", n.NATGateways)
	}
	if a := n.NATGateways[0]; a.ID != "nat-a" || a.AllocationIDs[0] != "eipalloc-a1" || a.PublicIPs[1] != "2.2.2.2" {
		t.Errorf("nat-a = %+v, want ordered by ID with the primary address first", a)
	}
	if len(n.InternetGateways) != 1 || n.DefaultSecurityGroup.ID != "sg-1" {
		t.Errorf("igws = %+v, default security group = %+v", n.InternetGateways, n.DefaultSecurityGroup)
	}
	if api.subnetCalls != 2 {
		t.Errorf("DescribeSubnets calls = %d, want 2 (paginated)", api.subnetCalls)
	}
	for _, f := range api.filters {
		if f != "vpc-id="+vpcID && f != "attachment.vpc-id="+vpcID && f != "group-name=default" {
			t.Errorf("call not scoped to the VPC: filter %s", f)
		}
	}
}

func TestDiscoverNetworkReadsOnlyNamedSubnets(t *testing.T) {
	n, err := fakeTenant(&fakeEC2{}).DiscoverNetwork(t.Context(), vpcID,
		[]string{"subnet-0000000000000000a", "subnet-0000000000000000c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Subnets) != 2 || n.Subnets[0].ID != "subnet-0000000000000000a" || n.Subnets[1].ID != "subnet-0000000000000000c" {
		t.Errorf("subnets = %+v, want exactly the two named", n.Subnets)
	}
}

func TestDiscoverNetworkErrors(t *testing.T) {
	notFound := &smithy.GenericAPIError{Code: "InvalidVpcID.NotFound", Message: "The vpc ID does not exist"}
	denied := &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "not authorized"}
	tests := []struct {
		name    string
		api     *fakeEC2
		vpc     string
		subnets []string
		want    error
	}{
		{"malformed vpc id", &fakeEC2{}, "vpc-xyz", platformSubnets, ErrInvalid},
		{"no subnets", &fakeEC2{}, vpcID, nil, ErrInvalid},
		{"malformed subnet id", &fakeEC2{}, vpcID, []string{"sub-1"}, ErrInvalid},
		{"vpc not found", &fakeEC2{vpcErr: notFound}, vpcID, platformSubnets, ErrNotFound},
		{"subnet does not exist", &fakeEC2{}, vpcID, []string{"subnet-00000000000000fff"}, ErrNotFound},
		{"subnet of another vpc", &fakeEC2{}, vpcID, []string{"subnet-00000000000000aaa"}, ErrInvalid},
		{"not authorized", &fakeEC2{vpcErr: denied}, vpcID, platformSubnets, ErrAccessDenied},
		{"malformed per AWS", &fakeEC2{vpcErr: &smithy.GenericAPIError{Code: "InvalidVpcID.Malformed"}}, vpcID, platformSubnets, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fakeTenant(tt.api).DiscoverNetwork(t.Context(), tt.vpc, tt.subnets)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	other := errors.New("connection reset")
	_, err := fakeTenant(&fakeEC2{vpcErr: other}).DiscoverNetwork(t.Context(), vpcID, platformSubnets)
	if !errors.Is(err, other) || errors.Is(err, ErrNotFound) {
		t.Errorf("other errors pass through wrapped: %v", err)
	}
}
