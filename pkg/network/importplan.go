package network

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// API versions of the resources a plan line names.
const (
	EC2APIVersion  = "ec2.aws.m.upbound.io/v1beta1"
	LogsAPIVersion = "cloudwatchlogs.aws.m.upbound.io/v1beta1"
	IAMAPIVersion  = "iam.aws.m.upbound.io/v1beta1"
)

const (
	minVPCPrefix = 16
	maxVPCPrefix = 24
	routeLocal   = "local"
)

// ImportPlan is what importing a discovered network does.
type ImportPlan struct {
	// Layout is the layout the discovered network is (the spec's).
	Layout *cloudv1.NetworkLayout
	// Resources are the Observe-only resources that import it
	// (crossplaneaws.ImportedNetworkResources renders them).
	Resources crossplaneaws.ImportedNetwork
	// Lines is the plan: one line per discovered resource.
	Lines []*cloudv1.ImportedResource
}

// Blocked reports whether a BLOCKING line prevents the import.
func (p ImportPlan) Blocked() bool {
	return slices.ContainsFunc(p.Lines, func(l *cloudv1.ImportedResource) bool {
		return l.GetDisposition() == cloudv1.Disposition_DISPOSITION_BLOCKING
	})
}

// NotImported lists the NOT_IMPORTED lines (a takeover needs them
// acknowledged).
func (p ImportPlan) NotImported() []*cloudv1.ImportedResource {
	var out []*cloudv1.ImportedResource
	for _, l := range p.Lines {
		if l.GetDisposition() == cloudv1.Disposition_DISPOSITION_NOT_IMPORTED {
			out = append(out, l)
		}
	}
	return out
}

// planner builds an ImportPlan; base carries the namespace, names and
// account of the network.
type planner struct {
	d    *awscloud.Network
	base crossplaneaws.ImportedNetwork
	plan ImportPlan
	// subnetNames maps subnet IDs to managed resource names.
	subnetNames map[string]string
	natKeys     map[string]string
}

// PlanImport maps a discovered network onto the contract: the layout it
// is (spec), the Observe-only resources that import it, and one plan line
// per resource. Lines are MATCHES, WILL_UPDATE (subnets missing their load
// balancer tag), CARRIED_OVER (the VPC's flow logs to CloudWatch Logs with
// their log group and delivery role, the default network ACL and the
// network ACLs of the imported subnets with their rules, routes of specific
// CIDRs to the internet gateway), NOT_IMPORTED (outside this version: the
// main route table, other routes, unused NAT gateways, the default security
// group, other flow logs and network ACLs) or BLOCKING (not expressible in
// the contract).
func PlanImport(d *awscloud.Network, base crossplaneaws.ImportedNetwork) ImportPlan {
	p := &planner{d: d, base: base, subnetNames: map[string]string{}, natKeys: map[string]string{}}
	p.plan.Layout = &cloudv1.NetworkLayout{CidrBlock: d.VPC.CIDRBlock}
	p.plan.Resources = base
	p.vpc()
	p.subnets()
	p.routeTables()
	p.natMode()
	p.networkACLs()
	p.flowLogs()
	p.extras()
	return p.plan
}

func (p *planner) line(kind, name, externalName, specPath string, disposition cloudv1.Disposition, detail string) {
	p.lineOf(EC2APIVersion, kind, name, externalName, specPath, disposition, detail)
}

func (p *planner) lineOf(apiVersion, kind, name, externalName, specPath string, disposition cloudv1.Disposition, detail string) {
	p.plan.Lines = append(p.plan.Lines, &cloudv1.ImportedResource{ApiVersion: apiVersion, Kind: kind, Name: name,
		ExternalName: externalName, SpecPath: specPath, Disposition: disposition, Detail: detail})
}

func (p *planner) vpc() {
	v := p.d.VPC
	p.plan.Resources.VPC = crossplaneaws.ImportedVPC{ID: v.ID, CIDRBlock: v.CIDRBlock, Tags: v.Tags}
	disposition, detail := cloudv1.Disposition_DISPOSITION_MATCHES, ""
	if prefix, err := netip.ParsePrefix(v.CIDRBlock); err != nil || prefix.Bits() < minVPCPrefix || prefix.Bits() > maxVPCPrefix {
		disposition, detail = cloudv1.Disposition_DISPOSITION_BLOCKING,
			fmt.Sprintf("VPC CIDR %s: networks are /%d to /%d", v.CIDRBlock, minVPCPrefix, maxVPCPrefix)
	}
	p.line("VPC", p.base.Prefix+"-vpc", v.ID, "spec.layout.cidr_block", disposition, detail)
	for _, cidr := range v.SecondaryCIDRBlocks {
		p.line("VPCIpv4CidrBlockAssociation", "", cidr, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
			"secondary VPC CIDR blocks are not imported yet")
	}
	if len(p.d.InternetGateways) > 0 {
		g := p.d.InternetGateways[0]
		p.plan.Resources.InternetGateway = &crossplaneaws.ImportedInternetGateway{ID: g.ID, Tags: g.Tags}
		p.line("InternetGateway", p.base.Prefix+"-igw", g.ID, "", cloudv1.Disposition_DISPOSITION_MATCHES, "")
	}
}

