package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"

	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// fakeDiscoverer returns a fixed network, or err, and counts its lookups
// when given somewhere to.
type fakeDiscoverer struct {
	network *awscloud.Network
	err     error
	lookups *int
}

func (f fakeDiscoverer) DiscoverNetwork(context.Context, awscloud.Access, string, string, []string) (*awscloud.Network, error) {
	if f.lookups != nil {
		*f.lookups++
	}
	return f.network, f.err
}

// noAWS fails the test when anything looks the cloud up: what a handler
// must never do.
type noAWS struct{ t *testing.T }

func (n noAWS) DiscoverNetwork(context.Context, awscloud.Access, string, string, []string) (*awscloud.Network, error) {
	n.t.Helper()
	n.t.Error("a handler looked the cloud up: only the reconciler may")
	return nil, errors.New("no AWS in a handler")
}

// existingVPC is a Terraform-style two-zone VPC: public subnets on one
// route table to the internet gateway, private subnets on a route table
// per zone to that zone's NAT gateway; the main table is unused.
func existingVPC() *awscloud.Network {
	subnet := func(id, cidr, zone string, role awscloud.SubnetRole, rt string) awscloud.Subnet {
		return awscloud.Subnet{ID: id, CIDRBlock: cidr, ZoneID: zone, Role: role, RouteTableID: rt,
			Tags: map[string]string{"Name": id}}
	}
	assoc := func(id, subnet string) awscloud.RouteTableAssociation {
		return awscloud.RouteTableAssociation{ID: id, SubnetID: subnet}
	}
	local := awscloud.Route{DestinationCIDRBlock: "10.1.0.0/16", GatewayID: "local"}
	return &awscloud.Network{
		AccountID: "444455556666", Region: "us-east-1",
		VPC:              awscloud.VPC{ID: "vpc-1", CIDRBlock: "10.1.0.0/16", Tags: map[string]string{"Name": "tf"}},
		InternetGateways: []awscloud.InternetGateway{{ID: "igw-1"}},
		Subnets: []awscloud.Subnet{
			subnet("subnet-pa", "10.1.0.0/24", "use1-az1", awscloud.SubnetRolePublic, "rtb-pub"),
			subnet("subnet-pb", "10.1.1.0/24", "use1-az2", awscloud.SubnetRolePublic, "rtb-pub"),
			subnet("subnet-xa", "10.1.16.0/20", "use1-az1", awscloud.SubnetRolePrivate, "rtb-a"),
			subnet("subnet-xb", "10.1.32.0/20", "use1-az2", awscloud.SubnetRolePrivate, "rtb-b"),
		},
		RouteTables: []awscloud.RouteTable{
			{ID: "rtb-main", Main: true, Associations: []awscloud.RouteTableAssociation{{ID: "rtbassoc-main", Main: true}},
				Routes: []awscloud.Route{local}},
			{ID: "rtb-pub", Associations: []awscloud.RouteTableAssociation{assoc("rtbassoc-pa", "subnet-pa"), assoc("rtbassoc-pb", "subnet-pb")},
				Routes: []awscloud.Route{local, {DestinationCIDRBlock: "0.0.0.0/0", GatewayID: "igw-1"}}},
			{ID: "rtb-a", Associations: []awscloud.RouteTableAssociation{assoc("rtbassoc-xa", "subnet-xa")},
				Routes: []awscloud.Route{local, {DestinationCIDRBlock: "0.0.0.0/0", NATGatewayID: "nat-a"},
					{DestinationCIDRBlock: "172.16.0.0/12", TransitGatewayID: "tgw-1"}}},
			{ID: "rtb-b", Associations: []awscloud.RouteTableAssociation{assoc("rtbassoc-xb", "subnet-xb")},
				Routes: []awscloud.Route{local, {DestinationCIDRBlock: "0.0.0.0/0", NATGatewayID: "nat-b"}}},
		},
		NATGateways: []awscloud.NATGateway{
			{ID: "nat-a", SubnetID: "subnet-pa", AllocationIDs: []string{"eipalloc-a"}},
			{ID: "nat-b", SubnetID: "subnet-pb", AllocationIDs: []string{"eipalloc-b"}},
		},
		DefaultSecurityGroup: awscloud.SecurityGroup{ID: "sg-default", Name: "default"},
	}
}

