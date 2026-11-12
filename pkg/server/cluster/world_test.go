package cluster

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	"github.com/yindia/cluster-vending/pkg/kube"
	recon "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// world is a cluster in a store (storetest.NewStore) on a network whose subnets
// exist in a fake management cluster, its EKS cluster ACTIVE.
type world struct {
	svc   *ClusterService
	store repository.Store
	kube  client.Client
	row   *model.Cluster
}

const worldTenant, worldNS = "acme", "tenant-acme"

func newWorld(t *testing.T, funcs interceptor.Funcs, reader client.Reader, opts ...func(*recon.Deps)) world {
	t.Helper()
	ctx := t.Context()
	store := storetest.NewStore(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(store.Tenants().Create(ctx, &model.Tenant{ID: worldTenant}))
	account := &model.CloudAccount{TenantID: worldTenant, Provider: model.ProviderAWS, AccountID: "444455556666",
		ProviderConfigName: "aws-0a1b2c3d", Namespace: worldNS, Identity: "arn:aws:iam::444455556666:role/provisioner"}
	must(store.Tenants().PutCloudAccount(ctx, account))
	account, err := store.Tenants().GetCloudAccount(ctx, worldTenant, model.ProviderAWS, "444455556666")
	must(err)
	network := &model.Network{TenantID: worldTenant, Name: "net", CloudAccountID: account.ID, Provider: model.ProviderAWS,
		Region: "us-east-1", Resource: model.Resource{State: "NETWORK_STATE_PROVISIONING", Spec: model.JSON(`{}`)}}
	must(store.Networks().Create(ctx, network))

	scheme, err := kube.Scheme()
	must(err)
	var objects []client.Object
	var records []model.ManagedResource
	for i, zone := range []string{"use1-az1", "use1-az2"} {
		name := "net-private-" + zone
		subnet := &awsec2.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: name,
			Labels:      map[string]string{crossplane.LabelNetwork: network.ID, crossplane.LabelSubnetRole: "private", crossplane.LabelZone: zone},
			Annotations: map[string]string{crossplaneaws.ExternalNameAnnotation: "subnet-" + string(rune('a'+i))}}}
		subnet.Spec.ForProvider.Region = ptr.To("us-east-1")
		objects = append(objects, subnet)
		records = append(records, model.ManagedResource{TenantID: worldTenant, CloudAccountID: account.ID,
			OwnerKind: model.OwnerNetwork, OwnerID: network.ID, APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "Subnet",
			Namespace: worldNS, Name: name})
	}
	must(store.ManagedResources().Put(ctx, records))

	spec := minimalSpec()
	spec.Network.NetworkId = network.ID
	raw, err := reconcilecluster.Encode(mustResolve(t, spec, []string{"use1-az1", "use1-az2"}))
	must(err)
	row := &model.Cluster{TenantID: worldTenant, Name: "prod", NetworkID: network.ID, Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: reconcilecluster.StateUpdating, Generation: 2, AppliedGeneration: 1, Spec: raw}}
	must(store.Clusters().Create(ctx, row))

	prefix := crossplaneaws.ClusterPrefix(worldTenant, row.ID, row.Name)
	eks := &awseks.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: worldNS, Name: prefix,
		Labels: crossplane.ClusterLabels(worldTenant, row.ID)}}
	eks.Spec.ForProvider.Region = ptr.To("us-east-1")
	eks.Status.AtProvider.Status = ptr.To("ACTIVE")
	objects = append(objects, eks)
	must(store.ManagedResources().Put(ctx, []model.ManagedResource{{TenantID: worldTenant, CloudAccountID: account.ID,
		OwnerKind: model.OwnerCluster, OwnerID: row.ID, APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: "Cluster",
		Namespace: worldNS, Name: prefix}}))

	if funcs.Apply == nil {
		funcs.Apply = dryRunApply(scheme)
	}
	funcs.Apply = withoutSubnetTags(funcs.Apply)
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(funcs).Build()
	deps := recon.Deps{Store: store, Kube: k, KubeReader: reader}
	for _, opt := range opts {
		opt(&deps)
	}
	return world{svc: NewClusterService(deps), store: store, kube: k, row: row}
}

func (w world) cluster(t *testing.T) *model.Cluster {
	t.Helper()
	row, err := w.store.Clusters().Get(t.Context(), worldTenant, w.row.ID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// withoutSubnetTags leaves out the applies of the cluster's tags on the
// network's subnets (TagSubnets, which every apply of a cluster starts
// with): the fake round-trips applies through the Go types, which send
// forProvider.region as null and conflict; the API server does not (see
// subnets_test.go).
func withoutSubnetTags(apply func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error) func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		ao := &client.ApplyOptions{}
		ao.ApplyOptions(opts)
		if strings.HasPrefix(ao.FieldManager, kube.FieldOwner+"/cluster-") {
			return nil
		}
		return apply(ctx, c, obj, opts...)
	}
}

// dryRunApply makes the fake client honour dry-run applies like the API
// server (the fake persists them): the apply runs against a scratch copy
// of the live object, so the result is the merged object and nothing is
// stored.
func dryRunApply(scheme *runtime.Scheme) func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		ao := &client.ApplyOptions{}
		ao.ApplyOptions(opts)
		if strings.HasPrefix(ao.FieldManager, kube.FieldOwner+"/cluster-") {
			// The cluster's tags on the network's subnets (tagSubnets): the
			// fake round-trips applies through the Go types, which send
			// forProvider.region as null and conflict; the API server does
			// not (see subnets_test.go).
			return nil
		}
		if !slices.Contains(ao.DryRun, metav1.DryRunAll) {
			return c.Apply(ctx, obj, opts...)
		}
		body, ok := obj.(client.Object)
		if !ok {
			return errors.New("dry-run apply of a typed apply configuration")
		}
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(body.GetObjectKind().GroupVersionKind())
		if err := c.Get(ctx, client.ObjectKeyFromObject(body), live); err != nil {
			return client.IgnoreNotFound(err)
		}
		live.SetResourceVersion("")
		scratch := fake.NewClientBuilder().WithScheme(scheme).WithObjects(live).Build()
		return scratch.Apply(ctx, obj, client.FieldOwner(ao.FieldManager), client.ForceOwnership)
	}
}
