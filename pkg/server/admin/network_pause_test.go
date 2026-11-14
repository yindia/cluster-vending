package admin

import (
	"testing"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// One network is paused on its own: its row and its own managed
// resources, not the tenant's other network nor any cluster. Paused also
// with the tenant, the tenant's resume leaves it and its resources paused;
// its own resume removes exactly what its pause added.
func TestPauseOfOneNetwork(t *testing.T) {
	ctx := t.Context()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	store := storetest.NewStore(t)
	storetest.MustTenant(t, store, "acme")
	net := storetest.MustNetwork(t, store, "acme", "net")
	other := storetest.MustNetwork(t, store, "acme", "other")
	prod := storetest.MustCluster(t, store, "acme", "prod", net)
	vpc := func(name, networkID string) *awsec2.VPC {
		o := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: name, Labels: crossplane.NetworkLabels("acme", networkID)}}
		o.Spec.ForProvider.Region = ptr.To("us-east-1")
		return o
	}
	nodes := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "prod-default",
		Labels: crossplane.ClusterLabels("acme", prod.ID)}}
	nodes.Spec.ForProvider.Region = ptr.To("us-east-1")
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vpc("net-vpc", net.ID), vpc("other-vpc", other.ID), nodes).Build()
	s := NewService(reconcile.Deps{Kube: k, Store: store})
	paused := func(name string, obj client.Object) bool {
		t.Helper()
		obj.SetNamespace("tenant-acme")
		obj.SetName(name)
		if err := k.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		return obj.GetAnnotations()[PausedAnnotation] == "true"
	}
	rowPaused := func(id string) bool {
		t.Helper()
		n, err := store.Networks().Get(ctx, "acme", id)
		if err != nil {
			t.Fatal(err)
		}
		return n.Paused != nil
	}
	state := func() [4]bool {
		return [4]bool{paused("net-vpc", &awsec2.VPC{}), paused("other-vpc", &awsec2.VPC{}),
			paused("prod-default", &awseks.NodeGroup{}), rowPaused(net.ID)}
	}
	step := func(name string, call func() (int32, error), resources int32, want [4]bool) {
		t.Helper()
		n, err := call()
		if err != nil || n != resources {
			t.Fatalf("%s: %v, %d resources; want %d", name, err, n, resources)
		}
		if got := state(); got != want {
			t.Fatalf("%s: net vpc, other vpc, prod nodes, net row paused = %v, want %v", name, got, want)
		}
	}
	pause := func(req *cloudv1.PauseReconcileRequest) func() (int32, error) {
		return func() (int32, error) { r, err := s.PauseReconcile(ctx, req); return r.GetResources(), err }
	}
	resume := func(req *cloudv1.ResumeReconcileRequest) func() (int32, error) {
		return func() (int32, error) { r, err := s.ResumeReconcile(ctx, req); return r.GetResources(), err }
	}

	step("pause the network", pause(&cloudv1.PauseReconcileRequest{TenantId: "acme", NetworkId: net.ID}), 1,
		[4]bool{true, false, false, true})
	step("pause the tenant", pause(&cloudv1.PauseReconcileRequest{TenantId: "acme"}), 3, [4]bool{true, true, true, true})
	step("resume the tenant: the network stays paused", resume(&cloudv1.ResumeReconcileRequest{TenantId: "acme"}), 2,
		[4]bool{true, false, false, true})
	if c, err := store.Clusters().Get(ctx, "acme", prod.ID); err != nil || c.Paused != nil {
		t.Fatalf("the network's cluster: paused %+v, %v", c.Paused, err)
	}
	step("resume the network", resume(&cloudv1.ResumeReconcileRequest{TenantId: "acme", NetworkId: net.ID}), 1,
		[4]bool{false, false, false, false})

	n, err := store.Networks().Get(ctx, "acme", net.ID)
	if err != nil || n.Paused != nil {
		t.Fatalf("after both resumes: %+v, %v", n.Paused, err)
	}
	if _, err := s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "acme", NetworkId: "0190a000-0000-7000-8000-0000000000ff"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("no such network: %v, want NOT_FOUND", err)
	}
	both := &cloudv1.PauseReconcileRequest{TenantId: "acme", NetworkId: net.ID, ClusterId: prod.ID}
	if _, err := s.PauseReconcile(ctx, both); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a network and a cluster: %v, want INVALID_ARGUMENT", err)
	}
	_ = model.PauseNetwork
}
