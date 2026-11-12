package cloudstore_test

import (
	"context"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	awscloud "github.com/yindia/cluster-vending/pkg/cloud/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/server/network"
	"github.com/yindia/cluster-vending/pkg/server/service"
)

// emptyVPC is a VPC with nothing in it: its import plan is blocked.
type emptyVPC struct{ lookups *int }

func (e emptyVPC) DiscoverNetwork(_ context.Context, a awscloud.Access, region, vpcID string, _ []string) (*awscloud.Network, error) {
	*e.lookups++
	return &awscloud.Network{AccountID: a.AccountID, Region: region, VPC: awscloud.VPC{ID: vpcID, CIDRBlock: "172.31.0.0/16"}}, nil
}

// An import on the store of the kinds: the call writes a Network object
// that is DISCOVERING, with no layout and nothing of the cloud; the
// reconciler looks the VPC up once and writes the plan onto the same
// object.
func TestImportIsStoredThenDiscoveredOnTheKinds(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&awsec2.InternetGateway{},
		&cloudv1alpha1.CloudAccount{}, &cloudv1alpha1.Network{}, &cloudv1alpha1.Cluster{}).Build()
	store := cloudstore.New(k)
	ctx := t.Context()
	if err := store.Tenants().Create(ctx, &model.Tenant{ID: tenant}); err != nil {
		t.Fatal(err)
	}
	account := &model.CloudAccount{TenantID: tenant, Provider: model.ProviderAWS, AccountID: "444455556666",
		ProviderConfigName: "aws-0a1b2c3d", Namespace: namespace, Identity: "arn:aws:iam::444455556666:role/provisioner"}
	if err := store.Tenants().PutCloudAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	const vpc = "vpc-0123456789abcdef0"
	request := &cloudv1.ImportNetworkRequest{TenantId: tenant, Name: "legacy", Region: "us-east-1", BootstrapId: account.ID,
		Provider: &cloudv1.ImportNetworkRequest_Aws{Aws: &cloudv1.AwsImportNetwork{VpcId: vpc, SubnetIds: []string{"subnet-0123456789abcdef0"}}}}

	// The API server: no AWS lookup at all.
	api := network.NewNetworkService(service.Deps{Store: store, Kube: k})
	res, err := api.ImportNetwork(ctx, request)
	if err != nil {
		t.Fatalf("ImportNetwork: %v", err)
	}
	id := res.GetNetwork().GetId()
	obj := &cloudv1alpha1.Network{}
	key := client.ObjectKey{Namespace: namespace, Name: "legacy"}
	if err := k.Get(ctx, key, obj); err != nil {
		t.Fatalf("the Network object of the import: %v", err)
	}
	if imp := obj.Spec.ImportFrom; imp == nil || imp.AWS.VPCID != vpc || obj.Spec.Layout != nil ||
		obj.Status.ImportPlan == nil || obj.Status.ImportPlan.Phase != cloudv1alpha1.ImportPhaseDiscovering {
		t.Fatalf("right after the call: importFrom %+v, layout %v, plan %+v; want the import stored as Discovering",
			obj.Spec.ImportFrom, obj.Spec.Layout, obj.Status.ImportPlan)
	}
	// A second import of the VPC is refused by the store, before any lookup.
	again := &cloudv1.ImportNetworkRequest{TenantId: tenant, Name: "legacy-2", Region: "us-east-1", BootstrapId: account.ID,
		Provider: request.GetProvider()}
	if _, err := api.ImportNetwork(ctx, again); err == nil {
		t.Error("the VPC was imported twice")
	}

	// The controller: the same store, with the AWS lookup.
	lookups := 0
	controller := network.NewNetworkService(service.Deps{Store: store, Kube: k, AWSDiscovery: emptyVPC{&lookups}})
	for range 2 {
		if err := controller.Reconcile(ctx, tenant, id); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if lookups != 1 {
		t.Errorf("%d lookups of the VPC over two passes, want one", lookups)
	}
	if err := k.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	if obj.Status.ImportPlan == nil || obj.Status.ImportPlan.Phase != cloudv1alpha1.ImportPhaseBlocked || obj.Spec.ImportFrom == nil {
		t.Errorf("after the reconciler's pass: plan %+v, importFrom %+v; want the plan (blocked: the VPC is empty)",
			obj.Status.ImportPlan, obj.Spec.ImportFrom)
	}
	row, err := store.Networks().Get(ctx, tenant, id)
	if err != nil || row.State != "NETWORK_STATE_FAILED" || row.ExternalID == nil || *row.ExternalID != vpc {
		t.Errorf("row after the pass: state %s, external ID %v (%v)", row.State, row.ExternalID, err)
	}
}
