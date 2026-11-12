package aws

import (
	"context"
	"sort"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// ClusterSecurityGroup is a security group an EKS cluster's control plane
// uses next to the one EKS created (vpcConfig.securityGroupIds), with its
// rules.
type ClusterSecurityGroup struct {
	ID, Name, Description, VPCID string
	Tags                         map[string]string
	// Rules are ordered by ID.
	Rules []SecurityGroupRule
	// Shared: network interfaces other than the cluster's use the group
	// too (or that could not be checked), so it is not the cluster's alone.
	Shared bool
	// Unreadable says why the group or its rules could not be read.
	Unreadable string
}

// SecurityGroupRule is one rule of a security group (an sgr- ID).
type SecurityGroupRule struct {
	ID     string
	Egress bool
	// Protocol is "tcp", "udp", "icmp", a protocol number or "-1" (all).
	Protocol string
	// FromPort and ToPort are nil for rules without a port range.
	FromPort, ToPort                                    *int
	CIDRIPv4, CIDRIPv6, PrefixListID, ReferencedGroupID string
	Description                                         string
	Tags                                                map[string]string
}

// eksInterfacePrefix starts the description of the network interfaces EKS
// creates for a cluster's control plane: "Amazon EKS <cluster name>".
const eksInterfacePrefix = "Amazon EKS "

// clusterSecurityGroups reads the security groups ids with their rules,
// and whether anything but the cluster's own network interfaces uses
// them. A group that cannot be read does not fail discovery: it says why
// (Unreadable).
//
// Calls: ec2:DescribeSecurityGroups, DescribeSecurityGroupRules,
// DescribeNetworkInterfaces.
func (t *Tenant) clusterSecurityGroups(ctx context.Context, ids []string, clusterName string) []ClusterSecurityGroup {
	out := make([]ClusterSecurityGroup, 0, len(ids))
	for _, id := range ids {
		out = append(out, t.clusterSecurityGroup(ctx, id, clusterName))
	}
	return out
}

func (t *Tenant) clusterSecurityGroup(ctx context.Context, id, clusterName string) ClusterSecurityGroup {
	g := ClusterSecurityGroup{ID: id}
	groups, err := t.ec2.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{id}})
	if err != nil || len(groups.SecurityGroups) != 1 {
		g.Unreadable = "the security group cannot be read"
		if err != nil {
			g.Unreadable = translate(err, "describe security group "+id).Error()
		}
		return g
	}
	sg := groups.SecurityGroups[0]
	g.Name, g.Description, g.VPCID, g.Tags = awssdk.ToString(sg.GroupName), awssdk.ToString(sg.Description), awssdk.ToString(sg.VpcId), tags(sg.Tags)
	byGroup := []types.Filter{{Name: awssdk.String("group-id"), Values: []string{id}}}
	rules := ec2.NewDescribeSecurityGroupRulesPaginator(t.ec2, &ec2.DescribeSecurityGroupRulesInput{Filters: byGroup})
	for rules.HasMorePages() {
		page, err := rules.NextPage(ctx)
		if err != nil {
			g.Unreadable = translate(err, "describe the rules of security group "+id).Error()
			return g
		}
		for _, r := range page.SecurityGroupRules {
			g.Rules = append(g.Rules, securityGroupRule(r))
		}
	}
	sort.Slice(g.Rules, func(i, j int) bool { return g.Rules[i].ID < g.Rules[j].ID })
	g.Shared = t.usedByOthers(ctx, byGroup, clusterName)
	return g
}

func securityGroupRule(r types.SecurityGroupRule) SecurityGroupRule {
	out := SecurityGroupRule{ID: awssdk.ToString(r.SecurityGroupRuleId), Egress: awssdk.ToBool(r.IsEgress),
		Protocol: awssdk.ToString(r.IpProtocol), CIDRIPv4: awssdk.ToString(r.CidrIpv4), CIDRIPv6: awssdk.ToString(r.CidrIpv6),
		PrefixListID: awssdk.ToString(r.PrefixListId), Description: awssdk.ToString(r.Description), Tags: tags(r.Tags)}
	if r.ReferencedGroupInfo != nil {
		out.ReferencedGroupID = awssdk.ToString(r.ReferencedGroupInfo.GroupId)
	}
	// AWS reports -1 / -1 for "all ports"; a rule for every protocol has
	// no port range at all.
	if out.Protocol != "-1" && r.FromPort != nil && r.ToPort != nil {
		out.FromPort, out.ToPort = awssdk.Int(int(*r.FromPort)), awssdk.Int(int(*r.ToPort))
	}
	return out
}

// usedByOthers reports whether a network interface that is not the
// cluster's control plane's uses the group. When that cannot be read the
// group counts as shared: it is then never deleted with the cluster.
func (t *Tenant) usedByOthers(ctx context.Context, byGroup []types.Filter, clusterName string) bool {
	p := ec2.NewDescribeNetworkInterfacesPaginator(t.ec2, &ec2.DescribeNetworkInterfacesInput{Filters: byGroup})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return true
		}
		for _, eni := range page.NetworkInterfaces {
			// Exactly: "Amazon EKS prod-2" is another cluster's, not prod's.
			if awssdk.ToString(eni.Description) != eksInterfacePrefix+clusterName {
				return true
			}
		}
	}
	return false
}
