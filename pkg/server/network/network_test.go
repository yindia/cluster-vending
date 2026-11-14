package network

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/uuid"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"go.uber.org/mock/gomock"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/repository/mock"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	"github.com/yindia/cluster-vending/pkg/fault"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
)

const (
	tenant    = "acme"
	unknownID = "0190f1a2-0000-7000-8000-00000000dead"
)

type fixture struct {
	svc       *NetworkService
	kube      client.Client
	store     repository.Store
	storeKube client.Client // the API server the store keeps its rows in
	accountID string        // bootstrap ID of the tenant's AWS account
}

// newFixture is a NetworkService on a store (storetest.NewStore) with a
// bootstrapped AWS account whose access was verified in us-east-1, and a
// fake Kubernetes client.
func newFixture(t *testing.T, funcs ...interceptor.Funcs) fixture {
	t.Helper()
	f := newUnverifiedFixture(t, funcs...)
	storetest.SetAccess(t, f.storeKube, tenant, f.accountID, storetest.Verified("us-east-1"))
	return f
}

// newUnverifiedFixture is newFixture before the controller checked the
// account's access.
func newUnverifiedFixture(t *testing.T, funcs ...interceptor.Funcs) fixture {
	t.Helper()
	storeKube := storetest.NewClient(t)
	store := cloudstore.New(storeKube)
	ctx := t.Context()
	if err := store.Tenants().Create(ctx, &model.Tenant{ID: tenant}); err != nil {
		t.Fatal(err)
	}
	account := &model.CloudAccount{TenantID: tenant, Provider: model.ProviderAWS, AccountID: "444455556666",
		ProviderConfigName: "aws-0a1b2c3d", Namespace: "tenant-acme",
		Identity: "arn:aws:iam::444455556666:role/provisioner"}
	if err := store.Tenants().PutCloudAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Tenants().GetCloudAccount(ctx, tenant, model.ProviderAWS, "444455556666")
	if err != nil {
		t.Fatal(err)
	}
	k := fakeKube(t, funcs...)
	return fixture{svc: NewNetworkService(reconcile.Deps{Store: store, Kube: k}), kube: k, store: store, storeKube: storeKube,
		accountID: stored.ID}
}

func fakeKube(t *testing.T, funcs ...interceptor.Funcs) client.Client {
	t.Helper()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(
		&awsec2.VPC{}, &awsec2.Subnet{}, &awsec2.NATGateway{}, &awsec2.EIP{}, &awsec2.InternetGateway{},
		&awsec2.RouteTable{}, &awsec2.Route{}, &awsec2.RouteTableAssociation{})
	for _, fn := range funcs {
		b = b.WithInterceptorFuncs(fn)
	}
	return b.Build()
}

func (f fixture) create(t *testing.T, name string, layout *cloudv1.NetworkLayout) *cloudv1.Network {
	t.Helper()
	res, err := f.svc.CreateNetwork(t.Context(), f.request(name, layout))
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	return f.attachGateway(t, res.GetNetwork().GetId())
}

// imported imports the fixture's VPC (importRequest) and runs the loop's
// Reconcile, which creates its Observe-only objects. It returns the
// network as it is afterwards.
func (f fixture) imported(t *testing.T) *cloudv1.Network {
	t.Helper()
	res, err := f.svc.ImportNetwork(t.Context(), importRequest(f.accountID))
	if err != nil {
		t.Fatalf("ImportNetwork: %v", err)
	}
	f.reconcile(t, res.GetNetwork().GetId())
	return f.status(t, res.GetNetwork().GetId())
}

// takenOver is imported, then taken over and reconciled once: the
// reconciler owns its objects.
func (f fixture) takenOver(t *testing.T) *cloudv1.Network {
	t.Helper()
	n := f.imported(t)
	if _, err := f.svc.TakeOverNetwork(t.Context(), &cloudv1.TakeOverNetworkRequest{TenantId: tenant, NetworkId: n.GetId(),
		AcknowledgeNotImported: true}); err != nil {
		t.Fatalf("TakeOverNetwork: %v", err)
	}
	f.reconcile(t, n.GetId())
	return f.status(t, n.GetId())
}

// reconcile runs one pass of the controller for the network: a call
// only stores what was asked for, this applies it.
func (f fixture) reconcile(t *testing.T, id string) {
	t.Helper()
	if err := f.svc.Reconcile(t.Context(), tenant, id); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// get is the network as GetNetwork returns it: from the store.
func (f fixture) get(t *testing.T, id string) *cloudv1.Network {
	t.Helper()
	res, err := f.svc.GetNetwork(t.Context(), &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: id})
	if err != nil {
		t.Fatalf("GetNetwork: %v", err)
	}
	return res.GetNetwork()
}

