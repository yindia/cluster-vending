package network

import (
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestFlowLogs(t *testing.T) {
	if ResolveFlowLogs(nil) != nil || FlowLogPlan(&cloudv1.NetworkFlowLogs{}) != nil {
		t.Error("flow logs that are off are resolved or planned")
	}
	on := ResolveFlowLogs(&cloudv1.NetworkFlowLogs{Enabled: ptr.To(true)})
	if on.GetTrafficType() != cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_ALL ||
		on.GetRetentionDays() != crossplaneaws.DefaultFlowLogRetentionDays ||
		on.GetAggregationIntervalSeconds() != crossplaneaws.DefaultFlowLogAggregationSeconds {
		t.Errorf("resolved = %v, want the defaults", on)
	}
	for _, tt := range []struct {
		name string
		in   *cloudv1.NetworkFlowLogs
		want crossplaneaws.FlowLogPlan
	}{
		{"defaults", &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true)}, crossplaneaws.FlowLogPlan{TrafficType: "ALL",
			RetentionDays: crossplaneaws.DefaultFlowLogRetentionDays, AggregationSeconds: crossplaneaws.DefaultFlowLogAggregationSeconds}},
		{"as set", &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), TrafficType: cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_REJECT,
			RetentionDays: 7, AggregationIntervalSeconds: 60}, crossplaneaws.FlowLogPlan{TrafficType: "REJECT", RetentionDays: 7, AggregationSeconds: 60}},
	} {
		if got := FlowLogPlan(tt.in); got == nil || *got != tt.want {
			t.Errorf("%s: plan = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestFlowLogsChangeable(t *testing.T) {
	all := &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), TrafficType: cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_ALL, AggregationIntervalSeconds: 600}
	for _, tt := range []struct {
		name          string
		before, after *cloudv1.NetworkFlowLogs
		refused       bool
	}{
		{"turned on", nil, all, false},
		{"turned off", all, &cloudv1.NetworkFlowLogs{}, false},
		{"retention", all, &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), TrafficType: all.GetTrafficType(), AggregationIntervalSeconds: 600, RetentionDays: 7}, false},
		{"traffic type", all, &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), TrafficType: cloudv1.FlowLogTrafficType_FLOW_LOG_TRAFFIC_TYPE_REJECT, AggregationIntervalSeconds: 600}, true},
		{"interval", all, &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true), TrafficType: all.GetTrafficType(), AggregationIntervalSeconds: 60}, true},
	} {
		if err := FlowLogsChangeable(tt.before, tt.after); (err != nil) != tt.refused {
			t.Errorf("%s: %v, refused want %t", tt.name, err, tt.refused)
		}
	}
}

// The render plan carries the network's identity, its account and its
// resolved layout; the stored layout is that layout with explicit subnets.
func TestRenderPlanAndLayoutProto(t *testing.T) {
	layout, err := PlanLayout("us-east-1", &cloudv1.NetworkLayout{NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE})
	if err != nil {
		t.Fatal(err)
	}
	spec := &cloudv1.NetworkSpec{Region: "us-east-1", FlowLogs: &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true)}}
	plan := RenderPlan(Identity{TenantID: "acme", ID: "id1", Name: "prod", Labels: map[string]string{"team": "net"}}, spec, layout,
		Account{Namespace: "tenant-acme", ProviderConfigName: "aws-1", AccountID: "444455556666"})
	if plan.Namespace != "tenant-acme" || plan.ProviderConfigName != "aws-1" || plan.AccountID != "444455556666" ||
		plan.TenantID != "acme" || plan.NetworkID != "id1" || plan.NetworkName != "prod" || plan.Tags["team"] != "net" ||
		plan.Prefix != crossplaneaws.NetworkPrefix("acme", "id1", "prod") || plan.Region != "us-east-1" ||
		plan.CIDRBlock != DefaultCIDRBlock || plan.NAT != crossplaneaws.NATPerZone || len(plan.Subnets) != 2*DefaultZoneCount ||
		plan.FlowLogs == nil {
		t.Errorf("plan = %+v", plan)
	}
	if _, err := crossplaneaws.NetworkSet(plan); err != nil {
		t.Errorf("the plan does not render: %v", err)
	}
	stored := LayoutProto(layout)
	if stored.GetCidrBlock() != DefaultCIDRBlock || stored.GetNatGatewayMode() != cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE ||
		len(stored.GetSubnets()) != len(layout.Subnets) {
		t.Fatalf("stored layout = %v", stored)
	}
	again, err := PlanLayout("us-east-1", stored)
	if err != nil || len(again.Subnets) != len(layout.Subnets) || again.NAT != layout.NAT {
		t.Errorf("the stored layout resolves to %+v (%v), want the same layout", again, err)
	}
}

