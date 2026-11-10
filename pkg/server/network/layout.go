package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	awscatalog "github.com/yindia/cluster-vending/pkg/constant/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Layout defaults (NetworkLayout).
const (
	DefaultCIDRBlock = "10.0.0.0/16"
	DefaultZoneCount = 3
)

// Default layout carving: private subnets are /(p+3) blocks 0..n-1 of the
// network; public subnets are /(p+6) blocks of the last /(p+3) block. The
// smallest AWS subnet is /28, so the default layout needs /22 or larger.
const (
	privateBits     = 3
	publicBits      = 6
	maxDefaultCIDR  = 22
	minSubnetPrefix = 16
	maxSubnetPrefix = 28
)

// errUnsupported marks layouts that are valid in the contract but not
// implemented yet (UNIMPLEMENTED).
var errUnsupported = errors.New("not supported yet")

// awsLayout is a NetworkLayout resolved for AWS.
type awsLayout struct {
	CIDRBlock string
	Subnets   []crossplaneaws.SubnetPlan
	NAT       crossplaneaws.NATMode
}

// planAWSLayout applies the layout defaults and checks what the schema
// cannot: subnets inside cidr_block without overlap, zones of the region.
func planAWSLayout(region string, l *cloudv1.NetworkLayout) (awsLayout, error) {
	out := awsLayout{CIDRBlock: l.GetCidrBlock()}
	if out.CIDRBlock == "" {
		out.CIDRBlock = DefaultCIDRBlock
	}
	network, err := netip.ParsePrefix(out.CIDRBlock)
	if err != nil {
		return awsLayout{}, fmt.Errorf("cidr_block %q: %w", out.CIDRBlock, err)
	}

	switch l.GetNatGatewayMode() {
	case cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_UNSPECIFIED, cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_SINGLE:
		out.NAT = crossplaneaws.NATSingle
	case cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE:
		out.NAT = crossplaneaws.NATPerZone
	default:
		return awsLayout{}, fmt.Errorf("nat_gateway_mode %s: %w", l.GetNatGatewayMode(), errUnsupported)
	}

	regionZones := awscatalog.ZoneIDs(awscatalog.RegionCode(region))
	if len(regionZones) == 0 {
		return awsLayout{}, fmt.Errorf("region %q has no known availability zones", region)
	}
	if len(l.GetSubnets()) > 0 {
		out.Subnets, err = explicitSubnets(network, region, l.GetSubnets())
	} else {
		out.Subnets, err = defaultSubnets(network, regionZones, int(l.GetAvailabilityZoneCount()))
	}
	if err != nil {
		return awsLayout{}, err
	}
	return out, nil
}

func defaultSubnets(network netip.Prefix, regionZones []awscatalog.ZoneID, count int) ([]crossplaneaws.SubnetPlan, error) {
	if count == 0 {
		count = DefaultZoneCount
	}
	if count > len(regionZones) {
		return nil, fmt.Errorf("availability_zone_count %d: the region has %d zones", count, len(regionZones))
	}
	if network.Bits() > maxDefaultCIDR {
		return nil, fmt.Errorf("the default subnet layout needs cidr_block /%d or larger, got /%d; list subnets explicitly",
			maxDefaultCIDR, network.Bits())
	}
	lastBlock := nthSubnet(network, privateBits, 1<<privateBits-1)
	var out []crossplaneaws.SubnetPlan
	for i, zone := range regionZones[:count] {
		out = append(out,
			crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPublic, ZoneID: string(zone),
				CIDR: nthSubnet(lastBlock, publicBits-privateBits, i).String()},
			crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPrivate, ZoneID: string(zone),
				CIDR: nthSubnet(network, privateBits, i).String()})
	}
	return out, nil
}

func explicitSubnets(network netip.Prefix, region string, subnets []*cloudv1.Subnet) ([]crossplaneaws.SubnetPlan, error) {
	out := make([]crossplaneaws.SubnetPlan, 0, len(subnets))
	var prefixes []netip.Prefix
	for _, s := range subnets {
		p, err := netip.ParsePrefix(s.GetCidrBlock())
		if err != nil {
			return nil, fmt.Errorf("subnet %q: %w", s.GetCidrBlock(), err)
		}
		if p.Bits() < network.Bits() || !network.Contains(p.Addr()) {
			return nil, fmt.Errorf("subnet %s is not inside cidr_block %s", p, network)
		}
		if p.Bits() < minSubnetPrefix || p.Bits() > maxSubnetPrefix {
			return nil, fmt.Errorf("subnet %s: AWS subnets are /%d to /%d", p, minSubnetPrefix, maxSubnetPrefix)
		}
		for _, q := range prefixes {
			if p.Overlaps(q) {
				return nil, fmt.Errorf("subnets %s and %s overlap", q, p)
			}
		}
		prefixes = append(prefixes, p)
		if zoneRegion, ok := awscatalog.LookupZone(s.GetZoneId()); !ok || string(zoneRegion) != region {
			return nil, fmt.Errorf("zone %q is not a zone ID of %s", s.GetZoneId(), region)
		}
		role := crossplaneaws.SubnetPrivate
		if s.GetRole() == cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC {
			role = crossplaneaws.SubnetPublic
		}
		out = append(out, crossplaneaws.SubnetPlan{Role: role, CIDR: p.String(), ZoneID: s.GetZoneId()})
	}
	return out, nil
}

// nthSubnet returns the i-th /(bits+newBits) block of p.
func nthSubnet(p netip.Prefix, newBits, i int) netip.Prefix {
	a := p.Addr().As4()
	bits := p.Bits() + newBits
	base := binary.BigEndian.Uint32(a[:]) + uint32(i)<<(32-bits) //nolint:gosec // i < 2^newBits, bits <= 28
	binary.BigEndian.PutUint32(a[:], base)
	return netip.PrefixFrom(netip.AddrFrom4(a), bits)
}