// status is the network as GetNetwork with live returns it: read live.
func (f fixture) status(t *testing.T, id string) *cloudv1.Network {
	t.Helper()
	res, err := f.svc.GetNetwork(t.Context(), &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: id, Live: true})
	if err != nil {
		t.Fatalf("GetNetwork live: %v", err)
	}
	return res.GetNetwork()
}

// attachGateway runs the loop's Reconcile (a call only stores the network:
// this applies it), reports the network's internet gateway Ready, as the
// provider does once it is attached, and runs Reconcile again: the NAT
// gateways, held back until then, are applied. It returns the network as
// it is afterwards.
func (f fixture) attachGateway(t *testing.T, id string) *cloudv1.Network {
	t.Helper()
	ctx := t.Context()
	f.reconcile(t, id)
	gateways := &awsec2.InternetGatewayList{}
	if err := f.kube.List(ctx, gateways, client.MatchingLabels{crossplane.LabelNetwork: id}); err != nil {
		t.Fatal(err)
	}
	for i := range gateways.Items {
		g := &gateways.Items[i]
		g.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
		if err := f.kube.Status().Update(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.svc.Reconcile(ctx, tenant, id); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res, err := f.svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: id, Live: true})
	if err != nil {
		t.Fatal(err)
	}
	return res.GetNetwork()
}

func (f fixture) request(name string, layout *cloudv1.NetworkLayout) *cloudv1.CreateNetworkRequest {
	if layout == nil {
		layout = &cloudv1.NetworkLayout{}
	}
	return &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{
		Name: name, DisplayName: "Production", Labels: map[string]string{"team": "platform"},
		Spec: &cloudv1.NetworkSpec{Region: "us-east-1", BootstrapId: f.accountID, Layout: layout},
	}}
}

// key names one managed resource of n: "<name>-<hash8>-<suffix>".
func key(n *cloudv1.Network, suffix string) client.ObjectKey {
	return client.ObjectKey{Namespace: "tenant-acme",
		Name: crossplaneaws.NetworkPrefix(tenant, n.GetId(), n.GetName()) + "-" + suffix}
}

// count returns how many objects of list's kind belong to the network (by
// id).
func (f fixture) count(t *testing.T, list client.ObjectList, network string) int {
	t.Helper()
	if err := f.kube.List(t.Context(), list, client.InNamespace("tenant-acme"),
		client.MatchingLabels{crossplane.LabelNetwork: network}); err != nil {
		t.Fatal(err)
	}
	return meta.LenList(list)
}

func TestCreateNetworkDefaultLayout(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)

	if _, err := uuid.Parse(n.GetId()); err != nil || n.GetName() != "prod" {
		t.Errorf("id, name = %q, %q: want a server-assigned UUID and the given name", n.GetId(), n.GetName())
	}
	if n.GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_PROVISIONING {
		t.Errorf("state = %s", n.GetStatus().GetState())
	}
	if l := n.GetSpec().GetLayout(); l.GetCidrBlock() != "10.0.0.0/16" || len(l.GetSubnets()) != 6 ||
		l.GetNatGatewayMode() != cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_SINGLE {
		t.Errorf("stored layout = %v, want the resolved default", l)
	}
	for list, want := range map[client.ObjectList]int{
		&awsec2.VPCList{}: 1, &awsec2.InternetGatewayList{}: 1, &awsec2.SubnetList{}: 6,
		&awsec2.EIPList{}: 1, &awsec2.NATGatewayList{}: 1, &awsec2.RouteTableList{}: 2,
		&awsec2.RouteList{}: 2, &awsec2.RouteTableAssociationList{}: 6,
	} {
		if got := f.count(t, list, n.GetId()); got != want {
			t.Errorf("%T: %d objects, want %d", list, got, want)
		}
	}

	vpc := &awsec2.VPC{}
	if err := f.kube.Get(t.Context(), key(n, "vpc"), vpc); err != nil {
		t.Fatal(err)
	}
	if ref := vpc.Spec.ProviderConfigReference; ref == nil || ref.Name != "aws-0a1b2c3d" {
		t.Errorf("providerConfigRef = %+v", ref)
	}
	if *vpc.Spec.ForProvider.CidrBlock != "10.0.0.0/16" || *vpc.Spec.ForProvider.Tags["team"] != "platform" ||
		*vpc.Spec.ForProvider.Tags["cluster-vending.io/network"] != n.GetId() ||
		*vpc.Spec.ForProvider.Tags["cluster-vending.io/network-name"] != "prod" {
		t.Errorf("vpc = %+v", vpc.Spec.ForProvider)
	}
	sub := &awsec2.Subnet{}
	if err := f.kube.Get(t.Context(), key(n, "private-use1-az2"), sub); err != nil {
		t.Fatal(err)
	}
	if *sub.Spec.ForProvider.CidrBlock != "10.0.32.0/19" || sub.Spec.ForProvider.VPCIDRef.Name != key(n, "vpc").Name ||
		*sub.Spec.ForProvider.Tags["kubernetes.io/role/internal-elb"] != "1" || sub.Labels[crossplane.LabelZone] != "use1-az2" {
		t.Errorf("private subnet = %+v %v", sub.Spec.ForProvider, sub.Labels)
	}
}

