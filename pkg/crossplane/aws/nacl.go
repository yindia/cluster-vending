package aws

import (
	"fmt"
	"hash/crc32"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ImportedNetworkACL is a network ACL of the VPC; Name is its managed
// resource name. The default one becomes a DefaultNetworkACL with its
// rules inline (that kind owns them) and is never deleted; a custom one a
// NetworkACL plus a NetworkACLRule per rule. The provider has no
// association kind: subnets are associated through subnetIds.
type ImportedNetworkACL struct {
	Name, ID  string
	Default   bool
	SubnetIDs []string
	Rules     []ImportedNetworkACLRule
	Tags      map[string]string
}

// ImportedNetworkACLRule is one entry of a network ACL. Name is its
// managed resource name (custom ACLs only). Protocol is the protocol
// number ("-1" is all).
type ImportedNetworkACLRule struct {
	Name                     string
	Number                   int
	Egress                   bool
	Protocol, Action         string
	CIDRBlock, IPv6CIDRBlock string
	FromPort, ToPort         int
	ICMPType, ICMPCode       *int
}

// NetworkACLRuleExternalName is the provider's ID of a network ACL rule:
// "nacl-<crc32 of '<acl ID>-<rule number>-<egress>-<protocol>-'>".
func NetworkACLRuleExternalName(aclID string, number int, egress bool, protocol string) string {
	return fmt.Sprintf("nacl-%d", crc32.ChecksumIEEE(fmt.Appendf(nil, "%s-%d-%t-%s-", aclID, number, egress, protocol)))
}

// networkACLs renders the imported network ACLs: each ACL, then the rules
// of the custom ones.
func (b importBuilder) networkACLs() []client.Object {
	var out []client.Object
	for _, a := range b.n.NetworkACLs {
		if a.Default {
			out = append(out, b.defaultNetworkACL(a))
		} else {
			out = append(out, b.networkACL(a))
		}
	}
	for _, a := range b.n.NetworkACLs {
		if a.Default {
			continue
		}
		for _, r := range a.Rules {
			out = append(out, b.networkACLRule(a, r))
		}
	}
	return out
}

func (b importBuilder) defaultNetworkACL(a ImportedNetworkACL) *awsec2.DefaultNetworkACL {
	meta := b.meta(a.Name, a.ID, nil)
	retain(&meta, true) // a VPC's default network ACL cannot be deleted
	params := awsec2.DefaultNetworkACLParameters{
		Region: ptr.To(b.n.Region), DefaultNetworkACLID: ptr.To(a.ID), SubnetIds: ptrs(a.SubnetIDs), Tags: tagPtrs(a.Tags),
	}
	for _, r := range a.Rules {
		if r.Egress {
			params.Egress = append(params.Egress, awsec2.EgressParameters{
				Action: ptr.To(r.Action), Protocol: ptr.To(r.Protocol), RuleNo: ptr.To(float64(r.Number)),
				FromPort: ptr.To(float64(r.FromPort)), ToPort: ptr.To(float64(r.ToPort)),
				CidrBlock: optional(r.CIDRBlock), IPv6CidrBlock: optional(r.IPv6CIDRBlock),
				IcmpType: floatPtr(r.ICMPType), IcmpCode: floatPtr(r.ICMPCode),
			})
			continue
		}
		params.Ingress = append(params.Ingress, awsec2.IngressParameters{
			Action: ptr.To(r.Action), Protocol: ptr.To(r.Protocol), RuleNo: ptr.To(float64(r.Number)),
			FromPort: ptr.To(float64(r.FromPort)), ToPort: ptr.To(float64(r.ToPort)),
			CidrBlock: optional(r.CIDRBlock), IPv6CidrBlock: optional(r.IPv6CIDRBlock),
			IcmpType: floatPtr(r.ICMPType), IcmpCode: floatPtr(r.ICMPCode),
		})
	}
	return &awsec2.DefaultNetworkACL{ObjectMeta: meta,
		Spec: awsec2.DefaultNetworkACLSpec{ManagedResourceSpec: b.spec(), ForProvider: params}}
}

func (b importBuilder) networkACL(a ImportedNetworkACL) *awsec2.NetworkACL {
	return &awsec2.NetworkACL{
		ObjectMeta: b.meta(a.Name, a.ID, nil),
		Spec: awsec2.NetworkACLSpec{ManagedResourceSpec: b.spec(), ForProvider: awsec2.NetworkACLParameters{
			Region: ptr.To(b.n.Region), VPCID: ptr.To(b.n.VPC.ID), SubnetIds: ptrs(a.SubnetIDs), Tags: tagPtrs(a.Tags),
		}},
	}
}

func (b importBuilder) networkACLRule(a ImportedNetworkACL, r ImportedNetworkACLRule) *awsec2.NetworkACLRule {
	params := awsec2.NetworkACLRuleParameters{
		Region: ptr.To(b.n.Region), NetworkACLID: ptr.To(a.ID), RuleNumber: ptr.To(float64(r.Number)),
		Egress: ptr.To(r.Egress), Protocol: ptr.To(r.Protocol), RuleAction: ptr.To(r.Action),
		CidrBlock: optional(r.CIDRBlock), IPv6CidrBlock: optional(r.IPv6CIDRBlock),
		IcmpType: floatPtr(r.ICMPType), IcmpCode: floatPtr(r.ICMPCode),
	}
	if r.FromPort != 0 || r.ToPort != 0 {
		params.FromPort, params.ToPort = ptr.To(float64(r.FromPort)), ptr.To(float64(r.ToPort))
	}
	return &awsec2.NetworkACLRule{
		ObjectMeta: b.meta(r.Name, NetworkACLRuleExternalName(a.ID, r.Number, r.Egress, r.Protocol), nil),
		Spec:       awsec2.NetworkACLRuleSpec{ManagedResourceSpec: b.spec(), ForProvider: params},
	}
}

// optional is nil for an empty string.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return ptr.To(s)
}

func floatPtr(i *int) *float64 {
	if i == nil {
		return nil
	}
	return ptr.To(float64(*i))
}
