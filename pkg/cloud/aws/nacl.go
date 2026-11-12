package aws

import (
	"context"
	"sort"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// defaultDenyRule is the rule number of the "deny everything else" entry
// AWS adds to every network ACL: it cannot be changed or removed.
const defaultDenyRule = 32767

// NetworkACL is an ec2 NetworkAcl of the VPC.
type NetworkACL struct {
	ID string
	// Default: the VPC's default network ACL (it cannot be deleted).
	Default bool
	// SubnetIDs are the subnets associated with it, ordered.
	SubnetIDs []string
	// Rules are its entries without the default deny, ingress first, by
	// rule number.
	Rules []NetworkACLRule
	Tags  map[string]string
}

// NetworkACLRule is one entry of a network ACL.
type NetworkACLRule struct {
	Number int
	Egress bool
	// Protocol is the protocol number as AWS returns it: "-1" (all), "6".
	Protocol string
	// Action is allow or deny.
	Action                   string
	CIDRBlock, IPv6CIDRBlock string
	// FromPort and ToPort are 0 when the entry has no port range.
	FromPort, ToPort int
	// ICMPType and ICMPCode are set for ICMP entries.
	ICMPType, ICMPCode *int
}

// networkACLs reads the VPC's network ACLs, ordered by ID.
//
// Calls: ec2:DescribeNetworkAcls.
func (t *Tenant) networkACLs(ctx context.Context, filters []types.Filter) ([]NetworkACL, error) {
	var out []NetworkACL
	p := ec2.NewDescribeNetworkAclsPaginator(t.ec2, &ec2.DescribeNetworkAclsInput{Filters: filters})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, translate(err, "describe network ACLs")
		}
		for _, a := range page.NetworkAcls {
			out = append(out, networkACL(a))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func networkACL(a types.NetworkAcl) NetworkACL {
	n := NetworkACL{ID: awssdk.ToString(a.NetworkAclId), Default: awssdk.ToBool(a.IsDefault), Tags: tags(a.Tags)}
	for _, assoc := range a.Associations {
		if id := awssdk.ToString(assoc.SubnetId); id != "" {
			n.SubnetIDs = append(n.SubnetIDs, id)
		}
	}
	sort.Strings(n.SubnetIDs)
	for _, e := range a.Entries {
		number := int(awssdk.ToInt32(e.RuleNumber))
		if number == defaultDenyRule {
			continue
		}
		r := NetworkACLRule{Number: number, Egress: awssdk.ToBool(e.Egress), Protocol: awssdk.ToString(e.Protocol),
			Action: string(e.RuleAction), CIDRBlock: awssdk.ToString(e.CidrBlock), IPv6CIDRBlock: awssdk.ToString(e.Ipv6CidrBlock)}
		if e.PortRange != nil {
			r.FromPort, r.ToPort = int(awssdk.ToInt32(e.PortRange.From)), int(awssdk.ToInt32(e.PortRange.To))
		}
		if e.IcmpTypeCode != nil {
			r.ICMPType, r.ICMPCode = awssdk.Int(int(awssdk.ToInt32(e.IcmpTypeCode.Type))), awssdk.Int(int(awssdk.ToInt32(e.IcmpTypeCode.Code)))
		}
		n.Rules = append(n.Rules, r)
	}
	sort.SliceStable(n.Rules, func(i, j int) bool {
		if n.Rules[i].Egress != n.Rules[j].Egress {
			return !n.Rules[i].Egress
		}
		return n.Rules[i].Number < n.Rules[j].Number
	})
	return n
}
