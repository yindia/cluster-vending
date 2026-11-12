package network

import (
	"slices"
	"testing"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func planLines(n *cloudv1.Network) map[string]*cloudv1.ImportedResource {
	lines := map[string]*cloudv1.ImportedResource{}
	for _, l := range n.GetStatus().GetImport().GetResources() {
		lines[l.GetKind()+"/"+l.GetExternalName()] = l
	}
	return lines
}

func TestImportNetworkACLs(t *testing.T) {
	d := existingVPC()
	allow := awscloud.NetworkACLRule{Number: 100, Protocol: "-1", Action: "allow", CIDRBlock: "0.0.0.0/0"}
	allowOut := allow
	allowOut.Egress = true
	d.NetworkACLs = []awscloud.NetworkACL{
		{ID: "acl-default", Default: true, SubnetIDs: []string{"subnet-pa", "subnet-pb"}, Rules: []awscloud.NetworkACLRule{allow, allowOut}},
		{ID: "acl-private", SubnetIDs: []string{"subnet-db", "subnet-xa", "subnet-xb"}, Rules: []awscloud.NetworkACLRule{allow, allowOut}},
		{ID: "acl-db", SubnetIDs: []string{"subnet-db2"}, Rules: []awscloud.NetworkACLRule{allow}},
	}
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: d}
	f.svc.deps.AWS.StaticKeys = testKeys
	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatalf("ImportNetwork: %v", err)
	}
	n := res.GetNetwork()
	lines := planLines(n)
	carried, notImported := cloudv1.Disposition_DISPOSITION_CARRIED_OVER, cloudv1.Disposition_DISPOSITION_NOT_IMPORTED
	for line, want := range map[string]cloudv1.Disposition{
		"DefaultNetworkACL/acl-default": carried, "NetworkACL/acl-private": carried, "NetworkACL/acl-db": notImported,
		"NetworkACLRule/" + crossplaneaws.NetworkACLRuleExternalName("acl-private", 100, false, "-1"): carried,
		"NetworkACLRule/" + crossplaneaws.NetworkACLRuleExternalName("acl-private", 100, true, "-1"):  carried,
	} {
		if got := lines[line].GetDisposition(); got != want {
			t.Errorf("%s = %s, want %s", line, got, want)
		}
	}
	if n.GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_IMPORTED {
		t.Fatalf("state = %s (%s): network ACLs must not block", n.GetStatus().GetState(), n.GetStatus().GetMessage())
	}
	// 20 as without network ACLs + the default ACL, the custom one and its 2 rules.
	if got := len(n.GetStatus().GetManagedResources()); got != 24 {
		t.Errorf("managed resources = %d, want 24", got)
	}
	acl := &awsec2.NetworkACL{}
	if err := f.kube.Get(t.Context(), key(n, "nacl"), acl); err != nil {
		t.Fatal(err)
	}
	// Every association is kept, also of subnets that are not imported.
	if len(acl.Spec.ForProvider.SubnetIds) != 3 || acl.Spec.ManagementPolicies[0] != "Observe" {
		t.Errorf("network ACL = %+v %v", acl.Spec.ForProvider, acl.Spec.ManagementPolicies)
	}
	if err := f.kube.Get(t.Context(), key(n, "nacl-out-100"), &awsec2.NetworkACLRule{}); err != nil {
		t.Errorf("egress rule: %v", err)
	}

	// The takeover owns the default network ACL without Delete.
	if _, err := f.svc.TakeOverNetwork(t.Context(), &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: n.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatalf("TakeOverNetwork: %v", err)
	}
	def := &awsec2.DefaultNetworkACL{}
	if err := f.kube.Get(t.Context(), key(n, "default-nacl"), def); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(def.Spec.ManagementPolicies, "Delete") || slices.Contains(def.Spec.ManagementPolicies, "*") ||
		len(def.Spec.ForProvider.Ingress) != 1 || len(def.Spec.ForProvider.SubnetIds) != 2 {
		t.Errorf("default network ACL after takeover = %v %+v", def.Spec.ManagementPolicies, def.Spec.ForProvider)
	}
	if err := f.kube.Get(t.Context(), key(n, "nacl"), acl); err != nil || acl.Spec.ManagementPolicies[0] != "*" {
		t.Errorf("custom network ACL after takeover = %v, %v", acl.Spec.ManagementPolicies, err)
	}

	// Deleting: rules before their ACL, everything before the VPC.
	ctx := t.Context()
	if _, err := f.svc.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()}); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if f.count(t, &awsec2.NetworkACLRuleList{}, n.GetId()) != 0 || f.count(t, &awsec2.NetworkACLList{}, n.GetId()) != 1 {
		t.Errorf("after delete: %d rules, %d network ACLs; want the rules gone first",
			f.count(t, &awsec2.NetworkACLRuleList{}, n.GetId()), f.count(t, &awsec2.NetworkACLList{}, n.GetId()))
	}
	for i := 0; ; i++ {
		if _, err := f.svc.GetNetworkStatus(ctx, &cloudv1.GetNetworkStatusRequest{TenantId: tenant, NetworkId: n.GetId()}); connect.CodeOf(err) == connect.CodeNotFound {
			break
		} else if err != nil || i > 10 {
			t.Fatalf("get %d: %v", i, err)
		}
		if err := f.svc.Reconcile(ctx, tenant, n.GetId()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if f.count(t, &awsec2.VPCList{}, n.GetId()) == 0 &&
			f.count(t, &awsec2.NetworkACLList{}, n.GetId())+f.count(t, &awsec2.DefaultNetworkACLList{}, n.GetId()) > 0 {
			t.Fatal("VPC deleted before its network ACLs")
		}
	}
}

