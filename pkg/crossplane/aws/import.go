package aws

import (
	"fmt"
	"hash/crc32"
	"maps"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
)

// ExternalNameAnnotation is the cloud ID of a managed resource.
const ExternalNameAnnotation = "crossplane.io/external-name"

// Management policies.
var (
	// PolicyObserve only reads the cloud resource (import).
	PolicyObserve = xpv2.ManagementPolicies{xpv2.ManagementActionObserve}
	// PolicyAll owns the cloud resource (takeover).
	PolicyAll = xpv2.ManagementPolicies{xpv2.ManagementActionAll}
)

// ImportedNetwork is an existing network to import: what discovery found,
// resolved to names. Everything becomes an Observe-only managed resource
// whose forProvider holds the current values (literal cloud IDs, existing
// tags), so a later takeover changes nothing it was not asked to.
type ImportedNetwork struct {
	Namespace, ProviderConfigName, TenantID string
	NetworkID, NetworkName, Prefix, Region  string

	VPC             ImportedVPC
	InternetGateway *ImportedInternetGateway
	Subnets         []ImportedSubnet
	NATGateways     []ImportedNAT
	RouteTables     []ImportedRouteTable
	// FlowLogs are the VPC's flow logs to CloudWatch Logs (CARRIED_OVER).
	FlowLogs []ImportedFlowLog
	// NetworkACLs are the VPC's default network ACL and the custom ones of
	// the imported subnets (CARRIED_OVER).
	NetworkACLs []ImportedNetworkACL
}

// ImportedVPC is the network's VPC.
type ImportedVPC struct {
	ID, CIDRBlock string
	Tags          map[string]string
}

// ImportedInternetGateway is the VPC's internet gateway.
type ImportedInternetGateway struct {
	ID   string
	Tags map[string]string
}

// ImportedSubnet is one subnet; Name is its managed resource name.
type ImportedSubnet struct {
	Name, ID, CIDRBlock, ZoneID string
	Role                        SubnetRole
	MapPublicIPOnLaunch         bool
	Tags                        map[string]string
}

// ImportedNAT is a NAT gateway and its Elastic IP; Key names both
// ("<prefix>-nat-<key>", "<prefix>-eip-<key>").
type ImportedNAT struct {
	Key, ID, SubnetID, AllocationID string
	Tags                            map[string]string
}

// ImportedRouteTable is a route table with its subnet associations, its
// default route and its routes of specific CIDRs to the internet gateway;
// Key names it ("<prefix>-rt-<key>").
type ImportedRouteTable struct {
	Key, ID      string
	Tags         map[string]string
	Associations []ImportedAssociation
	DefaultRoute *ImportedRoute
	// GatewayRoutes are named "<prefix>-route-<key>-<n>", n from 1.
	GatewayRoutes []ImportedRoute
}

// ImportedAssociation associates SubnetName (an ImportedSubnet) with the
// table.
type ImportedAssociation struct{ ID, SubnetID, SubnetName string }

// ImportedRoute is a route to an internet or NAT gateway. Destination is
// its IPv4 CIDR; empty means 0.0.0.0/0.
type ImportedRoute struct{ GatewayID, NATGatewayID, Destination string }

// RouteExternalName is the provider's ID of a route:
// "r-<route table ID><crc32 of the destination>".
func RouteExternalName(routeTableID, destination string) string {
	return fmt.Sprintf("r-%s%d", routeTableID, crc32.ChecksumIEEE([]byte(destination)))
}

// ImportedNetworkResources renders n as Observe-only managed resources, in
// creation order.
func ImportedNetworkResources(n ImportedNetwork) []client.Object {
	b := importBuilder{n: n}
	out := []client.Object{b.vpc()}
	if g := n.InternetGateway; g != nil {
		out = append(out, b.internetGateway(*g))
	}
	for _, s := range n.Subnets {
		out = append(out, b.subnet(s))
	}
	for _, g := range n.NATGateways {
		out = append(out, b.eip(g), b.nat(g))
	}
	for _, rt := range n.RouteTables {
		out = append(out, b.routeTable(rt))
	}
	for _, rt := range n.RouteTables {
		if rt.DefaultRoute != nil {
			out = append(out, b.route(rt, "route-"+rt.Key, *rt.DefaultRoute))
		}
		for i, r := range rt.GatewayRoutes {
			out = append(out, b.route(rt, fmt.Sprintf("route-%s-%d", rt.Key, i+1), r))
		}
	}
	for _, rt := range n.RouteTables {
		for _, a := range rt.Associations {
			out = append(out, b.association(rt, a))
		}
	}
	out = append(out, b.networkACLs()...)
	return append(out, b.flowLogs()...)
}

type importBuilder struct{ n ImportedNetwork }

func (b importBuilder) meta(name, externalName string, extra map[string]string) metav1.ObjectMeta {
	labels := crossplane.NetworkLabels(b.n.TenantID, b.n.NetworkID)
	maps.Copy(labels, extra)
	return metav1.ObjectMeta{Name: name, Namespace: b.n.Namespace, Labels: labels,
		Annotations: map[string]string{ExternalNameAnnotation: externalName}}
}

