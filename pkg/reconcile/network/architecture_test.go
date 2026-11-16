package network

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func architectureFixture(t *testing.T, kind, name string, configured, observed map[string]any) client.Object {
	t.Helper()
	var obj client.Object
	switch kind {
	case "VPC":
		obj = &awsec2.VPC{}
	case "Subnet":
		obj = &awsec2.Subnet{}
	case "NATGateway":
		obj = &awsec2.NATGateway{}
	case "InternetGateway":
		obj = &awsec2.InternetGateway{}
	case "RouteTable":
		obj = &awsec2.RouteTable{}
	case "Route":
		obj = &awsec2.Route{}
	case "RouteTableAssociation":
		obj = &awsec2.RouteTableAssociation{}
	case "EIP":
		obj = &awsec2.EIP{}
	case "VPCEndpoint":
		obj = &awsec2.VPCEndpoint{}
	case "VPCPeeringConnection":
		obj = &awsec2.VPCPeeringConnection{}
	case "TransitGateway":
		obj = &awsec2.TransitGateway{}
	default:
		t.Fatalf("unknown fixture kind %s", kind)
	}
	if kind == "Route" {
		for _, values := range []map[string]any{configured, observed} {
			if values != nil && values["destinationCidrBlock"] == nil && values["destinationIpv6CidrBlock"] == nil && values["destinationPrefixListId"] == nil {
				values["destinationCidrBlock"] = "0.0.0.0/0"
			}
		}
	}
	raw := map[string]any{"spec": map[string]any{"forProvider": configured}, "status": map[string]any{"atProvider": observed}}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, obj); err != nil {
		t.Fatal(err)
	}
	obj.SetName(name)
	obj.SetNamespace(kube.TenantNamespace("acme"))
	obj.SetLabels(crossplane.NetworkLabels("acme", "net"))
	return obj
}

func architectureRef(name string) map[string]any { return map[string]any{"name": name} }

func requireArchitectureEdge(t *testing.T, architecture *cloudv1.NetworkArchitecture, sourceKind, sourceName, targetKind, targetName, relation, evidence string) {
	t.Helper()
	if !slices.ContainsFunc(architecture.Connections, func(edge *cloudv1.NetworkArchitectureConnection) bool {
		return edge.SourceKind == sourceKind && edge.SourceName == sourceName && edge.TargetKind == targetKind && edge.TargetName == targetName && edge.Relation == relation && edge.Evidence == evidence
	}) {
		t.Fatalf("missing %s/%s -> %s/%s %s %s: %v", sourceKind, sourceName, targetKind, targetName, relation, evidence, architecture.Connections)
	}
}

