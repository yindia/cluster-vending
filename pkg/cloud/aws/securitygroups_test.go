package aws

import (
	"context"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func (f *fakeEC2) DescribeSecurityGroupRules(_ context.Context, in *ec2.DescribeSecurityGroupRulesInput, _ ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupRulesOutput, error) {
	f.record(in.Filters)
	return &ec2.DescribeSecurityGroupRulesOutput{SecurityGroupRules: []types.SecurityGroupRule{
		{SecurityGroupRuleId: awssdk.String("sgr-2"), IsEgress: awssdk.Bool(true), IpProtocol: awssdk.String("-1"),
			FromPort: awssdk.Int32(-1), ToPort: awssdk.Int32(-1), CidrIpv4: awssdk.String("0.0.0.0/0")},
		{SecurityGroupRuleId: awssdk.String("sgr-1"), IsEgress: awssdk.Bool(false), IpProtocol: awssdk.String("tcp"),
			FromPort: awssdk.Int32(443), ToPort: awssdk.Int32(443), Description: awssdk.String("nodes"),
			ReferencedGroupInfo: &types.ReferencedSecurityGroup{GroupId: awssdk.String("sg-nodes")}},
	}}, nil
}

func (f *fakeEC2) DescribeNetworkInterfaces(_ context.Context, in *ec2.DescribeNetworkInterfacesInput, _ ...func(*ec2.Options)) (*ec2.DescribeNetworkInterfacesOutput, error) {
	f.record(in.Filters)
	out := &ec2.DescribeNetworkInterfacesOutput{}
	for _, d := range f.interfaces {
		out.NetworkInterfaces = append(out.NetworkInterfaces, types.NetworkInterface{Description: awssdk.String(d)})
	}
	return out, nil
}

func TestClusterSecurityGroups(t *testing.T) {
	for _, tt := range []struct {
		name       string
		interfaces []string
		shared     bool
	}{
		{"only the cluster's interfaces", []string{"Amazon EKS tf", "Amazon EKS tf"}, false},
		{"unused", nil, false},
		{"another cluster's interface", []string{"Amazon EKS tf", "Amazon EKS other"}, true},
		{"a cluster whose name starts with this one's", []string{"Amazon EKS tf", "Amazon EKS tf-2"}, true},
		{"a load balancer", []string{"ELB app/x/1"}, true},
	} {
		api := &fakeEC2{interfaces: tt.interfaces}
		groups := fakeTenant(api).clusterSecurityGroups(t.Context(), []string{"sg-1"}, "tf")
		if len(groups) != 1 || groups[0].Unreadable != "" || groups[0].Shared != tt.shared {
			t.Errorf("%s: %+v, want shared %t", tt.name, groups, tt.shared)
			continue
		}
		g := groups[0]
		if g.Name != "default" || len(g.Rules) != 2 || g.Rules[0].ID != "sgr-1" {
			t.Errorf("%s: group = %+v, want its rules by ID", tt.name, g)
		}
		in, out := g.Rules[0], g.Rules[1]
		if in.Egress || in.Protocol != "tcp" || *in.FromPort != 443 || in.ReferencedGroupID != "sg-nodes" || in.Description != "nodes" {
			t.Errorf("%s: ingress rule = %+v", tt.name, in)
		}
		// Every protocol: no port range, although AWS reports -1 / -1.
		if !out.Egress || out.Protocol != "-1" || out.FromPort != nil || out.ToPort != nil || out.CIDRIPv4 != "0.0.0.0/0" {
			t.Errorf("%s: egress rule = %+v", tt.name, out)
		}
		for _, f := range api.filters {
			if strings.HasPrefix(f, "group-id=") && f != "group-id=sg-1" {
				t.Errorf("%s: a call not scoped to the group: %s", tt.name, f)
			}
		}
	}
}
