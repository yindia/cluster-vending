package convert

import (
	"k8s.io/utils/ptr"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// NetworkSpecToAPI is the kind's spec for a contract spec. accountName is
// the CloudAccount object the spec's bootstrap_id stands for. The fields
// of the kind that are not in the contract's spec (displayName, tags,
// importFrom) are the caller's to set.
//
// An imported network has no spec.layout in the kind: the layout was read
// from the cloud, it is not something the platform builds, and it may be
// one the kind's rules for a layout would refuse. It is returned as the
// remainder. For a created network the remainder is nil.
func NetworkSpecToAPI(p *cloudv1.NetworkSpec, accountName string, imported bool) (cloudv1alpha1.NetworkSpec, *cloudv1.NetworkSpec) {
	spec := cloudv1alpha1.NetworkSpec{
		AccountRef: cloudv1alpha1.LocalReference{Name: accountName},
		Region:     p.GetRegion(),
		FlowLogs:   flowLogsToAPI(p.GetFlowLogs()),
	}
	if imported {
		if p.GetLayout() == nil {
			return spec, nil
		}
		return spec, &cloudv1.NetworkSpec{Layout: p.GetLayout()}
	}
	spec.Layout = LayoutToAPI(p.GetLayout())
	return spec, nil
}

// NetworkSpecFromAPI is the contract spec of a kind's spec. accountID is
// the row ID of the account the spec's accountRef names; remainder is
// what NetworkSpecToAPI returned for the stored spec (nil for none).
func NetworkSpecFromAPI(s cloudv1alpha1.NetworkSpec, accountID string, remainder *cloudv1.NetworkSpec) *cloudv1.NetworkSpec {
	p := &cloudv1.NetworkSpec{
		Region:      s.Region,
		BootstrapId: accountID,
		Layout:      layoutFromAPI(s.Layout),
		FlowLogs:    flowLogsFromAPI(s.FlowLogs),
	}
	if p.Layout == nil {
		p.Layout = remainder.GetLayout()
	}
	return p
}

// LayoutToAPI is a contract layout as the kinds spell it; nil for nil.
func LayoutToAPI(p *cloudv1.NetworkLayout) *cloudv1alpha1.NetworkLayout {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.NetworkLayout{
		CIDRBlock:      p.GetCidrBlock(),
		NATGatewayMode: cloudv1alpha1.NATGatewayMode(camel(p.GetNatGatewayMode(), prefixNAT)),
	}
	if n := p.GetAvailabilityZoneCount(); n != 0 {
		out.AvailabilityZoneCount = ptr.To(toInt32(n))
	}
	for _, s := range p.GetSubnets() {
		out.Subnets = append(out.Subnets, cloudv1alpha1.Subnet{
			Role:      cloudv1alpha1.SubnetRole(camel(s.GetRole(), prefixSubnetRole)),
			CIDRBlock: s.GetCidrBlock(), ZoneID: s.GetZoneId(),
		})
	}
	return out
}

func layoutFromAPI(l *cloudv1alpha1.NetworkLayout) *cloudv1.NetworkLayout {
	if l == nil {
		return nil
	}
	out := &cloudv1.NetworkLayout{
		CidrBlock:             l.CIDRBlock,
		AvailabilityZoneCount: toUint32(ptr.Deref(l.AvailabilityZoneCount, 0)),
		NatGatewayMode: cloudv1.NatGatewayMode(uncamel(
			cloudv1.NatGatewayMode(0).Descriptor(), prefixNAT, string(l.NATGatewayMode))),
	}
	for _, s := range l.Subnets {
		out.Subnets = append(out.Subnets, &cloudv1.Subnet{
			Role:      cloudv1.SubnetRole(uncamel(cloudv1.SubnetRole(0).Descriptor(), prefixSubnetRole, string(s.Role))),
			CidrBlock: s.CIDRBlock, ZoneId: s.ZoneID,
		})
	}
	return out
}

func flowLogsToAPI(p *cloudv1.NetworkFlowLogs) *cloudv1alpha1.NetworkFlowLogs {
	if p == nil {
		return nil
	}
	return &cloudv1alpha1.NetworkFlowLogs{
		Enabled:                    p.Enabled,
		TrafficType:                cloudv1alpha1.FlowLogTrafficType(camel(p.GetTrafficType(), prefixFlowLog)),
		RetentionDays:              toInt32(p.GetRetentionDays()),
		AggregationIntervalSeconds: toInt32(p.GetAggregationIntervalSeconds()),
	}
}

func flowLogsFromAPI(f *cloudv1alpha1.NetworkFlowLogs) *cloudv1.NetworkFlowLogs {
	if f == nil {
		return nil
	}
	return &cloudv1.NetworkFlowLogs{
		Enabled: f.Enabled,
		TrafficType: cloudv1.FlowLogTrafficType(uncamel(
			cloudv1.FlowLogTrafficType(0).Descriptor(), prefixFlowLog, string(f.TrafficType))),
		RetentionDays:              toUint32(f.RetentionDays),
		AggregationIntervalSeconds: toUint32(f.AggregationIntervalSeconds),
	}
}