func TestArchitectureAWSRoutingFacts(t *testing.T) {
	objects := []client.Object{
		architectureFixture(t, "VPC", "a", map[string]any{"cidrBlock": "10.0.0.0/16"}, map[string]any{"id": "vpc-1", "cidrBlock": "10.1.0.0/16"}),
		architectureFixture(t, "Subnet", "b", map[string]any{"vpcIdRef": architectureRef("a"), "availabilityZoneId": "use1-az1", "cidrBlock": "10.1.0.0/24"}, map[string]any{"id": "subnet-pub", "vpcId": "vpc-1", "availabilityZoneId": "use1-az2", "cidrBlock": "10.1.1.0/24"}),
		architectureFixture(t, "Subnet", "c", map[string]any{"vpcIdRef": architectureRef("a"), "availabilityZoneId": "use1-az2", "cidrBlock": "10.1.2.0/24"}, map[string]any{"id": "subnet-priv", "vpcId": "vpc-1"}),
		architectureFixture(t, "NATGateway", "d", map[string]any{"subnetIdRef": architectureRef("b"), "allocationIdRef": architectureRef("e")}, map[string]any{"id": "nat-1", "subnetId": "subnet-pub", "allocationId": "eipalloc-1", "publicIp": "203.0.113.2"}),
		architectureFixture(t, "EIP", "e", nil, map[string]any{"id": "eipalloc-1", "publicIp": "203.0.113.2"}),
		architectureFixture(t, "InternetGateway", "f", map[string]any{"vpcIdRef": architectureRef("a")}, map[string]any{"id": "igw-1", "vpcId": "vpc-1"}),
		architectureFixture(t, "RouteTable", "g", map[string]any{"vpcIdRef": architectureRef("a")}, map[string]any{"id": "rtb-private", "vpcId": "vpc-1"}),
		architectureFixture(t, "RouteTable", "h", map[string]any{"vpcIdRef": architectureRef("a")}, map[string]any{"id": "rtb-public", "vpcId": "vpc-1"}),
		architectureFixture(t, "Route", "i", map[string]any{"routeTableIdRef": architectureRef("g"), "natGatewayIdRef": architectureRef("d"), "destinationCidrBlock": "0.0.0.0/0"}, map[string]any{"id": "route-private", "routeTableId": "rtb-private", "natGatewayId": "nat-1", "destinationCidrBlock": "0.0.0.0/0"}),
		architectureFixture(t, "Route", "j", map[string]any{"routeTableIdRef": architectureRef("h"), "gatewayIdRef": architectureRef("f"), "destinationCidrBlock": "0.0.0.0/0"}, map[string]any{"id": "route-public", "routeTableId": "rtb-public", "gatewayId": "igw-1", "destinationCidrBlock": "0.0.0.0/0"}),
		architectureFixture(t, "RouteTableAssociation", "k", map[string]any{"subnetIdRef": architectureRef("c"), "routeTableIdRef": architectureRef("g")}, map[string]any{"subnetId": "subnet-priv", "routeTableId": "rtb-private"}),
		architectureFixture(t, "RouteTableAssociation", "l", map[string]any{"subnetIdRef": architectureRef("b"), "routeTableIdRef": architectureRef("h")}, map[string]any{"subnetId": "subnet-pub", "routeTableId": "rtb-public"}),
	}
	objects[1].GetLabels()[crossplane.LabelSubnetRole] = "public"
	objects[2].GetLabels()[crossplane.LabelSubnetRole] = "private"
	got := architecture(objects, "acme", "net")
	if len(got.Resources) != len(objects) {
		t.Fatalf("resources = %v", got.Resources)
	}
	for _, edge := range []struct{ sk, sn, tk, tn, relation string }{
		{"Subnet", "b", "VPC", "a", "contained_in"},
		{"Subnet", "c", "RouteTable", "g", "routes_via"},
		{"Route", "i", "RouteTable", "g", "contained_in"},
		{"Route", "i", "NATGateway", "d", "forwards_to"},
		{"NATGateway", "d", "Subnet", "b", "placed_in"},
		{"NATGateway", "d", "EIP", "e", "uses_address"},
		{"Subnet", "b", "RouteTable", "h", "routes_via"},
		{"Route", "j", "InternetGateway", "f", "forwards_to"},
	} {
		requireArchitectureEdge(t, got, edge.sk, edge.sn, edge.tk, edge.tn, edge.relation, "observed")
	}
	for _, resource := range got.Resources {
		switch resource.Name {
		case "a":
			if resource.CidrBlock != "10.1.0.0/16" {
				t.Fatal("observed CIDR must take precedence")
			}
		case "b":
			if resource.ZoneId != "use1-az2" || resource.CidrBlock != "10.1.1.0/24" || resource.SubnetRole != cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC {
				t.Fatalf("subnet facts: %v", resource)
			}
		case "c":
			if resource.SubnetRole != cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE {
				t.Fatal("private role missing")
			}
		case "e":
			if resource.PublicIp != "203.0.113.2" {
				t.Fatal("egress IP missing")
			}
		}
	}
	for _, edge := range got.Connections {
		if edge.Relation == "forwards_to" && edge.Label != "0.0.0.0/0" {
			t.Fatalf("route destination missing: %v", edge)
		}
	}
	slices.Reverse(objects)
	if reversed := architecture(objects, "acme", "net"); !proto.Equal(got, reversed) {
		t.Fatal("topology output depends on observation order")
	}
}

