package cloudstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	corecluster "github.com/yindia/cluster-vending/pkg/cluster"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/server/cluster"
	"github.com/yindia/cluster-vending/pkg/server/network"
)

const tenant = "acme"

func mask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

// The network service, unchanged, on the store: a network is created,
// moved on by the loop, updated and deleted, and throughout its row is a
// Network object in the tenant's namespace with the records of its
// objects in status.inventory.
func TestNetworkServiceRunsOnTheKinds(t *testing.T) {
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
	svc := network.NewNetworkService(reconcile.Deps{Store: store, Kube: k})

	created, err := svc.CreateNetwork(ctx, &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{
		Name: "prod", Labels: map[string]string{"team": "platform"},
		Spec: &cloudv1.NetworkSpec{Region: "us-east-1", BootstrapId: account.ID, Layout: &cloudv1.NetworkLayout{}},
	}})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	id := created.GetNetwork().GetId()
	// The handler stores the desired state only; a pass applies it.
	if err := svc.Reconcile(ctx, tenant, id); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	obj := &cloudv1alpha1.Network{}
	key := client.ObjectKey{Namespace: namespace, Name: "prod"}
	if err := k.Get(ctx, key, obj); err != nil {
		t.Fatalf("the Network object of the created network: %v", err)
	}
	if obj.Labels[cloudstore.LabelID] != id || obj.Spec.Tags["team"] != "platform" || obj.Spec.Layout == nil ||
		len(obj.Spec.Layout.Subnets) == 0 || obj.Spec.AccountRef.Name != "aws-444455556666" || len(obj.Status.Inventory) == 0 {
		t.Fatalf("Network object: labels %v, spec %+v, %d inventory entries", obj.Labels, obj.Spec, len(obj.Status.Inventory))
	}
	if _, err := svc.CreateNetwork(ctx, &cloudv1.CreateNetworkRequest{TenantId: tenant, Network: &cloudv1.Network{
		Name: "prod", Spec: &cloudv1.NetworkSpec{Region: "us-east-1", BootstrapId: account.ID, Layout: &cloudv1.NetworkLayout{}},
	}}); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("a second network of the name = %v, want ALREADY_EXISTS", err)
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
	if err := k.Get(ctx, key, obj); err != nil || obj.Status.Phase == "" || obj.Status.ContractStatus == nil {
		t.Errorf("status after the loop's pass: phase %q (%v)", obj.Status.Phase, err)
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
	if err := k.Get(ctx, key, obj); client.IgnoreNotFound(err) != nil || err == nil {
		t.Errorf("the Network object after the deletion: %v, want it gone", err)
	}
}

// tagSubnetsQuietly lets the fake client take the cluster's subnet tags:
// it round-trips applies through the Go types, which send
// forProvider.region as null and conflict; the API server does not.
func tagSubnetsQuietly(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	ao := &client.ApplyOptions{}
	ao.ApplyOptions(opts)
	if strings.HasPrefix(ao.FieldManager, kube.FieldOwner+"/cluster-") {
		return nil
	}
	return c.Apply(ctx, obj, opts...)
}

