package admin

import (
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository"
	"github.com/yindia/cluster-vending/pkg/database/storetest"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

const (
	clusterA = "0190a000-0000-7000-8000-00000000000a"
	clusterB = "0190a000-0000-7000-8000-00000000000b"
)

// pauseStore is a store with tenant acme, its account, a network and the
// clusters clusterA and clusterB.
func pauseStore(t *testing.T) repository.Store {
	t.Helper()
	store := storetest.NewStore(t)
	storetest.MustTenant(t, store, "acme")
	network := storetest.MustNetwork(t, store, "acme", "net")
	for name, id := range map[string]string{"a": clusterA, "b": clusterB} {
		c := &model.Cluster{TenantID: "acme", Name: name, NetworkID: network.ID, Provider: "aws", Region: "us-east-1",
			Resource: model.Resource{Base: model.Base{ID: id}, State: "CLUSTER_STATE_RUNNING", Spec: storetest.Spec("cluster")}}
		if err := store.Clusters().Create(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// The pause is recorded on the rows with who asked, when and why: the
// controller honours it, and the status shows it. A resume removes it.
func TestPauseIsRecordedWithWhoAndWhy(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		caller  *auth.Identity
		cluster string
		want    string
		tenant  bool // the account and the other cluster are paused too
	}{
		{"a named caller pauses the tenant", &auth.Identity{Subject: "s-1", Username: "alice"}, "", "alice", true},
		{"a caller without a name is its subject", &auth.Identity{Subject: "svc-deploy"}, "", "svc-deploy", true},
		{"without authentication", nil, "", "anonymous", true},
		{"one cluster", &auth.Identity{Username: "alice"}, clusterA, "alice", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.caller != nil {
				ctx = auth.WithIdentity(ctx, tt.caller)
			}
			store := pauseStore(t)
			s := NewService(reconcile.Deps{Kube: fake.NewClientBuilder().WithScheme(scheme).Build(), Store: store})
			s.now = func() time.Time { return at }

			_, err := s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "acme", ClusterId: tt.cluster, Reason: "provider upgrade"})

			if err != nil {
				t.Fatal(err)
			}
			a, err := store.Clusters().Get(ctx, "acme", clusterA)
			if err != nil || a.Paused == nil || a.Paused.By != tt.want || !a.Paused.Since.Equal(at) || a.Paused.Reason != "provider upgrade" {
				t.Fatalf("cluster a paused %+v, %v", a.Paused, err)
			}
			b, err := store.Clusters().Get(ctx, "acme", clusterB)
			if err != nil || (b.Paused != nil) != tt.tenant {
				t.Fatalf("cluster b paused %+v, %v; want paused %v", b.Paused, err, tt.tenant)
			}
			accounts, err := store.Tenants().ListCloudAccounts(ctx, "acme")
			if err != nil || len(accounts) != 1 || (accounts[0].Paused != nil) != tt.tenant {
				t.Fatalf("accounts %+v, %v; want paused %v", accounts, err, tt.tenant)
			}

			if _, err := s.ResumeReconcile(ctx, &cloudv1.ResumeReconcileRequest{TenantId: "acme", ClusterId: tt.cluster}); err != nil {
				t.Fatal(err)
			}
			if a, err = store.Clusters().Get(ctx, "acme", clusterA); err != nil || a.Paused != nil {
				t.Fatalf("after the resume: %+v, %v", a.Paused, err)
			}
		})
	}
}

// A tenant or a cluster that does not exist is NOT_FOUND, and nothing is
// paused for it.
func TestPauseOfWhatDoesNotExist(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(reconcile.Deps{Kube: fake.NewClientBuilder().WithScheme(scheme).Build(), Store: pauseStore(t)})
	for name, req := range map[string]*cloudv1.PauseReconcileRequest{
		"no such tenant":  {TenantId: "nobody"},
		"no such cluster": {TenantId: "acme", ClusterId: "0190a000-0000-7000-8000-0000000000ff"},
	} {
		if _, err := s.PauseReconcile(t.Context(), req); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("%s: %v, want NOT_FOUND", name, err)
		}
	}
}

