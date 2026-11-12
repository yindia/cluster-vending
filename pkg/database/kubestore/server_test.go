package kubestore_test

import (
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/kubestore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/network"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

const (
	tenant   = "acme"
	platform = "cluster-vending-system"
)

// The network service, unchanged, on the custom resource store: a network
// is created, moved on by the loop, updated and deleted, and its row and
// records are objects in the platform namespace throughout.
func TestNetworkServiceRunsOnCustomResources(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&awsec2.InternetGateway{}).Build()
	store := kubestore.New(k, platform)
	ctx := t.Context()
	if err := store.Tenants().Create(ctx, &model.Tenant{ID: tenant}); err != nil {
		t.Fatal(err)
	}
	account := &model.CloudAccount{TenantID: tenant, Provider: model.ProviderAWS, AccountID: "444455556666",
		ProviderConfigName: "aws-0a1b2c3d", Namespace: "tenant-acme", Identity: "arn:aws:iam::444455556666:role/provisioner"}
	if err := store.Tenants().PutCloudAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	svc := network.NewNetworkService(service.Deps{Store: store, Kube: k})

	created, err := svc.CreateNetwork(ctx, &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{
		Name: "prod", Labels: map[string]string{"team": "platform"},
		Spec: &cloudv1.NetworkSpec{Region: "us-east-1", BootstrapId: account.ID, Layout: &cloudv1.NetworkLayout{}},
	}})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	id := created.GetNetwork().GetId()
	rows, records := &platformv1alpha1.NetworkList{}, &platformv1alpha1.ManagedResourceList{}
	if err := k.List(ctx, rows, client.InNamespace(platform)); err != nil || len(rows.Items) != 1 || rows.Items[0].Name != id {
		t.Fatalf("%d Network objects (%v), want the created one", len(rows.Items), err)
	}
	// The call only stored the network; a pass of the loop applies it.
	if err := svc.Reconcile(ctx, tenant, id); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := k.List(ctx, records, client.InNamespace(platform)); err != nil || len(records.Items) == 0 {
		t.Fatalf("%d ManagedResource objects (%v), want the network's records", len(records.Items), err)
	}

	// The internet gateway is attached: the loop applies the NAT gateways.
	gateways := &awsec2.InternetGatewayList{}
	if err := k.List(ctx, gateways, client.MatchingLabels{crossplane.LabelNetwork: id}); err != nil || len(gateways.Items) != 1 {
		t.Fatalf("%d internet gateways (%v)", len(gateways.Items), err)
	}
	gateways.Items[0].SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	if err := k.Status().Update(ctx, &gateways.Items[0]); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reconcile(ctx, tenant, id); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	nats := &awsec2.NATGatewayList{}
	if err := k.List(ctx, nats, client.MatchingLabels{crossplane.LabelNetwork: id}); err != nil || len(nats.Items) == 0 {
		t.Fatalf("%d NAT gateways after the loop's pass (%v)", len(nats.Items), err)
	}
	row, err := store.Networks().Get(ctx, tenant, id)
	if err != nil || row.AppliedGeneration != row.Generation {
		t.Fatalf("applied generation %d of %d (%v)", row.AppliedGeneration, row.Generation, err)
	}

	got, err := svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: id})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network:    &cloudv1.Network{Id: id, Etag: got.GetNetwork().GetEtag(), DisplayName: "Production"},
		UpdateMask: mask("display_name")})
	if err != nil || updated.GetNetwork().GetDisplayName() != "Production" || updated.GetNetwork().GetEtag() == got.GetNetwork().GetEtag() {
		t.Fatalf("UpdateNetwork = %v, %v", updated.GetNetwork(), err)
	}
	_, err = svc.UpdateNetwork(ctx, &cloudv1.UpdateNetworkRequest{TenantId: tenant,
		Network:    &cloudv1.Network{Id: id, Etag: got.GetNetwork().GetEtag(), DisplayName: "stale"},
		UpdateMask: mask("display_name")})
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("an update with a stale etag = %v, want ABORTED", err)
	}
	list, err := svc.ListNetworks(ctx, &cloudv1.ListNetworksRequest{TenantId: tenant})
	if err != nil || len(list.GetNetworks()) != 1 {
		t.Fatalf("ListNetworks = %d (%v)", len(list.GetNetworks()), err)
	}

	if _, err := svc.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: id}); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	for range 10 { // tier by tier, as the loop does
		if err := svc.Reconcile(ctx, tenant, id); err != nil {
			t.Fatalf("Reconcile while deleting: %v", err)
		}
	}
	var codeErr *connect.Error
	if _, err := svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: id}); !errors.As(err, &codeErr) ||
		codeErr.Code() != connect.CodeNotFound {
		t.Errorf("GetNetwork after the deletion = %v, want NOT_FOUND", err)
	}
	for _, left := range []client.ObjectList{&platformv1alpha1.NetworkList{}, &platformv1alpha1.ManagedResourceList{}} {
		if err := k.List(ctx, left, client.InNamespace(platform)); err != nil || meta.LenList(left) != 0 {
			t.Errorf("%d %T objects left after the deletion (%v)", meta.LenList(left), left, err)
		}
	}
}

func mask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