func TestCreateNetworkPerZoneNAT(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "ha", &cloudv1.NetworkLayout{NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_PER_ZONE})
	if got := f.count(t, &awsec2.NATGatewayList{}, n.GetId()); got != 3 {
		t.Errorf("NAT gateways = %d, want 3", got)
	}
	route := &awsec2.Route{}
	if err := f.kube.Get(t.Context(), key(n, "route-private-use1-az3"), route); err != nil {
		t.Fatal(err)
	}
	if route.Spec.ForProvider.NATGatewayIDRef.Name != key(n, "nat-use1-az3").Name {
		t.Errorf("zone 3 routes through %s", route.Spec.ForProvider.NATGatewayIDRef.Name)
	}
}

// markReady sets Ready / Synced and the cloud IDs, as the provider would.
func (f fixture) markReady(t *testing.T, syncError string) {
	t.Helper()
	ctx := t.Context()
	for _, list := range []client.ObjectList{&awsec2.VPCList{}, &awsec2.SubnetList{}, &awsec2.NATGatewayList{}, &awsec2.EIPList{}} {
		if err := f.kube.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		items, _ := meta.ExtractList(list)
		for _, item := range items {
			obj := item.(client.Object)
			switch m := obj.(type) {
			case *awsec2.VPC:
				m.Status.AtProvider.ID = ptr.To("vpc-1")
				m.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
			case *awsec2.Subnet:
				m.Status.AtProvider.ID = ptr.To("subnet-" + m.Labels[crossplane.LabelSubnetRole] + "-" + m.Labels[crossplane.LabelZone])
				m.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
			case *awsec2.NATGateway:
				m.Status.AtProvider.ID = ptr.To("nat-1")
				m.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
			case *awsec2.EIP:
				m.Status.AtProvider.PublicIP = ptr.To("3.3.3.3")
				m.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
				if syncError != "" {
					m.SetConditions(xpv2.ReconcileError(errors.New(syncError)))
				}
			}
			if err := f.kube.Status().Update(ctx, obj); err != nil {
				t.Fatal(err)
			}
			// The provider sets the cloud ID as the external name.
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatal(err)
			}
			obj.SetAnnotations(map[string]string{"crossplane.io/external-name": "ext-" + obj.GetName()})
			if err := f.kube.Update(ctx, obj); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestGetNetworkReportsResources(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	f.markReady(t, "")

	res, err := f.svc.GetNetwork(t.Context(), &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId(), Live: true})
	if err != nil {
		t.Fatal(err)
	}
	st := res.GetNetwork().GetStatus()
	// Only VPC, subnets, NAT and EIP were marked ready: the rest is pending.
	if st.GetState() != cloudv1.NetworkState_NETWORK_STATE_PROVISIONING {
		t.Errorf("state = %s (%s)", st.GetState(), st.GetMessage())
	}
	r := st.GetResources()
	if r.GetCloudNetworkId() != "vpc-1" || len(r.GetPublicSubnetIds()) != 3 || len(r.GetPrivateSubnetIds()) != 3 ||
		r.GetNatGatewayIds()[0] != "nat-1" || r.GetEgressIps()[0] != "3.3.3.3" {
		t.Errorf("resources = %v", r)
	}
	if r.GetPrivateSubnetIds()[0] != "subnet-private-use1-az1" {
		t.Errorf("private subnets = %v", r.GetPrivateSubnetIds())
	}

	views := st.GetManagedResources()
	if len(views) != 20 {
		t.Fatalf("managed resources = %d, want 20", len(views))
	}
	first := views[0]
	if first.GetKind() != "VPC" || first.GetName() != key(res.GetNetwork(), "vpc").Name ||
		first.GetState() != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY ||
		first.GetExternalName() != "ext-"+first.GetName() {
		t.Errorf("first resource = %v, want the VPC, ready, with its cloud ID", first)
	}
	byState := map[cloudv1.ManagedResourceState]int{}
	for _, v := range views {
		byState[v.GetState()]++
		if v.GetKind() == "Subnet" && (v.GetZoneId() == "" || v.GetRole() == "") {
			t.Errorf("subnet %s without zone or role", v.GetName())
		}
	}
	// VPC, internet gateway, 6 subnets, NAT and EIP are ready; the rest are
	// still creating.
	if byState[cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY] != 10 {
		t.Errorf("states = %v", byState)
	}
}

func TestCreateNetworkErrors(t *testing.T) {
	f := newFixture(t)
	f.create(t, "prod", nil)
	ctx := t.Context()

	other := f.request("prod2", nil)
	other.Network.Spec.BootstrapId = "0b1f6b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
	regional := f.request("prod3", &cloudv1.NetworkLayout{NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_REGIONAL})
	badRegion := f.request("prod4", nil)
	badRegion.Network.Spec.Region = "us-north-9"
	outside := f.request("prod5", &cloudv1.NetworkLayout{CidrBlock: "10.0.0.0/16", Subnets: []*cloudv1.Subnet{
		{Role: cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC, CidrBlock: "10.9.0.0/24", ZoneId: "use1-az1"},
		{Role: cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE, CidrBlock: "10.0.16.0/20", ZoneId: "use1-az1"},
		{Role: cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE, CidrBlock: "10.0.32.0/20", ZoneId: "use1-az2"},
	}})
	for name, tc := range map[string]struct {
		req  *cloudv1.CreateNetworkRequest
		want connect.Code
	}{
		"account not bootstrapped": {other, connect.CodeFailedPrecondition},
		"regional NAT":             {regional, connect.CodeUnimplemented},
		"unknown region":           {badRegion, connect.CodeInvalidArgument},
		"subnet outside cidr":      {outside, connect.CodeInvalidArgument},
		"invalid request":          {&cloudv1.CreateNetworkRequest{TenantId: tenant}, connect.CodeInvalidArgument},
	} {
		if _, err := f.svc.CreateNetwork(ctx, tc.req); connect.CodeOf(err) != tc.want {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
}

// A network's name is the tenant's once (it is the Network object's name
// in the tenant's namespace); networks of other names have their own
// resources.
func TestNetworkNamesAreUniquePerTenant(t *testing.T) {
	f := newFixture(t)
	a, b := f.create(t, "shared", nil), f.create(t, "other", nil)
	if a.GetId() == b.GetId() || key(a, "vpc") == key(b, "vpc") {
		t.Fatalf("two networks share id or resources: %s %s", a.GetId(), b.GetId())
	}
	if f.count(t, &awsec2.VPCList{}, a.GetId()) != 1 || f.count(t, &awsec2.VPCList{}, b.GetId()) != 1 {
		t.Error("each network must have its own VPC")
	}
	_, err := f.svc.CreateNetwork(t.Context(), f.request("shared", nil))
	if connect.CodeOf(err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), "unique per tenant") {
		t.Errorf("a second network named shared = %v, want ALREADY_EXISTS naming the rule", err)
	}
}

func TestCreateNetworkStoresInput(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil) // layout {}: defaults
	row, err := f.store.Networks().Get(t.Context(), tenant, n.GetId())
	if err != nil {
		t.Fatal(err)
	}
	input := &cloudv1.Network{}
	if err := protojson.Unmarshal(row.Input, input); err != nil {
		t.Fatal(err)
	}
	if input.GetName() != "prod" || len(input.GetSpec().GetLayout().GetSubnets()) != 0 {
		t.Errorf("input = %v, want the request as sent (no resolved subnets)", input)
	}
	if len(n.GetSpec().GetLayout().GetSubnets()) != 6 {
		t.Errorf("spec must hold the resolved layout")
	}
}

func TestUpdateNetworkUpdatesInput(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil) // layout {}: defaults
	upd := &cloudv1.Network{Id: n.GetId(), DisplayName: "Prod", Labels: map[string]string{"tier": "gold"}}
	if _, err := f.svc.UpdateNetwork(t.Context(), &cloudv1.UpdateNetworkRequest{TenantId: tenant, Network: upd,
		UpdateMask: mask("display_name", "labels")}); err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	row, err := f.store.Networks().Get(t.Context(), tenant, n.GetId())
	if err != nil {
		t.Fatal(err)
	}
	input := &cloudv1.Network{}
	if err := protojson.Unmarshal(row.Input, input); err != nil {
		t.Fatal(err)
	}
	if input.GetDisplayName() != "Prod" || input.GetLabels()["tier"] != "gold" {
		t.Errorf("input = %v, want the updated fields", input)
	}
	if input.GetName() != "prod" || input.GetSpec().GetRegion() == "" || len(input.GetSpec().GetLayout().GetSubnets()) != 0 {
		t.Errorf("input = %v, want the rest as first sent (no resolved subnets)", input)
	}
}