func importRequest(bootstrapID string) *cloudv1.ImportNetworkRequest {
	return &cloudv1.ImportNetworkRequest{TenantId: tenant, Name: "tf", Region: "us-east-1", BootstrapId: bootstrapID,
		Provider: &cloudv1.ImportNetworkRequest_Aws{Aws: &cloudv1.AwsImportNetwork{VpcId: "vpc-0a1b2c3d4e5f60718",
			SubnetIds: []string{"subnet-0aaaaaaaaaaaaaaaa"}}}}
}

func dispositions(imp *cloudv1.ImportStatus) map[cloudv1.Disposition]int {
	out := map[cloudv1.Disposition]int{}
	for _, l := range imp.GetResources() {
		out[l.GetDisposition()]++
	}
	return out
}

func TestImportNetwork(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}

	n := f.imported(t)
	if n.GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_IMPORTED || n.GetName() != "tf" {
		t.Errorf("state = %s (%s)", n.GetStatus().GetState(), n.GetStatus().GetMessage())
	}
	l := n.GetSpec().GetLayout()
	if l.GetCidrBlock() != "10.1.0.0/16" || len(l.GetSubnets()) != 4 ||
		l.GetNatGatewayMode() != cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE ||
		l.GetSubnets()[0].GetRole() != cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC {
		t.Errorf("derived layout = %v", l)
	}
	imp := n.GetStatus().GetImport()
	d := dispositions(imp)
	// 4 subnets lack their load balancer tag; the Transit Gateway route and
	// the default security group stay out. The main route table is not
	// listed: no imported subnet uses it.
	if imp.GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED || d[cloudv1.Disposition_DISPOSITION_WILL_UPDATE] != 4 ||
		d[cloudv1.Disposition_DISPOSITION_NOT_IMPORTED] != 2 || d[cloudv1.Disposition_DISPOSITION_BLOCKING] != 0 {
		t.Errorf("plan = %s %v", imp.GetPhase(), d)
	}
	// vpc, igw, 4 subnets, 2 eip + 2 nat, 3 route tables, 3 routes, 4 associations
	if got := len(n.GetStatus().GetManagedResources()); got != 20 {
		t.Errorf("managed resources = %d, want 20", got)
	}
	vpc := &awsec2.VPC{}
	if err := f.kube.Get(t.Context(), key(n, "vpc"), vpc); err != nil {
		t.Fatal(err)
	}
	if vpc.Spec.ManagementPolicies[0] != "Observe" || vpc.Annotations["crossplane.io/external-name"] != "vpc-1" {
		t.Errorf("vpc = %v %v", vpc.Spec.ManagementPolicies, vpc.Annotations)
	}
	route := &awsec2.Route{}
	if err := f.kube.Get(t.Context(), key(n, "route-private-use1-az2"), route); err != nil {
		t.Fatal(err)
	}
	if *route.Spec.ForProvider.NATGatewayID != "nat-b" {
		t.Errorf("zone 2 private route = %+v", route.Spec.ForProvider)
	}

	// Imported networks are read-only until taken over.
	_, err := f.svc.UpdateNetwork(t.Context(), &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network: &cloudv1.Network{Id: n.GetId(), DisplayName: "x"}, UpdateMask: mask("display_name")})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("update imported = %v, want FAILED_PRECONDITION", err)
	}
}

func TestTakeOverNetwork(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	n := res.GetNetwork()
	req := &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: n.GetId(), AcknowledgeNotImported: true}
	// Nothing to own before the import's objects are there.
	if _, err := f.svc.TakeOverNetwork(t.Context(), req); connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "still being imported") {
		t.Fatalf("takeover before the import is applied = %v, want FAILED_PRECONDITION", err)
	}
	f.reconcile(t, n.GetId())
	req.AcknowledgeNotImported = false

	if _, err := f.svc.TakeOverNetwork(t.Context(), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("takeover without acknowledgement = %v, want FAILED_PRECONDITION", err)
	}
	req.AcknowledgeNotImported = true
	out, err := f.svc.TakeOverNetwork(t.Context(), req)
	if err != nil {
		t.Fatalf("TakeOverNetwork: %v", err)
	}
	st := out.GetNetwork().GetStatus()
	if st.GetState() != cloudv1.NetworkState_NETWORK_STATE_PROVISIONING || st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_CONVERGING {
		t.Errorf("after takeover: %s / %s", st.GetState(), st.GetImport().GetPhase())
	}
	// The call stored the takeover; the reconciler owns the objects.
	sub := &awsec2.Subnet{}
	if err := f.kube.Get(t.Context(), key(n, "public-use1-az1"), sub); err != nil || sub.Spec.ManagementPolicies[0] != "Observe" {
		t.Fatalf("subnet right after the call = %v (%v): the call must not own", sub.Spec.ManagementPolicies, err)
	}
	f.reconcile(t, n.GetId())
	if err := f.kube.Get(t.Context(), key(n, "public-use1-az1"), sub); err != nil {
		t.Fatal(err)
	}
	if sub.Spec.ManagementPolicies[0] != "*" || *sub.Spec.ForProvider.Tags["kubernetes.io/role/elb"] != "1" ||
		*sub.Spec.ForProvider.Tags["Name"] != "subnet-pa" {
		t.Errorf("subnet after takeover = %v %v", sub.Spec.ManagementPolicies, sub.Spec.ForProvider.Tags)
	}
	if _, err := f.svc.TakeOverNetwork(t.Context(), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second takeover = %v, want FAILED_PRECONDITION", err)
	}
}

