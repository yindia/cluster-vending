package aws_test

import (
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func TestRouteExternalName(t *testing.T) {
	// As the provider names the default route of a route table it created.
	if got := crossplaneaws.RouteExternalName("rtb-0fce57845eb77cc47", "0.0.0.0/0"); got != "r-rtb-0fce57845eb77cc471080289494" {
		t.Errorf("RouteExternalName = %s", got)
	}
}

func TestImportedNetworkResources(t *testing.T) {
	objects := crossplaneaws.ImportedNetworkResources(crossplaneaws.ImportedNetwork{
		Namespace: "tenant-acme", ProviderConfigName: "aws-1", TenantID: "acme", NetworkID: "id1", Prefix: "tf-1a2b3c4d",
		Region:          "us-east-1",
		VPC:             crossplaneaws.ImportedVPC{ID: "vpc-1", CIDRBlock: "10.0.0.0/16", Tags: map[string]string{"Name": "tf"}},
		InternetGateway: &crossplaneaws.ImportedInternetGateway{ID: "igw-1"},
		Subnets: []crossplaneaws.ImportedSubnet{
			{Name: "tf-1a2b3c4d-public-use1-az1", ID: "subnet-1", CIDRBlock: "10.0.0.0/24", ZoneID: "use1-az1", Role: crossplaneaws.SubnetPublic},
		},
		NATGateways: []crossplaneaws.ImportedNAT{{Key: "use1-az1", ID: "nat-1", SubnetID: "subnet-1", AllocationID: "eipalloc-1"}},
		RouteTables: []crossplaneaws.ImportedRouteTable{{Key: "public", ID: "rtb-1",
			Associations: []crossplaneaws.ImportedAssociation{{ID: "rtbassoc-1", SubnetID: "subnet-1", SubnetName: "tf-1a2b3c4d-public-use1-az1"}},
			DefaultRoute: &crossplaneaws.ImportedRoute{GatewayID: "igw-1"}}},
	})
	want := []string{"tf-1a2b3c4d-vpc", "tf-1a2b3c4d-igw", "tf-1a2b3c4d-public-use1-az1", "tf-1a2b3c4d-eip-use1-az1",
		"tf-1a2b3c4d-nat-use1-az1", "tf-1a2b3c4d-rt-public", "tf-1a2b3c4d-route-public", "tf-1a2b3c4d-public-use1-az1-rta"}
	if len(objects) != len(want) {
		t.Fatalf("got %d objects, want %d", len(objects), len(want))
	}
	for i, name := range want {
		o := objects[i]
		if o.GetName() != name {
			t.Errorf("object %d = %s, want %s", i, o.GetName(), name)
		}
		if o.GetAnnotations()[crossplaneaws.ExternalNameAnnotation] == "" {
			t.Errorf("%s has no external name", o.GetName())
		}
	}
	vpc := objects[0].(*awsec2.VPC)
	if len(vpc.Spec.ManagementPolicies) != 1 || vpc.Spec.ManagementPolicies[0] != "Observe" ||
		*vpc.Spec.ForProvider.Tags["Name"] != "tf" || vpc.Annotations[crossplaneaws.ExternalNameAnnotation] != "vpc-1" {
		t.Errorf("vpc = %+v", vpc.Spec)
	}
	route := objects[6].(*awsec2.Route)
	if *route.Spec.ForProvider.GatewayID != "igw-1" || route.Annotations[crossplaneaws.ExternalNameAnnotation] != crossplaneaws.RouteExternalName("rtb-1", "0.0.0.0/0") {
		t.Errorf("route = %+v", route)
	}
}
