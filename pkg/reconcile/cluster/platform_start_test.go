package cluster

import (
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestServesCharts(t *testing.T) {
	const prefix = "prod-1234"
	ready := cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
	pending := cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING
	view := func(kind, name string, state cloudv1.ManagedResourceState) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: kind, Name: name, State: state}
	}
	eks, nodes := view("Cluster", prefix, ready), view("NodeGroup", prefix+"-default", ready)
	auth, addon := view("ClusterAuth", prefix+"-auth", ready), view("Addon", prefix+"-addon-metrics-server", pending)
	tests := []struct {
		name  string
		views []*cloudv1.ManagedResource
		want  bool
	}{
		{name: "all three ready, an add-on not", views: []*cloudv1.ManagedResource{eks, nodes, auth, addon}, want: true},
		{name: "no node group", views: []*cloudv1.ManagedResource{eks, auth}},
		{name: "node group not ready", views: []*cloudv1.ManagedResource{eks, view("NodeGroup", prefix+"-default", pending), auth}},
		{name: "EKS not ready", views: []*cloudv1.ManagedResource{view("Cluster", prefix, pending), nodes, auth}},
		{name: "no kubeconfig", views: []*cloudv1.ManagedResource{eks, nodes, view("ClusterAuth", prefix+"-auth", pending)}},
		{name: "only the live reader's kubeconfig", views: []*cloudv1.ManagedResource{eks, nodes,
			view("ClusterAuth", crossplaneaws.LiveClusterAuthName(prefix), ready)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := servesCharts(prefix, tt.views); got != tt.want {
				t.Errorf("servesCharts = %v, want %v", got, tt.want)
			}
		})
	}
}

// A cluster still being created gets its platform components (Karpenter
// among them) once its EKS cluster, node group and kubeconfig are Ready,
// although an add-on waits for nodes only Karpenter brings: waiting for
// RUNNING would wait forever.
func TestPlatformStartsBeforeRunningWhenAnAddonWaitsForNodes(t *testing.T) {
	w, _ := platformWorld(t)
	w.helmConfig(t)
	row := w.cluster(t)
	row.State = StateProvisioning
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}

	// Only the EKS cluster and the kubeconfig: no node group yet.
	w.markEKSReady(t)
	w.recordAuth(t)
	if p := w.install(t); p != nil || w.release(t, "base") != nil {
		t.Fatal("installed before the node group")
	}

	// The node group is Ready; metrics-server's add-on is not.
	w.recordObject(t, &awseks.NodeGroup{}, "NodeGroup", w.prefix()+"-default", true)
	w.recordObject(t, &awseks.Addon{}, "Addon", w.prefix()+"-addon-metrics-server", false)
	p := w.install(t)
	if p == nil {
		t.Fatal("nothing installed before the cluster is RUNNING")
	}
	if got := phases(p); got != "base=INSTALLING,mesh=PENDING" || w.release(t, "base") == nil {
		t.Fatalf("got %s, want base installing before the cluster is RUNNING", got)
	}
	if got := w.cluster(t).State; got != StateProvisioning {
		t.Errorf("state %s, want it still provisioning", got)
	}
}

// markEKSReady makes the world's EKS cluster Ready.
func (w world) markEKSReady(t *testing.T) {
	t.Helper()
	c := &awseks.Cluster{}
	if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: w.prefix()}, c); err != nil {
		t.Fatal(err)
	}
	c.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	if err := w.kube.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
}

// recordAuth records the world's own ClusterAuth, which is Ready already.
func (w world) recordAuth(t *testing.T) {
	t.Helper()
	w.put(t, "ClusterAuth", w.prefix()+"-auth")
}

// recordObject creates obj under name, Ready or not, and records it as the
// cluster's.
func (w world) recordObject(t *testing.T, obj interface {
	client.Object
	SetConditions(...xpv2.Condition)
}, kind, name string, ready bool) {
	t.Helper()
	obj.SetNamespace(worldNS)
	obj.SetName(name)
	obj.SetLabels(crossplane.ClusterLabels(worldTenant, w.row.ID))
	if ready {
		obj.SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
	} else {
		obj.SetConditions(xpv2.Creating(), xpv2.ReconcileSuccess())
	}
	if err := w.kube.Create(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	w.put(t, kind, name)
}

func (w world) put(t *testing.T, kind, name string) {
	t.Helper()
	row := w.cluster(t)
	if err := w.store.ManagedResources().Put(t.Context(), []model.ManagedResource{{TenantID: worldTenant,
		CloudAccountID: row.CloudAccountID, OwnerKind: model.OwnerCluster, OwnerID: row.ID,
		APIVersion: "eks.aws.m.upbound.io/v1beta1", Kind: kind, Namespace: worldNS, Name: name}}); err != nil {
		t.Fatal(err)
	}
}