func TestArchitectureConfiguredAndObservedDrift(t *testing.T) {
	objects := []client.Object{
		architectureFixture(t, "NATGateway", "desired", nil, map[string]any{"id": "nat-desired"}),
		architectureFixture(t, "NATGateway", "actual", nil, map[string]any{"id": "nat-actual"}),
		architectureFixture(t, "Route", "route", map[string]any{"natGatewayIdRef": architectureRef("desired"), "destinationCidrBlock": "0.0.0.0/0"}, map[string]any{"natGatewayId": "nat-actual", "destinationCidrBlock": "0.0.0.0/0"}),
		architectureFixture(t, "Subnet", "subnet", nil, map[string]any{"id": "subnet-1"}),
		architectureFixture(t, "RouteTable", "table", nil, map[string]any{"id": "rtb-1"}),
		// Mixed evidence must never fabricate a subnet -> table observed path.
		architectureFixture(t, "RouteTableAssociation", "incomplete", map[string]any{"subnetIdRef": architectureRef("subnet")}, map[string]any{"routeTableId": "rtb-1"}),
	}
	got := architecture(objects, "acme", "net")
	requireArchitectureEdge(t, got, "Route", "route", "NATGateway", "desired", "forwards_to", "configured")
	requireArchitectureEdge(t, got, "Route", "route", "NATGateway", "actual", "forwards_to", "observed")
	for _, edge := range got.Connections {
		if edge.Relation == "routes_via" {
			t.Fatalf("mixed evidence fabricated association: %v", edge)
		}
	}
}

func TestArchitectureRejectsForeignDanglingAndAmbiguousReferences(t *testing.T) {
	for _, tt := range []struct {
		name   string
		ref    map[string]any
		change func(client.Object)
	}{
		{"dangling", architectureRef("unknown"), func(client.Object) {}},
		{"cross namespace", map[string]any{"name": "target", "namespace": "tenant-other"}, func(client.Object) {}},
		{"other network", architectureRef("target"), func(o client.Object) { o.GetLabels()[crossplane.LabelNetwork] = "other" }},
		{"other tenant namespace", architectureRef("target"), func(o client.Object) { o.SetNamespace("tenant-other") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := architectureFixture(t, "NATGateway", "target", nil, map[string]any{"id": "nat-target"})
			tt.change(target)
			// A populated resolved ID must not override a foreign/dangling named ref.
			route := architectureFixture(t, "Route", "route", map[string]any{"natGatewayIdRef": tt.ref, "natGatewayId": "nat-target"}, nil)
			got := architecture([]client.Object{route, target}, "acme", "net")
			if len(got.Connections) != 0 {
				t.Fatalf("foreign reference resolved: %v", got.Connections)
			}
		})
	}
	a := architectureFixture(t, "NATGateway", "a", nil, map[string]any{"id": "duplicate"})
	b := architectureFixture(t, "NATGateway", "b", nil, map[string]any{"id": "duplicate"})
	route := architectureFixture(t, "Route", "route", nil, map[string]any{"natGatewayId": "duplicate"})
	if got := architecture([]client.Object{a, b, route}, "acme", "net"); len(got.Connections) != 0 {
		t.Fatal("ambiguous cloud ID resolved")
	}
	// A Route named like a target cannot satisfy a NATGateway reference.
	wrongKind := architectureFixture(t, "Route", "target", nil, map[string]any{"id": "nat-other"})
	route = architectureFixture(t, "Route", "source", map[string]any{"natGatewayIdRef": architectureRef("target")}, nil)
	if got := architecture([]client.Object{wrongKind, route}, "acme", "net"); len(got.Connections) != 0 {
		t.Fatal("wrong kind satisfied reference")
	}
}

func TestArchitectureStaleExternalNameDoesNotOverrideObservedIdentity(t *testing.T) {
	target := architectureFixture(t, "NATGateway", "target", nil, map[string]any{"id": "nat-current"})
	target.SetAnnotations(map[string]string{"crossplane.io/external-name": "nat-old"})
	route := architectureFixture(t, "Route", "route", nil, map[string]any{"natGatewayId": "nat-old"})
	if got := architecture([]client.Object{target, route}, "acme", "net"); len(got.Connections) != 0 {
		t.Fatalf("stale annotation fabricated observed link: %v", got.Connections)
	}
}