func TestUpdateNetworkLeavesMissingInputAlone(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	row, err := f.store.Networks().Get(t.Context(), tenant, n.GetId())
	if err != nil {
		t.Fatal(err)
	}
	row.Input = nil // a network stored before inputs were kept
	if err := f.store.Networks().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateNetwork(t.Context(), &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network: &cloudv1.Network{Id: n.GetId(), DisplayName: "Prod"}, UpdateMask: mask("display_name")}); err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	if row, err = f.store.Networks().Get(t.Context(), tenant, n.GetId()); err != nil {
		t.Fatal(err)
	}
	if len(row.Input) != 0 {
		t.Errorf("input = %s, want none: the original request is unknown", row.Input)
	}
}

func TestCreateNetworkIsIdempotentByRequestID(t *testing.T) {
	f := newFixture(t)
	req := f.request("prod", nil)
	req.RequestId = "6b1f2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	first, err := f.svc.CreateNetwork(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.CreateNetwork(t.Context(), req)
	if err != nil || again.GetNetwork().GetEtag() != first.GetNetwork().GetEtag() {
		t.Fatalf("retry = %v, %v; want the first network", again, err)
	}
}

// A network whose resources cannot be applied is stored all the same: the
// failure is its status, and the next pass tries again.
func TestCreateNetworkWithoutProviderIsFailedNotRefused(t *testing.T) {
	missing := true
	f := newFixture(t, interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			if missing {
				return &meta.NoKindMatchError{GroupKind: awsec2.VPC_GroupVersionKind.GroupKind()}
			}
			return c.Apply(ctx, obj, opts...)
		},
	})
	ctx := t.Context()
	res, err := f.svc.CreateNetwork(ctx, f.request("prod", nil))
	if err != nil || res.GetNetwork().GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_PROVISIONING {
		t.Fatalf("CreateNetwork = %s, %v; want the network stored as PROVISIONING", res.GetNetwork().GetStatus().GetState(), err)
	}
	id := res.GetNetwork().GetId()
	if err := f.svc.Reconcile(ctx, tenant, id); err == nil {
		t.Fatal("the pass reported no error although the provider is not installed")
	}
	const want = "a Crossplane AWS provider the network needs (ec2, iam, cloudwatchlogs) is not installed in the management cluster"
	get, err := f.svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: id})
	if err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]*cloudv1.NetworkStatus{"stored": get.GetNetwork().GetStatus(), "live": f.status(t, id).GetStatus()} {
		if st.GetState() != cloudv1.NetworkState_NETWORK_STATE_FAILED || st.GetMessage() != want {
			t.Errorf("%s status = %s %q, want FAILED with the missing provider", name, st.GetState(), st.GetMessage())
		}
	}
	// It is still tried: once the provider is there the network is built.
	missing = false
	n := f.attachGateway(t, id)
	if st := n.GetStatus(); st.GetState() == cloudv1.NetworkState_NETWORK_STATE_FAILED || len(st.GetManagedResources()) != 20 {
		t.Errorf("after the provider is installed: %s %q, %d resources", st.GetState(), st.GetMessage(), len(st.GetManagedResources()))
	}
}

