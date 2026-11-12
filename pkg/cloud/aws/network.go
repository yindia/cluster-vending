package aws

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EC2API is the part of the EC2 client discovery uses: read-only calls
// only.
type EC2API interface {
	DescribeVpcs(context.Context, *ec2.DescribeVpcsInput, ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error)
	DescribeInternetGateways(context.Context, *ec2.DescribeInternetGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeInternetGatewaysOutput, error)
	DescribeNatGateways(context.Context, *ec2.DescribeNatGatewaysInput, ...func(*ec2.Options)) (*ec2.DescribeNatGatewaysOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DescribeSecurityGroupRules(context.Context, *ec2.DescribeSecurityGroupRulesInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupRulesOutput, error)
	DescribeNetworkInterfaces(context.Context, *ec2.DescribeNetworkInterfacesInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error)
	DescribeNetworkAcls(context.Context, *ec2.DescribeNetworkAclsInput, ...func(*ec2.Options)) (*ec2.DescribeNetworkAclsOutput, error)
	DescribeFlowLogs(context.Context, *ec2.DescribeFlowLogsInput, ...func(*ec2.Options)) (*ec2.DescribeFlowLogsOutput, error)
}

// Network is what an existing VPC is made of: the input of an import plan.
// Lists are ordered by ID.
type Network struct {
	AccountID string
	Region    string
	VPC       VPC
	// Subnets are the subnets the caller named, nothing else.
	Subnets          []Subnet
	RouteTables      []RouteTable
	InternetGateways []InternetGateway
	NATGateways      []NATGateway
	// DefaultSecurityGroup is the VPC's "default" group. Other groups
	// belong to whatever uses them and are not imported.
	DefaultSecurityGroup SecurityGroup
	// NetworkACLs are the VPC's network ACLs, the default one included.
	NetworkACLs []NetworkACL
	// FlowLogs are the flow logs on the VPC itself (not on its subnets or
	// network interfaces).
	FlowLogs []FlowLog
}

// VPC is an ec2 VPC.
type VPC struct {
	ID        string
	CIDRBlock string
	// SecondaryCIDRBlocks are the other associated IPv4 blocks.
	SecondaryCIDRBlocks []string
	IsDefault           bool
	Tags                map[string]string
}

// SubnetRole is how a subnet routes to the internet, derived from its
// route table: the default route (0.0.0.0/0) first, else any route to an
// internet gateway.
type SubnetRole string

// Subnet roles.
const (
	// SubnetRolePublic: a route to an internet gateway, the default route
	// or routes for specific CIDRs only.
	SubnetRolePublic   SubnetRole = "public"
	SubnetRolePrivate  SubnetRole = "private"  // default route to a NAT gateway
	SubnetRoleIsolated SubnetRole = "isolated" // neither
)

// Subnet is an ec2 Subnet.
type Subnet struct {
	ID                  string
	CIDRBlock           string
	ZoneID              string // e.g. use1-az1: the contract uses zone IDs
	ZoneName            string // e.g. us-east-1a
	MapPublicIPOnLaunch bool
	// RouteTableID is the route table in effect: the explicitly associated
	// one, else the VPC's main route table.
	RouteTableID string
	Role         SubnetRole
	Tags         map[string]string
}

// RouteTable is an ec2 RouteTable.
type RouteTable struct {
	ID           string
	Main         bool
	Associations []RouteTableAssociation
	Routes       []Route
	Tags         map[string]string
}

// RouteTableAssociation links a route table to a subnet (or is the main
// association). ID is the rtbassoc-… external name of the Crossplane
// RouteTableAssociation.
type RouteTableAssociation struct {
	ID       string
	SubnetID string // empty for the main association
	Main     bool
}

// Route is one route of a route table; exactly one target is set.
type Route struct {
	DestinationCIDRBlock     string
	DestinationIPv6CIDRBlock string
	DestinationPrefixListID  string
	GatewayID                string // igw-…, vgw-…, or "local"
	NATGatewayID             string
	TransitGatewayID         string
	VPCPeeringConnectionID   string
	NetworkInterfaceID       string
	// Origin is CreateRouteTable (the local route), CreateRoute or
	// EnableVgwRoutePropagation.
	Origin string
	State  string
}

// InternetGateway is an ec2 InternetGateway attached to the VPC.
type InternetGateway struct {
	ID   string
	Tags map[string]string
}

// NATGateway is an ec2 NatGateway in the VPC (deleted ones are skipped).
type NATGateway struct {
	ID               string
	SubnetID         string
	State            string // pending, available, failed, deleting
	ConnectivityType string // public or private
	// AllocationIDs are the Elastic IPs of a public NAT gateway, primary
	// first.
	AllocationIDs []string
	PublicIPs     []string
	Tags          map[string]string
}

// SecurityGroup is an ec2 SecurityGroup in the VPC.
type SecurityGroup struct {
	ID          string
	Name        string
	Description string
	Tags        map[string]string
}

var (
	vpcIDPattern    = regexp.MustCompile(`^vpc-[0-9a-f]{8,17}$`)
	subnetIDPattern = regexp.MustCompile(`^subnet-[0-9a-f]{8,17}$`)
)

// DiscoverNetwork reads a VPC and what it is made of, for the subnets the
// caller passes (AwsImportNetwork.subnet_ids, at least one). A subnet that
// does not exist is ErrNotFound; one in another VPC is ErrInvalid. The
// VPC's other subnets are not read.
//
// Calls: ec2:DescribeVpcs, DescribeSubnets (by ID), DescribeRouteTables,
// DescribeInternetGateways, DescribeNatGateways, DescribeSecurityGroups
// (the default group only), DescribeNetworkAcls, and those of flowLogs.
func (t *Tenant) DiscoverNetwork(ctx context.Context, vpcID string, subnetIDs []string) (*Network, error) {
	if err := validateIDs(vpcID, subnetIDs); err != nil {
		return nil, err
	}
	vpc, err := t.vpc(ctx, vpcID)
	if err != nil {
		return nil, err
	}
	byVPC := []types.Filter{{Name: awssdk.String("vpc-id"), Values: []string{vpcID}}}

	routeTables, err := t.routeTables(ctx, byVPC)
	if err != nil {
		return nil, err
	}
	subnets, err := t.subnets(ctx, vpcID, subnetIDs, routeTables)
	if err != nil {
		return nil, err
	}
	igws, err := t.internetGateways(ctx, vpcID)
	if err != nil {
		return nil, err
	}
	nats, err := t.natGateways(ctx, byVPC)
	if err != nil {
		return nil, err
	}
	defaultGroup, err := t.defaultSecurityGroup(ctx, vpcID)
	if err != nil {
		return nil, err
	}
	flowLogs, err := t.flowLogs(ctx, vpcID)
	if err != nil {
		return nil, err
	}
	acls, err := t.networkACLs(ctx, byVPC)
	if err != nil {
		return nil, err
	}
	return &Network{
		NetworkACLs:          acls,
		FlowLogs:             flowLogs,
		AccountID:            t.AccountID,
		Region:               t.Region,
		VPC:                  vpc,
		Subnets:              subnets,
		RouteTables:          routeTables,
		InternetGateways:     igws,
		NATGateways:          nats,
		DefaultSecurityGroup: defaultGroup,
	}, nil
}

func validateIDs(vpcID string, subnetIDs []string) error {
	if !vpcIDPattern.MatchString(vpcID) {
		return fmt.Errorf("%w: VPC ID %q", ErrInvalid, vpcID)
	}
	if len(subnetIDs) == 0 {
		return fmt.Errorf("%w: at least one subnet ID is required", ErrInvalid)
	}
	for _, id := range subnetIDs {
		if !subnetIDPattern.MatchString(id) {
			return fmt.Errorf("%w: subnet ID %q", ErrInvalid, id)
		}
	}
	return nil
}

func (t *Tenant) vpc(ctx context.Context, vpcID string) (VPC, error) {
	out, err := t.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil {
		return VPC{}, translate(err, "describe VPC "+vpcID)
	}
	if len(out.Vpcs) != 1 {
		return VPC{}, fmt.Errorf("%w: VPC %s in %s/%s", ErrNotFound, vpcID, t.AccountID, t.Region)
	}
	v := out.Vpcs[0]
	primary := awssdk.ToString(v.CidrBlock)
	var secondary []string
	for _, a := range v.CidrBlockAssociationSet {
		cidr := awssdk.ToString(a.CidrBlock)
		if cidr == primary || a.CidrBlockState == nil || a.CidrBlockState.State != types.VpcCidrBlockStateCodeAssociated {
			continue
		}
		secondary = append(secondary, cidr)
	}
	return VPC{
		ID:                  vpcID,
		CIDRBlock:           primary,
		SecondaryCIDRBlocks: secondary,
		IsDefault:           awssdk.ToBool(v.IsDefault),
		Tags:                tags(v.Tags),
	}, nil
}

func (t *Tenant) routeTables(ctx context.Context, filters []types.Filter) ([]RouteTable, error) {
	var out []RouteTable
	p := ec2.NewDescribeRouteTablesPaginator(t.ec2, &ec2.DescribeRouteTablesInput{Filters: filters})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, translate(err, "describe route tables")
		}
		for _, rt := range page.RouteTables {
			out = append(out, routeTable(rt))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func routeTable(rt types.RouteTable) RouteTable {
	r := RouteTable{ID: awssdk.ToString(rt.RouteTableId), Tags: tags(rt.Tags)}
	for _, a := range rt.Associations {
		if a.AssociationState != nil && a.AssociationState.State != types.RouteTableAssociationStateCodeAssociated {
			continue
		}
		main := awssdk.ToBool(a.Main)
		r.Main = r.Main || main
		if a.SubnetId == nil && !main {
			continue // a gateway (edge) association
		}
		r.Associations = append(r.Associations, RouteTableAssociation{
			ID: awssdk.ToString(a.RouteTableAssociationId), SubnetID: awssdk.ToString(a.SubnetId), Main: main,
		})
	}
	for _, rr := range rt.Routes {
		r.Routes = append(r.Routes, Route{
			DestinationCIDRBlock:     awssdk.ToString(rr.DestinationCidrBlock),
			DestinationIPv6CIDRBlock: awssdk.ToString(rr.DestinationIpv6CidrBlock),
			DestinationPrefixListID:  awssdk.ToString(rr.DestinationPrefixListId),
			GatewayID:                awssdk.ToString(rr.GatewayId),
			NATGatewayID:             awssdk.ToString(rr.NatGatewayId),
			TransitGatewayID:         awssdk.ToString(rr.TransitGatewayId),
			VPCPeeringConnectionID:   awssdk.ToString(rr.VpcPeeringConnectionId),
			NetworkInterfaceID:       awssdk.ToString(rr.NetworkInterfaceId),
			Origin:                   string(rr.Origin),
			State:                    string(rr.State),
		})
	}
	return r
}

func (t *Tenant) subnets(ctx context.Context, vpcID string, ids []string, routeTables []RouteTable) ([]Subnet, error) {
	var out []Subnet
	// Unknown IDs fail the whole call with InvalidSubnetID.NotFound.
	p := ec2.NewDescribeSubnetsPaginator(t.ec2, &ec2.DescribeSubnetsInput{SubnetIds: ids})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, translate(err, "describe subnets")
		}
		for _, s := range page.Subnets {
			id := awssdk.ToString(s.SubnetId)
			if other := awssdk.ToString(s.VpcId); other != vpcID {
				return nil, fmt.Errorf("%w: subnet %s is in %s, not %s", ErrInvalid, id, other, vpcID)
			}
			rt := effectiveRouteTable(routeTables, id)
			out = append(out, Subnet{
				ID:                  id,
				CIDRBlock:           awssdk.ToString(s.CidrBlock),
				ZoneID:              awssdk.ToString(s.AvailabilityZoneId),
				ZoneName:            awssdk.ToString(s.AvailabilityZone),
				MapPublicIPOnLaunch: awssdk.ToBool(s.MapPublicIpOnLaunch),
				RouteTableID:        rt.ID,
				Role:                role(rt),
				Tags:                tags(s.Tags),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for _, id := range ids {
		if !slices.ContainsFunc(out, func(s Subnet) bool { return s.ID == id }) {
			return nil, fmt.Errorf("%w: subnet %s", ErrNotFound, id)
		}
	}
	return out, nil
}

// effectiveRouteTable is the table explicitly associated with the subnet,
// else the main one. The zero RouteTable when there is neither.
func effectiveRouteTable(tables []RouteTable, subnetID string) RouteTable {
	var main RouteTable
	for _, rt := range tables {
		for _, a := range rt.Associations {
			if a.SubnetID == subnetID {
				return rt
			}
		}
		if rt.Main {
			main = rt
		}
	}
	return main
}

func role(rt RouteTable) SubnetRole {
	hasDefault := false
	for _, r := range rt.Routes {
		if r.DestinationCIDRBlock != "0.0.0.0/0" {
			continue
		}
		hasDefault = true
		switch {
		case r.NATGatewayID != "":
			return SubnetRolePrivate
		case strings.HasPrefix(r.GatewayID, "igw-"):
			return SubnetRolePublic
		}
	}
	// No default route: a table that sends only specific CIDRs to the
	// internet gateway is still a public one.
	for _, r := range rt.Routes {
		if !hasDefault && r.DestinationCIDRBlock != "" && strings.HasPrefix(r.GatewayID, "igw-") {
			return SubnetRolePublic
		}
	}
	return SubnetRoleIsolated
}

func (t *Tenant) internetGateways(ctx context.Context, vpcID string) ([]InternetGateway, error) {
	var out []InternetGateway
	filters := []types.Filter{{Name: awssdk.String("attachment.vpc-id"), Values: []string{vpcID}}}
	p := ec2.NewDescribeInternetGatewaysPaginator(t.ec2, &ec2.DescribeInternetGatewaysInput{Filters: filters})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, translate(err, "describe internet gateways")
		}
		for _, g := range page.InternetGateways {
			out = append(out, InternetGateway{ID: awssdk.ToString(g.InternetGatewayId), Tags: tags(g.Tags)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (t *Tenant) natGateways(ctx context.Context, filters []types.Filter) ([]NATGateway, error) {
	var out []NATGateway
	p := ec2.NewDescribeNatGatewaysPaginator(t.ec2, &ec2.DescribeNatGatewaysInput{Filter: filters})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, translate(err, "describe NAT gateways")
		}
		for _, g := range page.NatGateways {
			if g.State == types.NatGatewayStateDeleted {
				continue
			}
			out = append(out, natGateway(g))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func natGateway(g types.NatGateway) NATGateway {
	n := NATGateway{
		ID:               awssdk.ToString(g.NatGatewayId),
		SubnetID:         awssdk.ToString(g.SubnetId),
		State:            string(g.State),
		ConnectivityType: string(g.ConnectivityType),
		Tags:             tags(g.Tags),
	}
	addrs := slices.Clone(g.NatGatewayAddresses)
	// Primary address first: it is the allocationId of the Crossplane
	// NATGateway; the others are secondaryAllocationIds.
	sort.SliceStable(addrs, func(i, j int) bool { return awssdk.ToBool(addrs[i].IsPrimary) && !awssdk.ToBool(addrs[j].IsPrimary) })
	for _, a := range addrs {
		if id := awssdk.ToString(a.AllocationId); id != "" {
			n.AllocationIDs = append(n.AllocationIDs, id)
		}
		if ip := awssdk.ToString(a.PublicIp); ip != "" {
			n.PublicIPs = append(n.PublicIPs, ip)
		}
	}
	return n
}

// defaultSecurityGroup reads the group every VPC has, named "default".
func (t *Tenant) defaultSecurityGroup(ctx context.Context, vpcID string) (SecurityGroup, error) {
	out, err := t.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{Filters: []types.Filter{
		{Name: awssdk.String("vpc-id"), Values: []string{vpcID}},
		{Name: awssdk.String("group-name"), Values: []string{"default"}},
	}})
	if err != nil {
		return SecurityGroup{}, translate(err, "describe the default security group")
	}
	if len(out.SecurityGroups) != 1 {
		return SecurityGroup{}, fmt.Errorf("aws: VPC %s has %d default security groups, want 1", vpcID, len(out.SecurityGroups))
	}
	g := out.SecurityGroups[0]
	return SecurityGroup{
		ID:          awssdk.ToString(g.GroupId),
		Name:        awssdk.ToString(g.GroupName),
		Description: awssdk.ToString(g.Description),
		Tags:        tags(g.Tags),
	}, nil
}

func tags(in []types.Tag) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for _, t := range in {
		out[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
	return out
}