func TestArchitectureAllowlistAndExternalIDs(t *testing.T) {
	vpc := architectureFixture(t, "VPC", "vpc", map[string]any{"cidrBlock": "10.0.0.0/16", "tags": map[string]any{"private-value": "DO-NOT-EXPOSE"}}, nil)
	vpc.SetAnnotations(map[string]string{"crossplane.io/external-name": "vpc-imported", "secret": "DO-NOT-EXPOSE"})
	subnet := architectureFixture(t, "Subnet", "subnet", map[string]any{"vpcId": "vpc-imported", "cidrBlock": "10.0.1.0/24"}, nil)
	unknown := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "unsupported", Namespace: kube.TenantNamespace("acme"), Labels: crossplane.NetworkLabels("acme", "net")}}
	got := architecture([]client.Object{vpc, subnet, unknown}, "acme", "net")
	requireArchitectureEdge(t, got, "Subnet", "subnet", "VPC", "vpc", "contained_in", "configured")
	raw, err := protojson.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"DO-NOT-EXPOSE", "tags", "annotations", "providerConfig", "unsupported"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("nonallowlisted data leaked: %s", raw)
		}
	}
	if len(got.Resources) != 2 {
		t.Fatal("unsupported kind included")
	}
}

func TestViewIncludesNetworkArchitecture(t *testing.T) {
	store := storetest.NewStore(t)
	storetest.MustTenant(t, store, "acme")
	network := storetest.MustNetwork(t, store, "acme", "shared")
	obj := architectureFixture(t, "VPC", "live-vpc", map[string]any{"cidrBlock": "10.0.0.0/16"}, map[string]any{"id": "vpc-1"})
	obj.GetLabels()[crossplane.LabelNetwork] = network.ID
	k := storetest.NewClient(t)
	if err := k.Create(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	s := New(reconcile.Deps{Store: store, Kube: k})
	row, err := store.Networks().Get(t.Context(), "acme", network.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedStatus(row).GetArchitecture() != nil {
		t.Fatal("fixture unexpectedly has persisted architecture")
	}
	before := slices.Clone(row.Status)
	view, err := s.viewOf(t.Context(), row, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resources := view.GetStatus().GetArchitecture().GetResources(); len(resources) != 1 || resources[0].CloudId != "vpc-1" {
		t.Fatalf("architecture not wired into view: %v", resources)
	}
	persisted, err := store.Networks().Get(t.Context(), "acme", network.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, persisted.Status) {
		t.Fatal("read-only live view persisted status")
	}

}

func TestArchitectureInlineLocalAndExternalRoutes(t *testing.T) {
	vpc := architectureFixture(t, "VPC", "vpc", nil, map[string]any{"id": "vpc-1", "cidrBlock": "10.0.0.0/16"})
	table := architectureFixture(t, "RouteTable", "table", map[string]any{"vpcIdRef": architectureRef("vpc")}, map[string]any{
		"id": "rtb-1", "vpcId": "vpc-1", "route": []any{
			map[string]any{"cidrBlock": "10.0.0.0/16", "gatewayId": "local"},
			map[string]any{"ipv6CidrBlock": "2001:db8::/56", "gatewayId": "local"},
			map[string]any{"destinationPrefixListId": "pl-0123456789abcdef0", "vpcEndpointId": "vpce-0123456789abcdef0"},
			map[string]any{"cidrBlock": "172.16.0.0/16", "vpcPeeringConnectionId": "pcx-0123456789abcdef0"},
			map[string]any{"cidrBlock": "192.168.0.0/16", "transitGatewayId": "tgw-0123456789abcdef0"},
			// Gateway endpoints can also appear in gatewayId observations.
			map[string]any{"destinationPrefixListId": "pl-abcdef01234567890", "gatewayId": "vpce-0123456789abcdef0"},
			// A familiar VPC CIDR alone is not evidence of local routing.
			map[string]any{"cidrBlock": "10.0.0.0/16"},
		},
	})
	got := architecture([]client.Object{table, vpc}, "acme", "net")
	requireArchitectureEdge(t, got, "RouteTable", "table", "VPC", "vpc", "forwards_to", "observed")
	for _, target := range []struct{ kind, id string }{
		{"VPCEndpoint", "vpce-0123456789abcdef0"}, {"VPCPeeringConnection", "pcx-0123456789abcdef0"}, {"TransitGateway", "tgw-0123456789abcdef0"},
	} {
		requireArchitectureEdge(t, got, "RouteTable", "table", target.kind, target.id, "forwards_to", "observed")
	}
	if len(got.Resources) != 5 {
		t.Fatalf("inline routes fabricated managed resources or duplicate references: %v", got.Resources)
	}
	for _, r := range got.Resources {
		if r.Kind == "Route" {
			t.Fatal("invented a Route managed resource")
		}
		if r.Kind == "VPC" || r.Kind == "RouteTable" {
			if r.ReferenceOnly {
				t.Fatal("owned resource marked reference-only")
			}
			continue
		}
		if !r.ReferenceOnly || r.Name != r.CloudId || r.CidrBlock != "" || r.ZoneId != "" || r.SubnetRole != cloudv1.SubnetRole_SUBNET_ROLE_UNSPECIFIED {
			t.Fatalf("unowned resource presented as discovered/managed: %v", r)
		}
	}
	localCount := 0
	for _, edge := range got.Connections {
		if edge.Relation == "forwards_to" && edge.TargetKind == "VPC" {
			localCount++
			if edge.DestinationCidrBlock != "10.0.0.0/16" && edge.DestinationIpv6CidrBlock != "2001:db8::/56" {
				t.Fatalf("local destination missing: %v", edge)
			}
		}
		if edge.TargetKind == "VPCEndpoint" && edge.DestinationPrefixListId == "" {
			t.Fatalf("prefix-list destination missing: %v", edge)
		}
		if edge.RouteState != "" || edge.RouteOrigin != "" {
			t.Fatal("invented inline route state/origin not exposed by provider")
		}
	}
	if localCount != 2 {
		t.Fatalf("local edges=%d, want only the two explicit local entries", localCount)
	}
	reversed := architecture([]client.Object{vpc, table}, "acme", "net")
	if !proto.Equal(got, reversed) {
		t.Fatal("reference nodes depend on observation order")
	}
}

func TestArchitectureStandaloneRouteTargetsAndBlackhole(t *testing.T) {
	for _, tt := range []struct{ field, kind, id string }{
		{"vpcEndpointId", "VPCEndpoint", "vpce-0123456789abcdef0"},
		{"vpcPeeringConnectionId", "VPCPeeringConnection", "pcx-0123456789abcdef0"},
		{"transitGatewayId", "TransitGateway", "tgw-0123456789abcdef0"},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			route := architectureFixture(t, "Route", "route", map[string]any{tt.field: tt.id, "destinationCidrBlock": "172.16.0.0/16"}, map[string]any{tt.field: tt.id, "destinationCidrBlock": "172.16.0.0/16", "state": "blackhole", "origin": "CreateRoute"})
			got := architecture([]client.Object{route}, "acme", "net")
			requireArchitectureEdge(t, got, "Route", "route", tt.kind, tt.id, "forwards_to", "observed")
			if len(got.Connections) != 1 || len(got.Resources) != 2 {
				t.Fatalf("matching evidence not coalesced: %v", got)
			}
			edge := got.Connections[0]
			if edge.DestinationCidrBlock != "172.16.0.0/16" || edge.RouteState != "blackhole" || edge.RouteOrigin != "CreateRoute" {
				t.Fatalf("route facts lost: %v", edge)
			}
			// Distinct configured and observed targets must remain distinct evidence.
			route.(*awsec2.Route).Status.AtProvider.TransitGatewayID = nil
			if tt.field == "transitGatewayId" {
				other := "tgw-abcdef01234567890"
				route.(*awsec2.Route).Status.AtProvider.TransitGatewayID = &other
				drift := architecture([]client.Object{route}, "acme", "net")
				requireArchitectureEdge(t, drift, "Route", "route", tt.kind, tt.id, "forwards_to", "configured")
				requireArchitectureEdge(t, drift, "Route", "route", tt.kind, other, "forwards_to", "observed")
				for _, e := range drift.Connections {
					if e.Evidence == "configured" && (e.RouteState != "" || e.RouteOrigin != "") {
						t.Fatal("observed failure status leaked onto configured intent")
					}
				}
			}
		})
	}
}

func TestArchitectureStandaloneLocalRouteUsesExplicitSameEvidenceVPC(t *testing.T) {
	vpc := architectureFixture(t, "VPC", "vpc", nil, map[string]any{"id": "vpc-1"})
	table := architectureFixture(t, "RouteTable", "table", map[string]any{"vpcIdRef": architectureRef("vpc")}, map[string]any{"id": "rtb-1", "vpcId": "vpc-1"})
	route := architectureFixture(t, "Route", "local", map[string]any{"routeTableIdRef": architectureRef("table"), "gatewayId": "local", "destinationCidrBlock": "10.0.0.0/16"}, map[string]any{"routeTableId": "rtb-1", "gatewayId": "local", "destinationCidrBlock": "10.0.0.0/16", "state": "active"})
	got := architecture([]client.Object{vpc, table, route}, "acme", "net")
	requireArchitectureEdge(t, got, "Route", "local", "VPC", "vpc", "forwards_to", "observed")
	table.(*awsec2.RouteTable).Status.AtProvider.VPCID = nil
	mixed := architecture([]client.Object{vpc, table, route}, "acme", "net")
	for _, e := range mixed.Connections {
		if e.Relation == "forwards_to" && e.TargetKind == "VPC" && e.Evidence == "observed" {
			t.Fatal("configured VPC ref fabricated an observed local path")
		}
	}
	requireArchitectureEdge(t, mixed, "Route", "local", "VPC", "vpc", "forwards_to", "configured")
}

func TestArchitectureExternalReferencesFailClosed(t *testing.T) {
	for _, tt := range []struct {
		name   string
		values map[string]any
	}{
		{"foreign named ref", map[string]any{"transitGatewayIdRef": map[string]any{"name": "foreign", "namespace": "tenant-other"}, "transitGatewayId": "tgw-0123456789abcdef0"}},
		{"dangling named ref", map[string]any{"transitGatewayIdRef": architectureRef("missing"), "transitGatewayId": "tgw-0123456789abcdef0"}},
		{"invalid namespaced ref", map[string]any{"transitGatewayIdRef": map[string]any{"namespace": "tenant-other"}, "transitGatewayId": "tgw-0123456789abcdef0"}},
		{"selector only", map[string]any{"transitGatewayIdSelector": map[string]any{"matchLabels": map[string]any{"secret": "DO-NOT-EXPOSE"}}}},
		{"arbitrary secret string", map[string]any{"vpcEndpointId": "DO-NOT-EXPOSE"}},
		{"wrong target ID kind", map[string]any{"transitGatewayId": "vpce-0123456789abcdef0"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			route := architectureFixture(t, "Route", "route", tt.values, nil)
			got := architecture([]client.Object{route}, "acme", "net")
			if len(got.Connections) != 0 || len(got.Resources) != 1 {
				t.Fatalf("fabricated external reference: %v", got)
			}
			raw, err := protojson.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "DO-NOT-EXPOSE") || strings.Contains(string(raw), "tenant-other") {
				t.Fatalf("reference metadata leaked: %s", raw)
			}
		})
	}
	// A gatewayIdRef is typed to InternetGateway, not VPCEndpoint.
	endpoint := architectureFixture(t, "VPCEndpoint", "same-name", nil, map[string]any{"id": "vpce-0123456789abcdef0"})
	route := architectureFixture(t, "Route", "route", map[string]any{"gatewayIdRef": architectureRef("same-name"), "gatewayId": "vpce-0123456789abcdef0"}, nil)
	if got := architecture([]client.Object{route, endpoint}, "acme", "net"); len(got.Connections) != 0 {
		t.Fatalf("typed reference reinterpreted by name: %v", got)
	}
}

func TestArchitectureExternalReferenceCannotAliasOwnedName(t *testing.T) {
	id := "vpce-0123456789abcdef0"
	owned := architectureFixture(t, "VPCEndpoint", id, nil, map[string]any{"id": "vpce-abcdef01234567890"})
	route := architectureFixture(t, "Route", "route", nil, map[string]any{"vpcEndpointId": id})
	got := architecture([]client.Object{owned, route}, "acme", "net")
	if len(got.Resources) != 2 || len(got.Connections) != 0 {
		t.Fatalf("external reference aliased a different owned cloud identity: %v", got)
	}
}