// subnets imports the named subnets; their role comes from their routing.
func (p *planner) subnets() {
	seen := map[string]int{}
	privateZones := map[string]bool{}
	public := 0
	for i, s := range p.d.Subnets {
		role, ok := map[awscloud.SubnetRole]crossplaneaws.SubnetRole{
			awscloud.SubnetRolePublic: crossplaneaws.SubnetPublic, awscloud.SubnetRolePrivate: crossplaneaws.SubnetPrivate,
		}[s.Role]
		path := fmt.Sprintf("spec.layout.subnets[%d]", i)
		if !ok {
			p.line("Subnet", "", s.ID, path, cloudv1.Disposition_DISPOSITION_BLOCKING,
				"no default route through an internet or NAT gateway: neither PUBLIC nor PRIVATE")
			continue
		}
		suffix := string(role) + "-" + s.ZoneID
		if seen[suffix]++; seen[suffix] > 1 {
			suffix = fmt.Sprintf("%s-%d", suffix, seen[suffix])
		}
		name := p.base.Prefix + "-" + suffix
		p.subnetNames[s.ID] = name
		p.plan.Resources.Subnets = append(p.plan.Resources.Subnets, crossplaneaws.ImportedSubnet{
			Name: name, ID: s.ID, CIDRBlock: s.CIDRBlock, ZoneID: s.ZoneID, Role: role,
			MapPublicIPOnLaunch: s.MapPublicIPOnLaunch, Tags: s.Tags})
		p.plan.Layout.Subnets = append(p.plan.Layout.Subnets, &cloudv1.Subnet{Role: subnetRole(role), CidrBlock: s.CIDRBlock, ZoneId: s.ZoneID})
		p.subnetLine(name, s, role, path)
		if role == crossplaneaws.SubnetPublic {
			public++
		} else {
			privateZones[s.ZoneID] = true
		}
	}
	if len(privateZones) < 2 {
		p.line("Subnet", "", "", "spec.layout.subnets", cloudv1.Disposition_DISPOSITION_BLOCKING,
			"EKS needs PRIVATE subnets in at least two zones")
	}
	if public == 0 {
		p.line("Subnet", "", "", "spec.layout.subnets", cloudv1.Disposition_DISPOSITION_BLOCKING,
			"at least one PUBLIC subnet is required (NAT and internet-facing load balancers)")
	}
}

func subnetRole(r crossplaneaws.SubnetRole) cloudv1.SubnetRole {
	if r == crossplaneaws.SubnetPublic {
		return cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC
	}
	return cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE
}

// subnetLine is MATCHES, or WILL_UPDATE when the subnet lacks the tag EKS
// load balancers select subnets by (the takeover adds it).
func (p *planner) subnetLine(name string, s awscloud.Subnet, role crossplaneaws.SubnetRole, path string) {
	tag := crossplaneaws.TagInternalELB
	if role == crossplaneaws.SubnetPublic {
		tag = crossplaneaws.TagELB
	}
	if s.Tags[tag] == "1" {
		p.line("Subnet", name, s.ID, path, cloudv1.Disposition_DISPOSITION_MATCHES, "")
		return
	}
	p.line("Subnet", name, s.ID, path, cloudv1.Disposition_DISPOSITION_WILL_UPDATE, "adds tag "+tag+"=1")
}

// routeTables imports the route tables of the named subnets with their
// associations and default routes, and the NAT gateways those routes use.
func (p *planner) routeTables() {
	keys := map[string]int{}
	mainSeen := false
	for _, rt := range p.d.RouteTables {
		var assocs []crossplaneaws.ImportedAssociation
		for _, a := range rt.Associations {
			if name, ok := p.subnetNames[a.SubnetID]; ok && !a.Main {
				assocs = append(assocs, crossplaneaws.ImportedAssociation{ID: a.ID, SubnetID: a.SubnetID, SubnetName: name})
			}
		}
		if rt.Main {
			if !mainSeen && p.usesMain(rt) {
				mainSeen = true
				p.line("DefaultRouteTable", "", rt.ID, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
					"the VPC's main route table is not imported yet; subnets without their own route table keep using it")
			}
			continue
		}
		if len(assocs) == 0 {
			continue // not a route table of the imported subnets
		}
		p.routeTable(rt, assocs, keys)
	}
}