func TestUpdateNetwork(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	ctx := t.Context()
	upd := &cloudv1.Network{Id: n.GetId(), DisplayName: "Prod", Etag: n.GetEtag(),
		Labels: map[string]string{"team": "network"}}

	// Labels are the AWS tags: updating them re-tags every resource.
	res, err := f.svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant, Network: upd,
		UpdateMask: mask("display_name", "labels")})
	if err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	if res.GetNetwork().GetDisplayName() != "Prod" || res.GetNetwork().GetLabels()["team"] != "network" ||
		res.GetNetwork().GetGeneration() != n.GetGeneration()+1 {
		t.Errorf("updated = %v", res.GetNetwork())
	}
	// The call stored the change; the reconciler applies it.
	vpc := &awsec2.VPC{}
	if err := f.kube.Get(ctx, key(n, "vpc"), vpc); err != nil || *vpc.Spec.ForProvider.Tags["team"] == "network" {
		t.Fatalf("vpc tags right after the call = %v (%v): the call must not apply", vpc.Spec.ForProvider.Tags, err)
	}
	f.reconcile(t, n.GetId())
	if err := f.kube.Get(ctx, key(n, "vpc"), vpc); err != nil {
		t.Fatal(err)
	}
	if *vpc.Spec.ForProvider.Tags["team"] != "network" {
		t.Errorf("vpc tags not re-applied: %v", vpc.Spec.ForProvider.Tags)
	}

	for name, tc := range map[string]struct {
		req  *cloudv1.UpdateNetworkRequest
		want connect.Code
	}{
		"stale etag":   {&cloudv1.UpdateNetworkRequest{TenantId: tenant, Network: upd, UpdateMask: mask("display_name")}, connect.CodeAborted},
		"layout":       {&cloudv1.UpdateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{Id: n.GetId()}, UpdateMask: mask("spec.layout")}, connect.CodeInvalidArgument},
		"unknown":      {&cloudv1.UpdateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{Id: unknownID}, UpdateMask: mask("display_name")}, connect.CodeNotFound},
		"reserved tag": {&cloudv1.UpdateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{Id: n.GetId(), Labels: map[string]string{"aws:x": "y"}}, UpdateMask: mask("labels")}, connect.CodeInvalidArgument},
		"spec.tags":    {&cloudv1.UpdateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{Id: n.GetId()}, UpdateMask: mask("spec.tags")}, connect.CodeInvalidArgument},
	} {
		if _, err := f.svc.UpdateNetwork(ctx, tc.req); connect.CodeOf(err) != tc.want {
			t.Errorf("%s: %v, want %s", name, err, tc.want)
		}
	}
}

