package controller

import (
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// readyNetwork stores a READY network "net" whose private subnets, in two
// zones, exist in the cloud: what a cluster needs to be created in it.
func (w world) readyNetwork(t *testing.T) *model.Network {
	t.Helper()
	ctx := t.Context()
	spec, err := protojson.Marshal(&cloudv1.NetworkSpec{Region: "us-east-1", BootstrapId: w.account.ID,
		Layout: &cloudv1.NetworkLayout{CidrBlock: "10.0.0.0/16", NatGatewayMode: cloudv1.NatGatewayMode_NAT_GATEWAY_MODE_SINGLE,
			Subnets: []*cloudv1.Subnet{
				{Role: cloudv1.SubnetRole_SUBNET_ROLE_PUBLIC, CidrBlock: "10.0.224.0/22", ZoneId: "use1-az1"},
				{Role: cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE, CidrBlock: "10.0.0.0/19", ZoneId: "use1-az1"},
				{Role: cloudv1.SubnetRole_SUBNET_ROLE_PRIVATE, CidrBlock: "10.0.32.0/19", ZoneId: "use1-az2"},
			}}})
	w.must(t, err)
	net := &model.Network{TenantID: tenant, Name: "net", CloudAccountID: w.account.ID, Provider: model.ProviderAWS, Region: "us-east-1",
		Resource: model.Resource{State: "NETWORK_STATE_READY", Spec: spec}}
	w.must(t, w.store.Networks().Create(ctx, net))
	storetest.SetAccess(t, w.k, tenant, w.account.ID, storetest.Verified("us-east-1"))
	var records []model.ManagedResource
	for i, zone := range []string{"use1-az1", "use1-az2"} {
		subnet := &awsec2.Subnet{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "net-private-" + zone,
			Labels:      map[string]string{crossplane.LabelNetwork: net.ID, crossplane.LabelSubnetRole: "private", crossplane.LabelZone: zone},
			Annotations: map[string]string{crossplaneaws.ExternalNameAnnotation: "subnet-" + string(rune('a'+i))}}}
		subnet.Spec.ForProvider.Region = ptr.To("us-east-1")
		w.must(t, w.k.Create(ctx, subnet))
		records = append(records, model.ManagedResource{TenantID: tenant, CloudAccountID: w.account.ID, OwnerKind: model.OwnerNetwork,
			OwnerID: net.ID, APIVersion: "ec2.aws.m.upbound.io/v1beta1", Kind: "Subnet", Namespace: namespace, Name: subnet.Name})
	}
	w.must(t, w.store.ManagedResources().Put(ctx, records))
	return net
}

// handMadeCluster is a Cluster as someone writes it for kubectl.
func handMadeCluster(t *testing.T, name, network string) *cloudv1alpha1.Cluster {
	t.Helper()
	obj := &cloudv1alpha1.Cluster{}
	err := yaml.UnmarshalStrict([]byte(`
apiVersion: cloud.cluster-vending.io/v1alpha1
kind: Cluster
metadata:
  name: `+name+`
  namespace: `+namespace+`
spec:
  region: us-east-1
  network:
    networkRef:
      name: `+network+`
  controlPlane:
    kubernetesVersion: "1.33"
    endpointAccess:
      publicAccessCIDRs:
        - 203.0.113.0/24
  aws: {}
`), obj)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func (w world) cluster(t *testing.T, name string) *cloudv1alpha1.Cluster {
	t.Helper()
	obj := &cloudv1alpha1.Cluster{}
	w.must(t, w.k.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, obj))
	return obj
}

