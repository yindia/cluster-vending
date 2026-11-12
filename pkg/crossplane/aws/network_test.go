package aws_test

import (
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func plan(subnets ...crossplaneaws.SubnetPlan) crossplaneaws.NetworkPlan {
	return crossplaneaws.NetworkPlan{Namespace: "tenant-acme", ProviderConfigName: "aws-0a1b2c3d", TenantID: "acme",
		NetworkID: "0190f1a2-0000-7000-8000-000000000001", NetworkName: "prod", Prefix: "prod", Region: "us-east-1", CIDRBlock: "10.0.0.0/16", NAT: crossplaneaws.NATSingle,
		Tags: map[string]string{"Name": "overridden", "team": "platform"}, Subnets: subnets}
}

func TestNetworkResourcesNamesAndTags(t *testing.T) {
	objects, err := crossplaneaws.NetworkResources(plan(
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPublic, CIDR: "10.0.0.0/24", ZoneID: "use1-az1"},
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPrivate, CIDR: "10.0.16.0/20", ZoneID: "use1-az1"},
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPrivate, CIDR: "10.0.32.0/20", ZoneID: "use1-az1"},
	))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]any{}
	for _, o := range objects {
		if _, dup := byName[o.GetName()]; dup {
			t.Errorf("duplicate name %s", o.GetName())
		}
		byName[o.GetName()] = o
	}
	for _, name := range []string{"prod-vpc", "prod-igw", "prod-public-use1-az1", "prod-private-use1-az1",
		"prod-private-use1-az1-2", "prod-eip-use1-az1", "prod-nat-use1-az1", "prod-rt-public", "prod-route-public",
		"prod-rt-private-use1-az1", "prod-route-private-use1-az1", "prod-private-use1-az1-2-rta"} {
		if byName[name] == nil {
			t.Errorf("missing %s", name)
		}
	}
	vpc := byName["prod-vpc"].(*awsec2.VPC)
	if *vpc.Spec.ForProvider.Tags["Name"] != "prod-vpc" || *vpc.Spec.ForProvider.Tags["team"] != "platform" {
		t.Errorf("platform tags must win over the network's: %v", vpc.Spec.ForProvider.Tags)
	}
	nat := byName["prod-nat-use1-az1"].(*awsec2.NATGateway)
	if nat.Spec.ForProvider.SubnetIDRef.Name != "prod-public-use1-az1" || nat.Spec.ForProvider.AllocationIDRef.Name != "prod-eip-use1-az1" {
		t.Errorf("nat refs = %+v", nat.Spec.ForProvider)
	}
	if pub := byName["prod-public-use1-az1"].(*awsec2.Subnet); !*pub.Spec.ForProvider.MapPublicIPOnLaunch {
		t.Error("public subnet must map public IPs")
	}
}

func TestNetworkResourcesNeedPublicSubnetForNAT(t *testing.T) {
	p := plan(
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPublic, CIDR: "10.0.0.0/24", ZoneID: "use1-az1"},
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPrivate, CIDR: "10.0.16.0/20", ZoneID: "use1-az1"},
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPrivate, CIDR: "10.0.32.0/20", ZoneID: "use1-az2"},
	)
	p.NAT = crossplaneaws.NATPerZone
	if _, err := crossplaneaws.NetworkResources(p); err == nil {
		t.Fatal("per-zone NAT without a public subnet in use1-az2 accepted")
	}
	if got, want := len(crossplaneaws.NetworkLists()), len(crossplaneaws.NetworkObjects()); got != want {
		t.Errorf("NetworkLists %d != NetworkObjects %d", got, want)
	}
}

func TestNetworkPrefix(t *testing.T) {
	a := crossplaneaws.NetworkPrefix("acme", "0190f1a2-0000-7000-8000-000000000001", "shared")
	b := crossplaneaws.NetworkPrefix("acme", "0190f1a2-0000-7000-8000-000000000002", "shared")
	if a == b || len(a) != len("shared-")+8 || a[:7] != "shared-" {
		t.Errorf("prefixes %q, %q: want shared-<hash8>, distinct", a, b)
	}
}

// The set is the rendered objects in their order; only the NAT gateways
// wait (for the internet gateway).
func TestNetworkSetHoldsNATGatewaysForTheInternetGateway(t *testing.T) {
	plan := plan(
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPublic, CIDR: "10.0.0.0/24", ZoneID: "use1-az1"},
		crossplaneaws.SubnetPlan{Role: crossplaneaws.SubnetPrivate, CIDR: "10.0.16.0/20", ZoneID: "use1-az1"})
	objects, err := crossplaneaws.NetworkResources(plan)
	if err != nil {
		t.Fatal(err)
	}
	set, err := crossplaneaws.NetworkSet(plan)
	if err != nil || len(set) != len(objects) {
		t.Fatalf("set of %d items for %d objects (%v)", len(set), len(objects), err)
	}
	nats := 0
	for i, item := range set {
		if item.Object.GetName() != objects[i].GetName() {
			t.Errorf("item %d is %s, want %s", i, item.Object.GetName(), objects[i].GetName())
		}
		_, isNAT := item.Object.(*awsec2.NATGateway)
		if isNAT {
			nats++
		}
		if (len(item.After) == 1) != isNAT {
			t.Errorf("%T %s waits for %d conditions", item.Object, item.Object.GetName(), len(item.After))
		}
	}
	if nats == 0 {
		t.Error("the plan rendered no NAT gateway")
	}
}