// twoZoneVPC is a discovered VPC with a public and a private subnet in
// each of two zones, one NAT gateway per zone.
func twoZoneVPC() *awscloud.Network {
	subnet := func(id, cidr, zone string, role awscloud.SubnetRole, rt string) awscloud.Subnet {
		return awscloud.Subnet{ID: id, CIDRBlock: cidr, ZoneID: zone, Role: role, RouteTableID: rt}
	}
	local := awscloud.Route{DestinationCIDRBlock: "10.1.0.0/16", GatewayID: "local"}
	return &awscloud.Network{
		VPC:              awscloud.VPC{ID: "vpc-1", CIDRBlock: "10.1.0.0/16"},
		InternetGateways: []awscloud.InternetGateway{{ID: "igw-1"}},
		Subnets: []awscloud.Subnet{
			subnet("subnet-pa", "10.1.0.0/24", "use1-az1", awscloud.SubnetRolePublic, "rtb-pub"),
			subnet("subnet-pb", "10.1.1.0/24", "use1-az2", awscloud.SubnetRolePublic, "rtb-pub"),
			subnet("subnet-xa", "10.1.16.0/20", "use1-az1", awscloud.SubnetRolePrivate, "rtb-a"),
			subnet("subnet-xb", "10.1.32.0/20", "use1-az2", awscloud.SubnetRolePrivate, "rtb-b"),
		},
		RouteTables: []awscloud.RouteTable{
			{ID: "rtb-pub", Associations: []awscloud.RouteTableAssociation{{ID: "rtbassoc-pa", SubnetID: "subnet-pa"}, {ID: "rtbassoc-pb", SubnetID: "subnet-pb"}},
				Routes: []awscloud.Route{local, {DestinationCIDRBlock: "0.0.0.0/0", GatewayID: "igw-1"}}},
			{ID: "rtb-a", Associations: []awscloud.RouteTableAssociation{{ID: "rtbassoc-xa", SubnetID: "subnet-xa"}},
				Routes: []awscloud.Route{local, {DestinationCIDRBlock: "0.0.0.0/0", NATGatewayID: "nat-a"},
					{DestinationCIDRBlock: "172.16.0.0/12", TransitGatewayID: "tgw-1"}}},
			{ID: "rtb-b", Associations: []awscloud.RouteTableAssociation{{ID: "rtbassoc-xb", SubnetID: "subnet-xb"}},
				Routes: []awscloud.Route{local, {DestinationCIDRBlock: "0.0.0.0/0", NATGatewayID: "nat-b"}}},
		},
		NATGateways: []awscloud.NATGateway{
			{ID: "nat-a", SubnetID: "subnet-pa", AllocationIDs: []string{"eipalloc-a"}},
			{ID: "nat-b", SubnetID: "subnet-pb", AllocationIDs: []string{"eipalloc-b"}},
		},
	}
}

func TestPlanImport(t *testing.T) {
	base := crossplaneaws.ImportedNetwork{Namespace: "tenant-acme", ProviderConfigName: "aws-1", TenantID: "acme",
		NetworkID: "id1", NetworkName: "tf", Prefix: "tf-1a2b3c4d", Region: "us-east-1"}
	plan := PlanImport(twoZoneVPC(), base)
	if plan.Blocked() {
		t.Fatalf("a two-zone VPC is blocked: %v", plan.Lines)
	}
	if plan.Layout.GetCidrBlock() != "10.1.0.0/16" || len(plan.Layout.GetSubnets()) != 4 ||
		plan.Layout.GetNatGatewayMode() != cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE {
		t.Errorf("layout = %v", plan.Layout)
	}
	if r := plan.Resources; r.VPC.ID != "vpc-1" || len(r.Subnets) != 4 || len(r.NATGateways) != 2 || len(r.RouteTables) != 3 ||
		r.Prefix != base.Prefix {
		t.Errorf("resources = %+v", r)
	}
	// The route to the transit gateway is outside this version.
	notImported := plan.NotImported()
	if len(notImported) != 1 || notImported[0].GetKind() != "Route" || !strings.Contains(notImported[0].GetDetail(), "tgw-1") {
		t.Errorf("not imported = %v, want the transit gateway route", notImported)
	}
	for _, l := range plan.Lines {
		if l.GetApiVersion() != EC2APIVersion {
			t.Errorf("%s %s has API version %s", l.GetKind(), l.GetName(), l.GetApiVersion())
		}
	}

	// One zone of private subnets cannot hold an EKS cluster: blocked.
	oneZone := twoZoneVPC()
	oneZone.Subnets = oneZone.Subnets[:3]
	if !PlanImport(oneZone, base).Blocked() {
		t.Error("a VPC with private subnets in one zone is not blocked")
	}
}