// A public route table that sends only specific CIDRs to the internet
// gateway is imported as it is.
func TestImportPublicRouteTableWithSpecificRoutes(t *testing.T) {
	d := existingVPC()
	for i := range d.RouteTables {
		if d.RouteTables[i].ID == "rtb-pub" {
			d.RouteTables[i].Routes = []awscloud.Route{
				{DestinationCIDRBlock: "10.1.0.0/16", GatewayID: "local"},
				{DestinationCIDRBlock: "203.0.113.0/24", GatewayID: "igw-1"},
				{DestinationCIDRBlock: "198.51.100.0/24", GatewayID: "igw-1"},
			}
		}
	}
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: d}
	f.svc.deps.AWS.StaticKeys = testKeys
	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatalf("ImportNetwork: %v", err)
	}
	n := res.GetNetwork()
	if n.GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_IMPORTED ||
		dispositions(n.GetStatus().GetImport())[cloudv1.Disposition_DISPOSITION_BLOCKING] != 0 {
		t.Fatalf("state = %s (%s), want IMPORTED without BLOCKING lines", n.GetStatus().GetState(), n.GetStatus().GetMessage())
	}
	lines := planLines(n)
	for _, cidr := range []string{"203.0.113.0/24", "198.51.100.0/24"} {
		if l := lines["Route/"+crossplaneaws.RouteExternalName("rtb-pub", cidr)]; l.GetDisposition() != cloudv1.Disposition_DISPOSITION_CARRIED_OVER {
			t.Errorf("route %s = %v, want CARRIED_OVER", cidr, l)
		}
	}
	if lines["Route/"+crossplaneaws.RouteExternalName("rtb-pub", "0.0.0.0/0")] != nil {
		t.Error("a default route was planned for the public table; it has none")
	}
	route := &awsec2.Route{}
	if err := f.kube.Get(t.Context(), key(n, "route-public-gw-2"), route); err != nil {
		t.Fatal(err)
	}
	if *route.Spec.ForProvider.DestinationCidrBlock != "198.51.100.0/24" || *route.Spec.ForProvider.GatewayID != "igw-1" {
		t.Errorf("route = %+v", route.Spec.ForProvider)
	}
	if err := f.kube.Get(t.Context(), key(n, "route-public"), route); err == nil {
		t.Error("a 0.0.0.0/0 route object was created for the public table")
	}
}

// New networks: the NAT gateways are applied only once the internet
// gateway is Ready; the loop's Reconcile applies them then.
func TestNATGatewayWaitsForInternetGateway(t *testing.T) {
	f := newFixture(t)
	res, err := f.svc.CreateNetwork(t.Context(), f.request("prod", nil))
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	id := res.GetNetwork().GetId()
	row, err := f.store.Networks().Get(t.Context(), tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.count(t, &awsec2.NATGatewayList{}, id) != 0 || f.count(t, &awsec2.EIPList{}, id) != 1 ||
		f.count(t, &awsec2.InternetGatewayList{}, id) != 1 || row.AppliedGeneration >= row.Generation {
		t.Fatalf("after create: %d NAT gateways, applied generation %d of %d; want none yet and the network pending",
			f.count(t, &awsec2.NATGatewayList{}, id), row.AppliedGeneration, row.Generation)
	}
	// The gateway is not Ready: another pass still holds them back.
	if err := f.svc.Reconcile(t.Context(), tenant, id); err != nil {
		t.Fatal(err)
	}
	if f.count(t, &awsec2.NATGatewayList{}, id) != 0 {
		t.Fatal("NAT gateway applied before the internet gateway is Ready")
	}
	n := f.attachGateway(t, id)
	row, err = f.store.Networks().Get(t.Context(), tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.count(t, &awsec2.NATGatewayList{}, id) != 1 || row.AppliedGeneration != row.Generation ||
		len(n.GetStatus().GetManagedResources()) != 20 {
		t.Errorf("after the gateway is Ready: %d NAT gateways, applied generation %d of %d, %d resources",
			f.count(t, &awsec2.NATGatewayList{}, id), row.AppliedGeneration, row.Generation, len(n.GetStatus().GetManagedResources()))
	}
	// Once created they are always applied, whatever the gateway reports.
	gw := &awsec2.InternetGateway{}
	if err := f.kube.Get(t.Context(), key(n, "igw"), gw); err != nil {
		t.Fatal(err)
	}
	gw.Status.Conditions = nil
	if err := f.kube.Status().Update(t.Context(), gw); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateNetwork(t.Context(), &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network: &cloudv1.Network{Id: id, Labels: map[string]string{"team": "net"}}, UpdateMask: mask("labels")}); err != nil {
		t.Fatal(err)
	}
	nat := &awsec2.NATGateway{}
	if err := f.kube.Get(t.Context(), key(n, "nat-use1-az1"), nat); err != nil || *nat.Spec.ForProvider.Tags["team"] != "net" {
		t.Errorf("NAT gateway after a label update: %v, %v", nat.Spec.ForProvider.Tags, err)
	}
}