// A Cluster created with kubectl becomes the row the API would have
// stored (an ID, its defaults resolved, owned by its network), is applied
// by the cluster reconciler, and goes, once deleted, when its resources
// are gone.
func TestClusterCreatedByHandIsAdoptedAppliedAndDeleted(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	net := w.readyNetwork(t)
	w.must(t, w.k.Create(ctx, handMadeCluster(t, "prod", "net")))
	r := NewClusterReconciler(w.deps)
	for range 2 { // adopted, then applied
		if _, err := r.Reconcile(ctx, request("prod")); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	obj := w.cluster(t, "prod")
	id := obj.Labels[cloudstore.LabelID]
	if id == "" {
		t.Fatalf("not adopted: %+v", obj.Status.Conditions)
	}
	if obj.Labels[cloudstore.LabelNetworkID] != net.ID || len(obj.OwnerReferences) != 1 || obj.OwnerReferences[0].Kind != "Network" ||
		len(obj.Finalizers) != 1 || obj.Finalizers[0] != cloudv1alpha1.ClusterFinalizer {
		t.Errorf("labels %v, owners %v, finalizers %v", obj.Labels, obj.OwnerReferences, obj.Finalizers)
	}
	if len(obj.Spec.Network.ZoneIDs) != 2 || obj.Status.Tier != cloudv1alpha1.ClusterTier0 {
		t.Errorf("the spec was not resolved as a create resolves it: zones %v, tier %q", obj.Spec.Network.ZoneIDs, obj.Status.Tier)
	}
	row, err := w.store.Clusters().Get(ctx, tenant, id)
	if err != nil || row.State != reconcilecluster.StateProvisioning || row.NetworkID != net.ID {
		t.Fatalf("row: state %s, network %s (%v)", row.State, row.NetworkID, err)
	}
	if w.count(t, &awseks.ClusterList{}, crossplane.LabelCluster, id) != 1 || len(obj.Status.Inventory) == 0 || obj.Status.Phase == "" {
		t.Fatalf("not applied: %d inventory entries, phase %q", len(obj.Status.Inventory), obj.Status.Phase)
	}
	eks := &awseks.ClusterList{}
	w.must(t, w.k.List(ctx, eks, client.MatchingLabels{crossplane.LabelCluster: id}))
	reqs := ownerRequests(&owners{}, w.k, crossplane.LabelCluster, func() client.ObjectList { return &cloudv1alpha1.ClusterList{} })(ctx, &eks.Items[0])
	if len(reqs) != 1 || reqs[0] != request("prod") {
		t.Errorf("the EKS cluster's owner = %v, want the cluster", reqs)
	}

	w.must(t, w.k.Delete(ctx, obj))
	if _, err := r.Reconcile(ctx, request("prod")); err != nil {
		t.Fatal(err)
	}
	if row, err := w.store.Clusters().Get(ctx, tenant, id); err == nil && row.State != reconcilecluster.StateDeleting {
		t.Fatalf("after the delete: state %s, want DELETING", row.State)
	}
	if got := &(cloudv1alpha1.Cluster{}); w.k.Get(ctx, request("prod").NamespacedName, got) == nil && len(got.Finalizers) == 0 {
		t.Error("the finalizer went before the resources")
	}
	for range 10 {
		if _, err := r.Reconcile(ctx, request("prod")); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.count(t, &awseks.ClusterList{}, crossplane.LabelCluster, id); n != 0 {
		t.Errorf("%d EKS cluster objects after the deletion", n)
	}
	err = w.k.Get(ctx, request("prod").NamespacedName, &cloudv1alpha1.Cluster{})
	if err == nil || client.IgnoreNotFound(err) != nil {
		t.Errorf("the Cluster object after its resources are gone: %v, want it gone", err)
	}
}

// A Cluster whose network does not exist, or that imports an existing
// cluster, is not adopted and says why.
func TestClusterThatCannotBeAdoptedSaysWhy(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	w.readyNetwork(t)
	importing := handMadeCluster(t, "imported", "net")
	importing.Spec.ImportFrom = &cloudv1alpha1.ClusterImport{}
	for name, tt := range map[string]struct {
		obj    *cloudv1alpha1.Cluster
		reason string
	}{
		"no such network": {handMadeCluster(t, "orphan", "nowhere"), reasonWaiting},
		"an import":       {importing, reasonNotThroughAPI},
	} {
		w.must(t, w.k.Create(ctx, tt.obj))
		res, err := NewClusterReconciler(w.deps).Reconcile(ctx, request(tt.obj.Name))
		if err != nil || res.RequeueAfter != failedDelay {
			t.Errorf("%s: %+v, %v", name, res, err)
		}
		got := w.cluster(t, tt.obj.Name)
		cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAdopted)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tt.reason || got.Labels[cloudstore.LabelID] != "" {
			t.Errorf("%s: condition %+v, labels %v", name, cond, got.Labels)
		}
	}
}