func TestDeleteNetwork(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	ctx := t.Context()

	res, err := f.svc.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()})
	if err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if res.GetNetwork().GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_DELETING {
		t.Errorf("state = %s", res.GetNetwork().GetStatus().GetState())
	}
	// The call only stores the deletion.
	count := func(list client.ObjectList) int { return f.count(t, list, n.GetId()) }
	if count(&awsec2.RouteList{}) == 0 || count(&awsec2.VPCList{}) != 1 {
		t.Fatalf("right after the call: routes %d, VPC %d; the call must not delete", count(&awsec2.RouteList{}), count(&awsec2.VPCList{}))
	}
	// Deleted in order, one tier per pass (the fake client deletes at once):
	// the first pass removes the routes and associations only.
	f.reconcile(t, n.GetId())
	if count(&awsec2.RouteList{}) != 0 || count(&awsec2.RouteTableAssociationList{}) != 0 ||
		count(&awsec2.NATGatewayList{}) == 0 || count(&awsec2.SubnetList{}) == 0 || count(&awsec2.VPCList{}) != 1 {
		t.Fatalf("after delete: routes %d, associations %d, NATs %d, subnets %d, VPC %d", count(&awsec2.RouteList{}),
			count(&awsec2.RouteTableAssociationList{}), count(&awsec2.NATGatewayList{}), count(&awsec2.SubnetList{}),
			count(&awsec2.VPCList{}))
	}
	// Reads change nothing: the loop's Reconcile moves the deletion on,
	// tier by tier; dependencies outlive their dependents.
	for i := 0; ; i++ {
		_, err := f.svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId(), Live: true})
		if connect.CodeOf(err) == connect.CodeNotFound {
			break
		}
		if err != nil || i > 10 {
			t.Fatalf("get %d: %v", i, err)
		}
		before := count(&awsec2.RouteTableList{}) + count(&awsec2.NATGatewayList{}) + count(&awsec2.EIPList{}) +
			count(&awsec2.SubnetList{}) + count(&awsec2.VPCList{})
		if _, err := f.svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId(), Live: true}); err != nil {
			t.Fatal(err)
		}
		if after := count(&awsec2.RouteTableList{}) + count(&awsec2.NATGatewayList{}) + count(&awsec2.EIPList{}) +
			count(&awsec2.SubnetList{}) + count(&awsec2.VPCList{}); after != before {
			t.Fatal("a read deleted resources")
		}
		if err := f.svc.Reconcile(ctx, tenant, n.GetId()); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if count(&awsec2.NATGatewayList{}) > 0 && (count(&awsec2.EIPList{}) == 0 || count(&awsec2.SubnetList{}) == 0) {
			t.Fatal("EIPs or subnets deleted before the NAT gateways")
		}
		if count(&awsec2.SubnetList{}) > 0 && count(&awsec2.VPCList{}) == 0 {
			t.Fatal("VPC deleted before its subnets")
		}
	}
	if count(&awsec2.VPCList{}) != 0 {
		t.Error("VPC left after the network is gone")
	}
}