func TestImportNetworkBlocked(t *testing.T) {
	f := newFixture(t)
	vpc := existingVPC()
	vpc.Subnets[3].Role = awscloud.SubnetRoleIsolated // private egress is not through a NAT gateway
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: vpc}

	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	// The call stored the import; the reconciler finds the plan blocked.
	if st := res.GetNetwork().GetStatus(); st.GetState() != cloudv1.NetworkState_NETWORK_STATE_IMPORTED ||
		st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING {
		t.Errorf("right after the call = %s / %s, want IMPORTED and DISCOVERING", st.GetState(), st.GetImport().GetPhase())
	}
	f.reconcile(t, res.GetNetwork().GetId())
	for name, n := range map[string]*cloudv1.Network{"stored": f.get(t, res.GetNetwork().GetId()), "live": f.status(t, res.GetNetwork().GetId())} {
		st := n.GetStatus()
		if st.GetState() != cloudv1.NetworkState_NETWORK_STATE_FAILED || st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED {
			t.Errorf("%s blocked import = %s / %s (%s)", name, st.GetState(), st.GetImport().GetPhase(), st.GetMessage())
		}
	}
	if got := f.count(t, &awsec2.VPCList{}, res.GetNetwork().GetId()); got != 0 {
		t.Errorf("blocked import created %d VPCs", got)
	}
	// The blocked network still stands for the VPC: it is deleted before
	// the VPC is imported again.
	if _, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("a second import next to the blocked one = %v, want ALREADY_EXISTS", err)
	}
	if _, err := f.svc.DeleteNetwork(t.Context(), &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: res.GetNetwork().GetId()}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Reconcile(t.Context(), tenant, res.GetNetwork().GetId()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID)); err != nil {
		t.Errorf("importing the VPC again once the blocked network is deleted: %v", err)
	}
}

// The call never looks at the cloud: what the lookup finds wrong is the
// network's status once the reconciler made it. A VPC that is not there
// or cannot be read is FAILED with the reason; a lookup that did not
// answer is tried again.
func TestImportNetworkErrorsAreStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		discovery awscloud.NetworkDiscoverer
		state     cloudv1.NetworkState
		message   string
	}{
		"not configured": {nil, cloudv1.NetworkState_NETWORK_STATE_IMPORTED, "retrying: AWS lookups are not configured"},
		"lookup failed": {fakeDiscoverer{err: errors.New("timeout")}, cloudv1.NetworkState_NETWORK_STATE_IMPORTED,
			"retrying: the AWS lookup failed: timeout"},
		"access denied": {fakeDiscoverer{err: awscloud.ErrAccessDenied}, cloudv1.NetworkState_NETWORK_STATE_FAILED,
			"cannot read the VPC with the account's credentials"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.svc.deps.AWSDiscovery = noAWS{t}
			res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
			if err != nil {
				t.Fatalf("ImportNetwork: %v; the call stores the import whatever the cloud says", err)
			}
			id := res.GetNetwork().GetId()
			f.svc.deps.AWSDiscovery = tc.discovery
			if err := f.svc.Reconcile(t.Context(), tenant, id); err == nil {
				t.Error("the pass reported no error")
			}
			for view, n := range map[string]*cloudv1.Network{"stored": f.get(t, id), "live": f.status(t, id)} {
				st := n.GetStatus()
				if st.GetState() != tc.state || !strings.Contains(st.GetMessage(), tc.message) ||
					st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_DISCOVERING {
					t.Errorf("%s status = %s %q (%s), want %s with %q, still DISCOVERING", view, st.GetState(), st.GetMessage(),
						st.GetImport().GetPhase(), tc.state, tc.message)
				}
			}
			if got := f.count(t, &awsec2.VPCList{}, id); got != 0 {
				t.Errorf("a failed lookup created %d VPCs", got)
			}
			// Once the cloud answers the import goes on.
			f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
			f.reconcile(t, id)
			if st := f.status(t, id).GetStatus(); st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_PLANNED ||
				st.GetState() != cloudv1.NetworkState_NETWORK_STATE_IMPORTED {
				t.Errorf("after the lookup succeeded = %s / %s (%s)", st.GetState(), st.GetImport().GetPhase(), st.GetMessage())
			}
		})
	}
}