func TestPauseAndResumeReconcile(t *testing.T) {
	ctx := t.Context()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	ng := func(ns, name string, labels map[string]string) *awseks.NodeGroup {
		o := &awseks.NodeGroup{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
		o.Spec.ForProvider.Region = ptr.To("us-east-1")
		return o
	}
	vpc := &awsec2.VPC{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-acme", Name: "net-vpc", Labels: crossplane.NetworkLabels("acme", "n-1")}}
	vpc.Spec.ForProvider.Region = ptr.To("us-east-1")
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		ng("tenant-acme", "a-default", crossplane.ClusterLabels("acme", clusterA)),
		ng("tenant-acme", "b-default", crossplane.ClusterLabels("acme", clusterB)),
		ng("tenant-beta", "c-default", crossplane.ClusterLabels("beta", clusterA)), vpc).Build()
	s := NewService(reconcile.Deps{Kube: k, Store: pauseStore(t)})
	paused := func(obj client.Object) bool {
		t.Helper()
		if err := k.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		return obj.GetAnnotations()[PausedAnnotation] == "true"
	}
	a, b := ng("tenant-acme", "a-default", nil), ng("tenant-acme", "b-default", nil)
	other := ng("tenant-beta", "c-default", nil)

	// One cluster.
	res, err := s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "acme", ClusterId: clusterA})
	if err != nil || res.GetResources() != 1 || !paused(a) || paused(b) || paused(other) || paused(vpc) {
		t.Fatalf("pause one cluster: %v, %d resources; a %v b %v other tenant %v vpc %v", err, res.GetResources(),
			paused(a), paused(b), paused(other), paused(vpc))
	}
	// The whole tenant, networks included; again is a no-op with the same count.
	for range 2 {
		res, err = s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "acme"})
		if err != nil || res.GetResources() != 3 || !paused(a) || !paused(b) || !paused(vpc) || paused(other) {
			t.Fatalf("pause the tenant: %v, %d resources", err, res.GetResources())
		}
	}
	resumed, err := s.ResumeReconcile(ctx, &cloudv1.ResumeReconcileRequest{TenantId: "acme", ClusterId: clusterB})
	if err != nil || resumed.GetResources() != 1 || paused(b) || !paused(a) {
		t.Fatalf("resume one cluster: %v, %d resources; a %v b %v", err, resumed.GetResources(), paused(a), paused(b))
	}
	if _, err := s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "Not_Valid"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad tenant: %v, want INVALID_ARGUMENT", err)
	}
}

// A pause covers the cluster's Helm Releases too: provider-helm is a
// Crossplane provider like the others, and during its upgrade it must not
// install, upgrade or uninstall a chart either.
func TestPauseCoversHelmReleases(t *testing.T) {
	ctx := t.Context()
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	release := func(name, cluster string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
		u.SetNamespace("tenant-acme")
		u.SetName(name)
		u.SetLabels(crossplane.ComponentLabels("acme", cluster, "kyverno"))
		return u
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(release("a-kyverno", clusterA), release("b-kyverno", clusterB)).Build()
	s := NewService(reconcile.Deps{Kube: k, Store: pauseStore(t)})
	paused := func(name string) bool {
		t.Helper()
		u := release(name, "")
		if err := k.Get(ctx, client.ObjectKeyFromObject(u), u); err != nil {
			t.Fatal(err)
		}
		return u.GetAnnotations()[PausedAnnotation] == "true"
	}

	res, err := s.PauseReconcile(ctx, &cloudv1.PauseReconcileRequest{TenantId: "acme", ClusterId: clusterA})

	if err != nil || res.GetResources() != 1 || !paused("a-kyverno") || paused("b-kyverno") {
		t.Fatalf("pause: %v, %d resources; a %v b %v", err, res.GetResources(), paused("a-kyverno"), paused("b-kyverno"))
	}
	resumed, err := s.ResumeReconcile(ctx, &cloudv1.ResumeReconcileRequest{TenantId: "acme"})
	if err != nil || resumed.GetResources() != 2 || paused("a-kyverno") {
		t.Fatalf("resume: %v, %d resources; a %v", err, resumed.GetResources(), paused("a-kyverno"))
	}
}