// usesMain reports whether an imported subnet routes through the main
// table (no association of its own).
func (p *planner) usesMain(main awscloud.RouteTable) bool {
	for _, s := range p.d.Subnets {
		if _, imported := p.subnetNames[s.ID]; imported && s.RouteTableID == main.ID {
			return true
		}
	}
	return false
}

func (p *planner) routeTable(rt awscloud.RouteTable, assocs []crossplaneaws.ImportedAssociation, keys map[string]int) {
	key := p.routeTableKey(assocs)
	if keys[key]++; keys[key] > 1 {
		key = fmt.Sprintf("%s-%d", key, keys[key])
	}
	t := crossplaneaws.ImportedRouteTable{Key: key, ID: rt.ID, Tags: rt.Tags, Associations: assocs}
	p.line("RouteTable", p.base.Prefix+"-rt-"+key, rt.ID, "", cloudv1.Disposition_DISPOSITION_MATCHES, "")
	for _, a := range assocs {
		p.line("RouteTableAssociation", a.SubnetName+"-rta", a.ID, "", cloudv1.Disposition_DISPOSITION_MATCHES, "")
	}
	for _, r := range rt.Routes {
		switch {
		case r.GatewayID == routeLocal:
		case r.DestinationCIDRBlock == "0.0.0.0/0" && (r.NATGatewayID != "" || r.GatewayID != "") && t.DefaultRoute == nil:
			t.DefaultRoute = &crossplaneaws.ImportedRoute{GatewayID: r.GatewayID, NATGatewayID: r.NATGatewayID}
			p.line("Route", p.base.Prefix+"-route-"+key, crossplaneaws.RouteExternalName(rt.ID, "0.0.0.0/0"), "",
				cloudv1.Disposition_DISPOSITION_MATCHES, "")
			if r.NATGatewayID != "" {
				p.useNAT(r.NATGatewayID)
			}
		case r.DestinationCIDRBlock != "" && r.DestinationCIDRBlock != "0.0.0.0/0" && strings.HasPrefix(r.GatewayID, "igw-"):
			// A public table that routes specific CIDRs: kept as it is.
			t.GatewayRoutes = append(t.GatewayRoutes, crossplaneaws.ImportedRoute{GatewayID: r.GatewayID, Destination: r.DestinationCIDRBlock})
			p.line("Route", fmt.Sprintf("%s-route-%s-gw-%d", p.base.Prefix, key, len(t.GatewayRoutes)),
				crossplaneaws.RouteExternalName(rt.ID, r.DestinationCIDRBlock), "", cloudv1.Disposition_DISPOSITION_CARRIED_OVER,
				fmt.Sprintf("route %s to the internet gateway, kept as it is", r.DestinationCIDRBlock))
		default:
			p.line("Route", "", rt.ID+"_"+routeDestination(r), "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
				fmt.Sprintf("route %s -> %s is not imported yet", routeDestination(r), routeTarget(r)))
		}
	}
	p.plan.Resources.RouteTables = append(p.plan.Resources.RouteTables, t)
}

// routeTableKey is "public" for a public table, else "private-<zone of its
// first subnet>".
func (p *planner) routeTableKey(assocs []crossplaneaws.ImportedAssociation) string {
	for _, s := range p.plan.Resources.Subnets {
		if s.ID == assocs[0].SubnetID {
			if s.Role == crossplaneaws.SubnetPublic {
				return "public"
			}
			return "private-" + s.ZoneID
		}
	}
	return "imported"
}

func routeDestination(r awscloud.Route) string {
	for _, d := range []string{r.DestinationCIDRBlock, r.DestinationIPv6CIDRBlock, r.DestinationPrefixListID} {
		if d != "" {
			return d
		}
	}
	return "?"
}

func routeTarget(r awscloud.Route) string {
	for _, t := range []string{r.GatewayID, r.NATGatewayID, r.TransitGatewayID, r.VPCPeeringConnectionID, r.NetworkInterfaceID} {
		if t != "" {
			return t
		}
	}
	return "?"
}

// useNAT imports a NAT gateway (once) with its primary Elastic IP.
func (p *planner) useNAT(id string) {
	if _, done := p.natKeys[id]; done {
		return
	}
	for _, g := range p.d.NATGateways {
		if g.ID != id {
			continue
		}
		key := p.natKey(g)
		p.natKeys[id] = key
		alloc := ""
		if len(g.AllocationIDs) > 0 {
			alloc = g.AllocationIDs[0]
		}
		// A private NAT gateway has no Elastic IP.
		private := g.ConnectivityType == "private" || alloc == ""
		p.plan.Resources.NATGateways = append(p.plan.Resources.NATGateways,
			crossplaneaws.ImportedNAT{Key: key, ID: g.ID, SubnetID: g.SubnetID, AllocationID: alloc, Private: private, Tags: g.Tags})
		if !private {
			p.line("EIP", p.base.Prefix+"-eip-"+key, alloc, "", cloudv1.Disposition_DISPOSITION_MATCHES, "")
		}
		p.line("NATGateway", p.base.Prefix+"-nat-"+key, g.ID, "spec.layout.nat_gateway_mode", cloudv1.Disposition_DISPOSITION_MATCHES, "")
		for _, extra := range g.AllocationIDs[min(1, len(g.AllocationIDs)):] {
			p.line("EIP", "", extra, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
				"secondary NAT gateway addresses are not imported yet")
		}
	}
}