// A lookup that says the import cannot succeed (no such VPC, an ID AWS
// refuses) is the end of it: the network is FAILED with the reason, its
// import BLOCKED, and the cloud is not asked again. The way on is to
// delete the import and import again.
func TestImportNetworkThatCannotSucceedStops(t *testing.T) {
	for name, tc := range map[string]struct {
		err     error
		message string
	}{
		"vpc not found": {awscloud.ErrNotFound, "not found"},
		"bad vpc id":    {awscloud.ErrInvalid, "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.svc.deps.AWSDiscovery = noAWS{t}
			res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
			if err != nil {
				t.Fatalf("ImportNetwork: %v", err)
			}
			id := res.GetNetwork().GetId()
			f.svc.deps.AWSDiscovery = fakeDiscoverer{err: tc.err}

			if err := f.svc.Reconcile(t.Context(), tenant, id); err != nil {
				t.Fatalf("the pass failed: %v; what the lookup said is the network's status", err)
			}

			check := func(when string) {
				t.Helper()
				for view, n := range map[string]*cloudv1.Network{"stored": f.get(t, id), "live": f.status(t, id)} {
					st := n.GetStatus()
					if st.GetState() != cloudv1.NetworkState_NETWORK_STATE_FAILED || !strings.Contains(st.GetMessage(), tc.message) ||
						st.GetImport().GetPhase() != cloudv1.ImportPhase_IMPORT_PHASE_BLOCKED {
						t.Errorf("%s, %s status = %s %q (%s), want FAILED with %q, BLOCKED", when, view, st.GetState(), st.GetMessage(),
							st.GetImport().GetPhase(), tc.message)
					}
				}
			}
			check("after the lookup")
			// The cloud would answer now: it is not asked.
			lookups := 0
			f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC(), lookups: &lookups}
			f.reconcile(t, id)
			f.reconcile(t, id)
			if lookups != 0 {
				t.Errorf("%d lookups of a VPC whose import cannot succeed, want none", lookups)
			}
			check("two passes later")
			if got := f.count(t, &awsec2.VPCList{}, id); got != 0 {
				t.Errorf("an import that cannot succeed created %d VPCs", got)
			}
		})
	}
}

func TestImportListsMainRouteTableWhenUsed(t *testing.T) {
	f := newFixture(t)
	vpc := existingVPC()
	vpc.Subnets[3].RouteTableID = "rtb-main" // no association of its own
	vpc.RouteTables[3].Associations = nil
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: vpc}
	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, res.GetNetwork().GetId())
	found := false
	for _, l := range f.get(t, res.GetNetwork().GetId()).GetStatus().GetImport().GetResources() {
		found = found || (l.GetKind() == "DefaultRouteTable" && l.GetDisposition() == cloudv1.Disposition_DISPOSITION_NOT_IMPORTED)
	}
	if !found {
		t.Error("the main route table used by a subnet must be listed NOT_IMPORTED")
	}
}

func TestImportRefusesAVPCThatHasANetwork(t *testing.T) {
	f := newFixture(t)
	f.svc.deps.AWSDiscovery = fakeDiscoverer{network: existingVPC()}
	first, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, first.GetNetwork().GetId())
	// The import request names vpc-0a1b2c3d4e5f60718; the fake discovery
	// returns vpc-1. Point the request at the VPC now owned.
	again := importRequest(f.accountID)
	again.GetAws().VpcId = "vpc-00000000000000001"
	vpc := &awsec2.VPC{}
	if err := f.kube.Get(t.Context(), key(first.GetNetwork(), "vpc"), vpc); err != nil {
		t.Fatal(err)
	}
	vpc.Annotations["crossplane.io/external-name"] = "vpc-00000000000000001"
	if err := f.kube.Update(t.Context(), vpc); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.ImportNetwork(t.Context(), again)
	if connect.CodeOf(err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), first.GetNetwork().GetId()) {
		t.Fatalf("second import of the same VPC = %v, want ALREADY_EXISTS naming %s", err, first.GetNetwork().GetId())
	}
}