func TestDeleteNetworkInUse(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	row, err := f.store.Networks().Get(t.Context(), tenant, n.GetId())
	if err != nil {
		t.Fatal(err)
	}
	c := &model.Cluster{TenantID: tenant, Name: "eks", NetworkID: row.ID, Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: "x", Spec: model.JSON(`{}`)}}
	if err := f.store.Clusters().Create(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.DeleteNetwork(t.Context(), &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err = %v, want FAILED_PRECONDITION", err)
	}
	got, err := f.svc.GetNetwork(t.Context(), &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId(), Live: true})
	if err != nil || got.GetNetwork().GetStatus().GetClusterIds()[0] != "eks" {
		t.Errorf("cluster_ids = %v, %v", got.GetNetwork().GetStatus().GetClusterIds(), err)
	}
}

func TestListNetworksPages(t *testing.T) {
	f := newFixture(t)
	var ids []string
	for _, name := range []string{"a", "b", "c"} {
		ids = append(ids, f.create(t, name, nil).GetId())
	}
	first, err := f.svc.ListNetworks(t.Context(), &cloudv1.ListNetworksRequest{TenantId: tenant, PageSize: 2})
	if err != nil || len(first.GetNetworks()) != 2 || first.GetNextPageToken() != ids[1] || first.GetNetworks()[0].GetId() != ids[0] {
		t.Fatalf("page 1 = %v, %v; want the two oldest", first, err)
	}
	second, err := f.svc.ListNetworks(t.Context(), &cloudv1.ListNetworksRequest{TenantId: tenant, PageSize: 2, PageToken: ids[1]})
	if err != nil || len(second.GetNetworks()) != 1 || second.GetNextPageToken() != "" || second.GetNetworks()[0].GetId() != ids[2] {
		t.Fatalf("page 2 = %v, %v", second, err)
	}
}

func TestDatabaseErrorsAreInternal(t *testing.T) {
	ctrl := gomock.NewController(t)
	store := mock.NewMockStore(ctrl)
	networks := mock.NewMockNetworks(ctrl)
	store.EXPECT().Networks().Return(networks).AnyTimes()
	networks.EXPECT().Get(gomock.Any(), tenant, unknownID).Return(nil, errors.New("connection refused"))
	svc := NewNetworkService(reconcile.Deps{Store: store, Kube: fakeKube(t)})

	_, err := svc.GetNetwork(t.Context(), &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: unknownID, Live: true})
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v, want INTERNAL", err)
	}
}

func mask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

// Deleting a network records its live objects that have no record (left by
// a failure between apply and record), so they are deleted in order too.
func TestDeleteNetworkRecordsUnrecordedResources(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "old", nil)
	if err := f.store.ManagedResources().DeleteByOwner(t.Context(), model.OwnerNetwork, n.GetId()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.DeleteNetwork(t.Context(), &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t, n.GetId())
	recorded, err := f.store.ManagedResources().ListByOwner(t.Context(), model.OwnerNetwork, n.GetId())
	if err != nil || len(recorded) != 20 || recorded[0].Kind != "VPC" || recorded[len(recorded)-1].Kind != "RouteTableAssociation" {
		t.Fatalf("recorded = %d, %v; want all 20 in creation order", len(recorded), err)
	}
	for i := 0; i < 10; i++ {
		if err := f.svc.Reconcile(t.Context(), tenant, n.GetId()); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.count(t, &awsec2.VPCList{}, n.GetId()); got != 0 {
		t.Errorf("%d VPCs left", got)
	}
}

// A cluster still being deleted blocks deleting its network: only the
// controller removes a cluster, once its resources are confirmed gone
// and the network's subnets are untagged.
func TestDeleteNetworkWaitsForDeletingClusters(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	ctx := t.Context()
	c := &model.Cluster{TenantID: tenant, Name: "eks", NetworkID: n.GetId(), Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: clusterStateDeleting, Spec: model.JSON(`{}`)}}
	if err := f.store.Clusters().Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("DeleteNetwork: %v, want FAILED_PRECONDITION", err)
	}
	if _, err := f.store.Clusters().Get(ctx, tenant, c.ID); err != nil {
		t.Errorf("the deleting cluster's row: %v, want it kept", err)
	}
}