// The cluster service, unchanged, on the store: the loop applies a
// cluster row that is a Cluster object, the API reads and updates it, and
// its deletion removes the object.
func TestClusterServiceRunsOnTheKinds(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	k := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&cloudv1alpha1.CloudAccount{}, &cloudv1alpha1.Network{}, &cloudv1alpha1.Cluster{}, &cloudv1alpha1.Component{}, &awseks.Cluster{}).
		WithInterceptorFuncs(interceptor.Funcs{Apply: tagSubnetsQuietly}).Build()
	store := cloudstore.New(k)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(store.Tenants().Create(ctx, &model.Tenant{ID: tenant}))
	account := &model.CloudAccount{TenantID: tenant, Provider: model.ProviderAWS, AccountID: "444455556666",
		ProviderConfigName: "aws-0a1b2c3d", Namespace: namespace}
	must(store.Tenants().PutCloudAccount(ctx, account))
	net := &model.Network{TenantID: tenant, Name: "net", CloudAccountID: account.ID, Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: "NETWORK_STATE_READY"}}
	net.Spec = document(t, networkSpec(net))
	must(store.Networks().Create(ctx, net))
	var records []model.ManagedResource
	for i, zone := range []string{"use1-az1", "use1-az2"} {
		subnet := &awsec2.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "net-private-" + zone,
			Labels:      map[string]string{crossplane.LabelNetwork: net.ID, crossplane.LabelSubnetRole: "private", crossplane.LabelZone: zone},
			Annotations: map[string]string{crossplaneaws.ExternalNameAnnotation: "subnet-" + string(rune('a'+i))}}}
		subnet.Spec.ForProvider.Region = ptr.To("us-east-1")
		must(k.Create(ctx, subnet))
		records = append(records, model.ManagedResource{TenantID: tenant, CloudAccountID: account.ID, OwnerKind: model.OwnerNetwork,
			OwnerID: net.ID, APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "Subnet", Namespace: namespace, Name: subnet.Name})
	}
	must(store.ManagedResources().Put(ctx, records))

	spec, err := corecluster.ResolveSpec(&cloudv1.ClusterSpec{Region: "us-east-1",
		Network:      &cloudv1.ClusterNetwork{NetworkId: net.ID},
		ControlPlane: &cloudv1.ControlPlaneSpec{KubernetesVersion: "1.33", EndpointAccess: &cloudv1.EndpointAccess{PublicAccessCidrs: []string{"203.0.113.0/24"}}},
		DefaultNodePool: &cloudv1.NodePoolSpec{InstanceTypes: []string{"m6i.large"},
			ScalingConfig: &cloudv1.ScalingConfig{MinSize: 2, MaxSize: 4}},
		Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}},
	}, []string{"use1-az1", "use1-az2"})
	must(err)
	row := &model.Cluster{TenantID: tenant, Name: "prod", NetworkID: net.ID, Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: "CLUSTER_STATE_UPDATING", Generation: 2, AppliedGeneration: 1, Spec: document(t, spec)}}
	must(store.Clusters().Create(ctx, row))
	// Its EKS cluster exists and is ACTIVE: an update is being applied.
	prefix := crossplaneaws.ClusterPrefix(tenant, row.ID, row.Name)
	eks := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: prefix, Labels: crossplane.ClusterLabels(tenant, row.ID)}}
	eks.Spec.ForProvider.Region = ptr.To("us-east-1")
	must(k.Create(ctx, eks))
	eks.Status.AtProvider.Status = ptr.To("ACTIVE")
	must(k.Status().Update(ctx, eks))
	// Its ClusterAuth is Ready: the helm ProviderConfig can be applied.
	auth := &awseks.ClusterAuth{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: prefix + "-auth", Labels: crossplane.ClusterLabels(tenant, row.ID)}}
	auth.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	must(k.Create(ctx, auth))
	must(store.ManagedResources().Put(ctx, []model.ManagedResource{{TenantID: tenant, CloudAccountID: account.ID, OwnerKind: model.OwnerCluster,
		OwnerID: row.ID, APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Cluster", Namespace: namespace, Name: prefix}}))

	svc := cluster.NewClusterService(reconcile.Deps{Store: store, Kube: k})
	if err := svc.Reconcile(ctx, tenant, row.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	applied, err := store.Clusters().Get(ctx, tenant, row.ID)
	if err != nil || applied.AppliedGeneration != applied.Generation {
		t.Fatalf("applied generation %d of %d (%v)", applied.AppliedGeneration, applied.Generation, err)
	}
	groups := &awseks.NodeGroupList{}
	if err := k.List(ctx, groups, client.MatchingLabels{crossplane.LabelCluster: row.ID}); err != nil || len(groups.Items) != 1 {
		t.Fatalf("%d node groups after the loop's pass (%v)", len(groups.Items), err)
	}
	obj := &cloudv1alpha1.Cluster{}
	key := client.ObjectKey{Namespace: namespace, Name: "prod"}
	must(k.Get(ctx, key, obj))
	if len(obj.Status.Inventory) < 5 || obj.Status.ContractStatus == nil || obj.Spec.Network.NetworkRef.Name != "net" ||
		obj.Spec.ControlPlane.KubernetesVersion != "1.33" || len(obj.Spec.Network.ZoneIDs) != 2 {
		t.Errorf("Cluster object: %d inventory entries, spec %+v", len(obj.Status.Inventory), obj.Spec)
	}

	got, err := svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: tenant, ClusterId: row.ID})
	if err != nil || got.GetCluster().GetSpec().GetNetwork().GetNetworkId() != net.ID ||
		got.GetCluster().GetSpec().GetDefaultNodePool().GetScalingConfig().GetMaxSize() != 4 {
		t.Fatalf("GetCluster = %v, %v", got.GetCluster().GetSpec(), err)
	}
	updated, err := svc.UpdateCluster(ctx, &cloudv1.UpdateClusterRequest{TenantId: tenant,
		Cluster:    &cloudv1.Cluster{Id: row.ID, Etag: got.GetCluster().GetEtag(), DisplayName: "Production"},
		UpdateMask: mask("display_name")})
	if err != nil || updated.GetCluster().GetDisplayName() != "Production" || updated.GetCluster().GetEtag() == got.GetCluster().GetEtag() {
		t.Fatalf("UpdateCluster = %v, %v", updated.GetCluster(), err)
	}
	must(k.Get(ctx, key, obj))
	if obj.Spec.DisplayName != "Production" {
		t.Errorf("displayName of the Cluster object = %q", obj.Spec.DisplayName)
	}
	list, err := svc.ListClusters(ctx, &cloudv1.ListClustersRequest{TenantId: tenant})
	if err != nil || len(list.GetClusters()) != 1 {
		t.Fatalf("ListClusters = %d (%v)", len(list.GetClusters()), err)
	}

	if _, err := svc.DeleteCluster(ctx, &cloudv1.DeleteClusterRequest{TenantId: tenant, ClusterId: row.ID, Force: true}); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	for range 10 {
		if err := svc.Reconcile(ctx, tenant, row.ID); err != nil {
			t.Fatalf("Reconcile while deleting: %v", err)
		}
	}
	if _, err := svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: tenant, ClusterId: row.ID}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("GetCluster after the deletion = %v, want NOT_FOUND", err)
	}
	if err := k.Get(ctx, key, obj); client.IgnoreNotFound(err) != nil || err == nil {
		t.Errorf("the Cluster object after the deletion: %v, want it gone", err)
	}
	// The network is free again and can go.
	must(store.Networks().Delete(ctx, tenant, net.ID))
}
