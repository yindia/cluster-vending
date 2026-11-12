package aws_test

import (
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"k8s.io/utils/ptr"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// As the Terraform provider behind provider-upjet-aws v2.9.0 computes it
// (networkACLRuleCreateResourceID): "nacl-" + crc32("<acl>-<n>-<egress>-<protocol>-").
func TestNetworkACLRuleExternalName(t *testing.T) {
	for _, tt := range []struct {
		egress   bool
		protocol string
		want     string
	}{
		{false, "6", "nacl-4001095323"},
		{true, "-1", "nacl-1548466682"},
	} {
		if got := crossplaneaws.NetworkACLRuleExternalName("acl-0abc", 100, tt.egress, tt.protocol); got != tt.want {
			t.Errorf("egress %t protocol %s = %s, want %s", tt.egress, tt.protocol, got, tt.want)
		}
	}
}

// External names per kind (config/externalname.go, all
// IdentifierFromProvider): DefaultNetworkACL and NetworkACL the acl- ID,
// NetworkACLRule the provider's rule ID, Route "r-<table><crc32>".
func TestImportedNetworkACLs(t *testing.T) {
	objects := crossplaneaws.ImportedNetworkResources(crossplaneaws.ImportedNetwork{
		Namespace: "tenant-acme", ProviderConfigName: "aws-1", TenantID: "acme", NetworkID: "id1", Prefix: "tf", Region: "us-east-1",
		VPC: crossplaneaws.ImportedVPC{ID: "vpc-1", CIDRBlock: "10.0.0.0/16"},
		RouteTables: []crossplaneaws.ImportedRouteTable{{Key: "public", ID: "rtb-1",
			GatewayRoutes: []crossplaneaws.ImportedRoute{{GatewayID: "igw-1", Destination: "203.0.113.0/24"}}}},
		NetworkACLs: []crossplaneaws.ImportedNetworkACL{
			{Name: "tf-default-nacl", ID: "acl-default", Default: true, SubnetIDs: []string{"subnet-db"},
				Rules: []crossplaneaws.ImportedNetworkACLRule{
					{Number: 100, Protocol: "-1", Action: "allow", CIDRBlock: "0.0.0.0/0"},
					{Number: 100, Egress: true, Protocol: "-1", Action: "allow", CIDRBlock: "0.0.0.0/0"}}},
			{Name: "tf-nacl", ID: "acl-0abc", SubnetIDs: []string{"subnet-1", "subnet-2"}, Tags: map[string]string{"Name": "private"},
				Rules: []crossplaneaws.ImportedNetworkACLRule{
					{Name: "tf-nacl-in-100", Number: 100, Protocol: "6", Action: "allow", CIDRBlock: "10.0.0.0/8", FromPort: 443, ToPort: 443},
					{Name: "tf-nacl-out-100", Number: 100, Egress: true, Protocol: "-1", Action: "deny", IPv6CIDRBlock: "::/0"},
					{Name: "tf-nacl-in-110", Number: 110, Protocol: "1", Action: "allow", CIDRBlock: "10.0.0.0/8",
						ICMPType: ptr.To(-1), ICMPCode: ptr.To(-1)}}},
		},
	})
	want := []struct{ name, externalName string }{
		{"tf-vpc", "vpc-1"},
		{"tf-rt-public", "rtb-1"},
		{"tf-route-public-gw-1", crossplaneaws.RouteExternalName("rtb-1", "203.0.113.0/24")},
		{"tf-default-nacl", "acl-default"},
		{"tf-nacl", "acl-0abc"},
		{"tf-nacl-in-100", "nacl-4001095323"},
		{"tf-nacl-out-100", "nacl-1548466682"},
		{"tf-nacl-in-110", crossplaneaws.NetworkACLRuleExternalName("acl-0abc", 110, false, "1")},
	}
	if len(objects) != len(want) {
		t.Fatalf("got %d objects, want %d", len(objects), len(want))
	}
	for i, w := range want {
		if o := objects[i]; o.GetName() != w.name || o.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] != w.externalName {
			t.Errorf("object %d = %s (%s), want %s (%s)", i, o.GetName(), o.GetAnnotations()[crossplaneaws.ExternalNameAnnotation],
				w.name, w.externalName)
		}
	}
	route := objects[2].(*awsec2.Route).Spec.ForProvider
	if *route.DestinationCidrBlock != "203.0.113.0/24" || *route.GatewayID != "igw-1" {
		t.Errorf("gateway route = %+v", route)
	}
	def := objects[3].(*awsec2.DefaultNetworkACL)
	if def.Annotations[crossplaneaws.RetainAnnotation] != "true" || *def.Spec.ForProvider.DefaultNetworkACLID != "acl-default" ||
		len(def.Spec.ForProvider.Ingress) != 1 || len(def.Spec.ForProvider.Egress) != 1 ||
		*def.Spec.ForProvider.SubnetIds[0] != "subnet-db" || *def.Spec.ForProvider.Ingress[0].RuleNo != 100 {
		t.Errorf("default network ACL = %v %+v, want it retained with its rules inline", def.Annotations, def.Spec.ForProvider)
	}
	acl := objects[4].(*awsec2.NetworkACL)
	if _, retained := acl.Annotations[crossplaneaws.RetainAnnotation]; retained || len(acl.Spec.ForProvider.SubnetIds) != 2 ||
		*acl.Spec.ForProvider.VPCID != "vpc-1" || *acl.Spec.ForProvider.Tags["Name"] != "private" {
		t.Errorf("network ACL = %+v", acl.Spec.ForProvider)
	}
	in := objects[5].(*awsec2.NetworkACLRule).Spec.ForProvider
	if *in.NetworkACLID != "acl-0abc" || *in.Egress || *in.FromPort != 443 || *in.ToPort != 443 || *in.RuleAction != "allow" ||
		*in.CidrBlock != "10.0.0.0/8" || in.IPv6CidrBlock != nil {
		t.Errorf("ingress rule = %+v", in)
	}
	out := objects[6].(*awsec2.NetworkACLRule).Spec.ForProvider
	if !*out.Egress || out.FromPort != nil || *out.IPv6CidrBlock != "::/0" || out.CidrBlock != nil {
		t.Errorf("egress rule = %+v", out)
	}
	if icmp := objects[7].(*awsec2.NetworkACLRule).Spec.ForProvider; *icmp.IcmpType != -1 || *icmp.IcmpCode != -1 {
		t.Errorf("icmp rule = %+v", icmp)
	}
}