// A taken-over network is never rendered from its spec: its objects came
// from the discovered VPC, and rendering would create a second network.
// One that has nothing kept from its import (it was imported before that
// was kept) has nothing to render from: it says so and renders nothing.
func TestReconcileDoesNotRenderAnImportedNetwork(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	row := &model.Network{TenantID: tenant, Name: "tf", CloudAccountID: f.accountID, Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: netreconcile.StateProvisioning, Generation: 1,
			Spec: model.JSON(`{"region":"us-east-1","bootstrapId":"` + f.accountID +
				`","layout":{"cidrBlock":"10.0.0.0/16","availabilityZoneCount":2}}`),
			Status: model.JSON(`{"import":{"phase":"IMPORT_PHASE_CONVERGING"}}`)}}
	if err := f.store.Networks().Create(ctx, row); err != nil {
		t.Fatal(err)
	}
	err := f.svc.Reconcile(ctx, tenant, row.ID)
	if !fault.Is(err, fault.FailedPrecondition) || !strings.Contains(err.Error(), "import the VPC again") {
		t.Fatalf("Reconcile = %v, want the fault that says to import again", err)
	}
	vpcs := &awsec2.VPCList{}
	if err := f.kube.List(ctx, vpcs); err != nil || len(vpcs.Items) != 0 {
		t.Errorf("%d VPCs rendered (%v), want none", len(vpcs.Items), err)
	}
	if st := f.status(t, row.ID).GetStatus(); !strings.Contains(st.GetMessage(), "import the VPC again") {
		t.Errorf("status = %s %q, want it to say why nothing is applied", st.GetState(), st.GetMessage())
	}
	// A label update does not render it either (see
	// TestUpdateImportedNetworkLabels), and it takes no flow logs.
	if _, err = f.svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network: &cloudv1.Network{Id: row.ID, Labels: map[string]string{"team": "a"}}, UpdateMask: mask("labels")}); err != nil {
		t.Errorf("label update of an imported network: %v", err)
	}
	if err := f.kube.List(ctx, vpcs); err != nil || len(vpcs.Items) != 0 {
		t.Errorf("%d VPCs rendered by a label update (%v), want none", len(vpcs.Items), err)
	}
	_, err = f.svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant, UpdateMask: mask("spec.flow_logs"),
		Network: &cloudv1.Network{Id: row.ID, Spec: &cloudv1.NetworkSpec{FlowLogs: &cloudv1.NetworkFlowLogs{Enabled: ptr.To(true)}}}})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("flow logs on an imported network: %v, want FAILED_PRECONDITION", err)
	}
}

func TestUpdateNetworkNeedsAMask(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	_, err := f.svc.UpdateNetwork(t.Context(), &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network: &cloudv1.Network{Id: n.GetId()}, UpdateMask: mask()})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty update_mask: %v, want INVALID_ARGUMENT", err)
	}
}

// WatchNetwork over a real Connect stream: the first message carries the
// network and one ADDED event per managed resource.
func TestWatchNetworkFirstMessage(t *testing.T) {
	f := newFixture(t)
	n := f.create(t, "prod", nil)
	server := connect.NewServer()
	cloudv1connect.RegisterNetworkServiceHandler(server, f.svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := cloudv1connect.NewNetworkServiceClient(connect.NewClient(connecthttp.NewTransport(srv.Client(), srv.URL)))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := client.WatchNetwork(ctx, &cloudv1.WatchNetworkRequest{TenantId: tenant, NetworkId: n.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	first, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	want := len(n.GetStatus().GetManagedResources())
	if first.GetNetwork().GetId() != n.GetId() || len(first.GetEvents()) != want || want == 0 {
		t.Fatalf("first message: network %q, %d events; want %s and %d", first.GetNetwork().GetId(), len(first.GetEvents()), n.GetId(), want)
	}
	for _, e := range first.GetEvents() {
		if e.GetType() != cloudv1.ManagedResourceEvent_TYPE_ADDED {
			t.Errorf("%s %s: %s, want ADDED", e.GetResource().GetKind(), e.GetResource().GetName(), e.GetType())
		}
	}

	// An unknown network fails the stream with NOT_FOUND.
	bad, err := client.WatchNetwork(ctx, &cloudv1.WatchNetworkRequest{TenantId: tenant, NetworkId: "0190a000-0000-7000-8000-00000000dead"})
	if err == nil {
		_, err = bad.Receive()
		_ = bad.Close()
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown network: %v, want NOT_FOUND", err)
	}
}