func (b importBuilder) spec() xpv2.ManagedResourceSpec {
	return xpv2.ManagedResourceSpec{ProviderConfigReference: ProviderConfigRef(b.n.ProviderConfigName),
		ManagementPolicies: PolicyObserve}
}

func (b importBuilder) name(suffix string) string { return b.n.Prefix + "-" + suffix }

func tagPtrs(tags map[string]string) map[string]*string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]*string, len(tags))
	for k, v := range tags {
		out[k] = ptr.To(v)
	}
	return out
}

func (b importBuilder) vpc() *awsec2.VPC {
	v := b.n.VPC
	return &awsec2.VPC{
		ObjectMeta: b.meta(b.name("vpc"), v.ID, nil),
		Spec: awsec2.VPCSpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.VPCParameters_2{
			Region: ptr.To(b.n.Region), CidrBlock: ptr.To(v.CIDRBlock), Tags: tagPtrs(v.Tags),
		}},
	}
}

func (b importBuilder) internetGateway(g ImportedInternetGateway) *awsec2.InternetGateway {
	return &awsec2.InternetGateway{
		ObjectMeta: b.meta(b.name("igw"), g.ID, nil),
		Spec: awsec2.InternetGatewaySpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.InternetGatewayParameters_2{
			Region: ptr.To(b.n.Region), VPCID: ptr.To(b.n.VPC.ID), Tags: tagPtrs(g.Tags),
		}},
	}
}

func (b importBuilder) subnet(s ImportedSubnet) *awsec2.Subnet {
	return &awsec2.Subnet{
		ObjectMeta: b.meta(s.Name, s.ID, map[string]string{crossplane.LabelSubnetRole: string(s.Role), crossplane.LabelZone: s.ZoneID}),
		Spec: awsec2.SubnetSpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.SubnetParameters_2{
			Region: ptr.To(b.n.Region), VPCID: ptr.To(b.n.VPC.ID), CidrBlock: ptr.To(s.CIDRBlock),
			AvailabilityZoneID: ptr.To(s.ZoneID), MapPublicIPOnLaunch: ptr.To(s.MapPublicIPOnLaunch), Tags: tagPtrs(s.Tags),
		}},
	}
}

func (b importBuilder) eip(g ImportedNAT) *awsec2.EIP {
	return &awsec2.EIP{
		ObjectMeta: b.meta(b.name("eip-"+g.Key), g.AllocationID, nil),
		Spec: awsec2.EIPSpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.EIPParameters{
			Region: ptr.To(b.n.Region), Domain: ptr.To("vpc"),
		}},
	}
}

func (b importBuilder) nat(g ImportedNAT) *awsec2.NATGateway {
	return &awsec2.NATGateway{
		ObjectMeta: b.meta(b.name("nat-"+g.Key), g.ID, nil),
		Spec: awsec2.NATGatewaySpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.NATGatewayParameters_2{
			Region: ptr.To(b.n.Region), SubnetID: ptr.To(g.SubnetID), AllocationID: ptr.To(g.AllocationID),
			ConnectivityType: ptr.To("public"), Tags: tagPtrs(g.Tags),
		}},
	}
}

func (b importBuilder) routeTable(rt ImportedRouteTable) *awsec2.RouteTable {
	return &awsec2.RouteTable{
		ObjectMeta: b.meta(b.name("rt-"+rt.Key), rt.ID, nil),
		Spec: awsec2.RouteTableSpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.RouteTableParameters_2{
			Region: ptr.To(b.n.Region), VPCID: ptr.To(b.n.VPC.ID), Tags: tagPtrs(rt.Tags),
		}},
	}
}

func (b importBuilder) route(rt ImportedRouteTable, suffix string, r ImportedRoute) *awsec2.Route {
	destination := r.Destination
	if destination == "" {
		destination = anyIPv4
	}
	params := awsec2.RouteParameters_2{
		Region: ptr.To(b.n.Region), RouteTableID: ptr.To(rt.ID), DestinationCidrBlock: ptr.To(destination),
	}
	if id := r.GatewayID; id != "" {
		params.GatewayID = ptr.To(id)
	}
	if id := r.NATGatewayID; id != "" {
		params.NATGatewayID = ptr.To(id)
	}
	return &awsec2.Route{
		ObjectMeta: b.meta(b.name(suffix), RouteExternalName(rt.ID, destination), nil),
		Spec:       awsec2.RouteSpec{ManagedResourceSpec: b.spec(), ForProvider: params},
	}
}

func (b importBuilder) association(rt ImportedRouteTable, a ImportedAssociation) *awsec2.RouteTableAssociation {
	return &awsec2.RouteTableAssociation{
		ObjectMeta: b.meta(a.SubnetName+"-rta", a.ID, nil),
		Spec: awsec2.RouteTableAssociationSpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.RouteTableAssociationParameters{
			Region: ptr.To(b.n.Region), SubnetID: ptr.To(a.SubnetID), RouteTableID: ptr.To(rt.ID),
		}},
	}
}