// natKey is the zone of the NAT gateway's subnet when that subnet is
// imported, else its ID; "-2", "-3", ... for a second NAT in one zone.
func (p *planner) natKey(g awscloud.NATGateway) string {
	key := g.ID
	for _, s := range p.plan.Resources.Subnets {
		if s.ID == g.SubnetID {
			key = s.ZoneID
		}
	}
	taken := map[string]bool{}
	for _, k := range p.natKeys {
		taken[k] = true
	}
	for n := 2; taken[key]; n++ {
		key = fmt.Sprintf("%s-%d", key, n)
	}
	return key
}

// natMode is SINGLE with one NAT gateway in use, PER_ZONE with more.
func (p *planner) natMode() {
	p.plan.Layout.NatGatewayMode = cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_SINGLE
	if len(p.natKeys) > 1 {
		p.plan.Layout.NatGatewayMode = cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE
	}
}

// networkACLs imports, as CARRIED_OVER, the VPC's default network ACL (a
// DefaultNetworkACL with its rules inline, never deleted) and the custom
// network ACLs associated with an imported subnet, each with a
// NetworkACLRule per rule. Subnet associations are the ACL's subnetIds,
// all of them: the provider has no association kind. Other network ACLs
// are NOT_IMPORTED.
func (p *planner) networkACLs() {
	custom := 0
	for _, a := range p.d.NetworkACLs {
		acl := crossplaneaws.ImportedNetworkACL{ID: a.ID, Default: a.Default, SubnetIDs: a.SubnetIDs, Tags: a.Tags}
		switch {
		case a.Default:
			acl.Name = p.base.Prefix + "-default-nacl"
			p.line("DefaultNetworkACL", acl.Name, a.ID, "", cloudv1.Disposition_DISPOSITION_CARRIED_OVER,
				"the VPC's default network ACL with its rules: owned without Delete, it goes with the VPC")
		case !slices.ContainsFunc(a.SubnetIDs, func(id string) bool { return p.subnetNames[id] != "" }):
			p.line("NetworkACL", "", a.ID, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
				"no imported subnet is associated with this network ACL")
			continue
		default:
			custom++
			acl.Name = numbered(p.base.Prefix+"-nacl", custom)
			p.line("NetworkACL", acl.Name, a.ID, "", cloudv1.Disposition_DISPOSITION_CARRIED_OVER,
				fmt.Sprintf("associated with %d subnets", len(a.SubnetIDs)))
		}
		for _, r := range a.Rules {
			rule := crossplaneaws.ImportedNetworkACLRule{Number: r.Number, Egress: r.Egress, Protocol: r.Protocol, Action: r.Action,
				CIDRBlock: r.CIDRBlock, IPv6CIDRBlock: r.IPv6CIDRBlock, FromPort: r.FromPort, ToPort: r.ToPort,
				ICMPType: r.ICMPType, ICMPCode: r.ICMPCode}
			if !a.Default {
				direction := "in"
				if r.Egress {
					direction = "out"
				}
				rule.Name = fmt.Sprintf("%s-%s-%d", acl.Name, direction, r.Number)
				p.line("NetworkACLRule", rule.Name, crossplaneaws.NetworkACLRuleExternalName(a.ID, r.Number, r.Egress, r.Protocol),
					"", cloudv1.Disposition_DISPOSITION_CARRIED_OVER, "")
			}
			acl.Rules = append(acl.Rules, rule)
		}
		p.plan.Resources.NetworkACLs = append(p.plan.Resources.NetworkACLs, acl)
	}
}

// extras lists what discovery found that this version leaves alone.
func (p *planner) extras() {
	for _, g := range p.d.NATGateways {
		if _, used := p.natKeys[g.ID]; !used {
			p.line("NATGateway", "", g.ID, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
				"no imported subnet routes through this NAT gateway")
		}
	}
	for _, g := range p.d.InternetGateways[min(1, len(p.d.InternetGateways)):] {
		p.line("InternetGateway", "", g.ID, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED, "a VPC has one internet gateway")
	}
	if sg := p.d.DefaultSecurityGroup; sg.ID != "" {
		p.line("DefaultSecurityGroup", "", sg.ID, "", cloudv1.Disposition_DISPOSITION_NOT_IMPORTED,
			"the default security group is not imported yet")
	}
}
