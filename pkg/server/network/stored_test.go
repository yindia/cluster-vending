package network

import (
	"context"
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	netreconcile "github.com/yindia/cluster-vending/pkg/reconcile/network"
)

// countReads counts every read of the management cluster.
func countReads(reads *atomic.Int64) interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads.Add(1)
			return c.List(ctx, list, opts...)
		},
	}
}

// allReady reports every managed resource of the network Ready, as the
// providers would.
func (f fixture) allReady(t *testing.T, id string) {
	t.Helper()
	ctx := t.Context()
	for _, list := range crossplaneaws.NetworkLists() {
		if err := f.kube.List(ctx, list, client.MatchingLabels{crossplane.LabelNetwork: id}); err != nil {
			t.Fatal(err)
		}
		items, _ := meta.ExtractList(list)
		for _, item := range items {
			obj := item.(client.Object)
			obj.(interface{ SetConditions(...xpv2.Condition) }).SetConditions(xpv2.Available(), xpv2.ReconcileSuccess())
			// Kinds the fake serves a status subresource for, and the others.
			if err := f.kube.Status().Update(ctx, obj); err != nil {
				if err := f.kube.Update(ctx, obj); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// GetNetwork and ListNetworks answer from the database: the spec and the
// status the loop last recorded. Only GetNetwork with live reads the
// management cluster.
func TestGetNetworkReadsOnlyTheDatabase(t *testing.T) {
	var reads atomic.Int64
	f := newFixture(t, countReads(&reads))
	ctx := t.Context()
	n := f.create(t, "prod", nil) // the loop ran once: its status is recorded
	get := func() *cloudv1.Network {
		t.Helper()
		reads.Store(0)
		res, err := f.svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId()})
		if err != nil {
			t.Fatalf("GetNetwork: %v", err)
		}
		list, err := f.svc.ListNetworks(ctx, &cloudv1.ListNetworksRequest{TenantId: tenant})
		if err != nil || len(list.GetNetworks()) != 1 || list.GetNetworks()[0].GetStatus().GetState() != res.GetNetwork().GetStatus().GetState() {
			t.Fatalf("ListNetworks: %v, %v", list.GetNetworks(), err)
		}
		if got := reads.Load(); got != 0 {
			t.Fatalf("GetNetwork and ListNetworks read the management cluster %d times", got)
		}
		return res.GetNetwork()
	}
	row := func() *model.Network {
		t.Helper()
		r, err := f.store.Networks().Get(ctx, tenant, n.GetId())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	got := get()
	if st := got.GetStatus(); st.GetState() != cloudv1.NetworkState_NETWORK_STATE_PROVISIONING || len(st.GetManagedResources()) != 20 ||
		got.GetSpec().GetLayout().GetCidrBlock() != "10.0.0.0/16" {
		t.Errorf("recorded: %s, %d resources, spec %v", st.GetState(), len(st.GetManagedResources()), got.GetSpec())
	}
	// The providers finish; nothing is recorded until the loop looks.
	f.allReady(t, n.GetId())
	if st := get().GetStatus(); st.GetState() != cloudv1.NetworkState_NETWORK_STATE_PROVISIONING {
		t.Errorf("before the loop's pass GetNetwork says %s", st.GetState())
	}
	live, err := f.svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId(), Live: true})
	if err != nil || live.GetNetwork().GetStatus().GetState() != cloudv1.NetworkState_NETWORK_STATE_READY || reads.Load() == 0 {
		t.Errorf("GetNetwork live = %s (%v) with %d reads, want READY read live", live.GetNetwork().GetStatus().GetState(), err, reads.Load())
	}
	etag := row().ETag
	if err := f.svc.Reconcile(ctx, tenant, n.GetId()); err != nil {
		t.Fatal(err)
	}
	if st := get().GetStatus(); st.GetState() != cloudv1.NetworkState_NETWORK_STATE_READY {
		t.Errorf("after the loop's pass GetNetwork says %s (%s)", st.GetState(), st.GetMessage())
	}
	// READY is stored: the loop leaves the network alone from here on.
	if r := row(); r.State != netreconcile.StateReady {
		t.Errorf("stored state %s, want %s", r.State, netreconcile.StateReady)
	}
	if r := row(); r.AppliedGeneration != r.Generation {
		t.Errorf("applied generation %d of %d: the network is not settled", r.AppliedGeneration, r.Generation)
	}
	// Recording a status alone does not change the etag.
	settled := row().ETag
	f.allReady(t, n.GetId())
	if err := f.svc.Reconcile(ctx, tenant, n.GetId()); err != nil {
		t.Fatal(err)
	}
	if row().ETag != settled || settled == etag {
		t.Errorf("etags: before %s, settled %s, after another pass %s; want one change (READY stored) and none after", etag, settled, row().ETag)
	}

	// A change is stored: GetNetwork shows it at once, before any pass.
	if _, err := f.svc.DeleteNetwork(ctx, &cloudv1.DeleteNetworkRequest{TenantId: tenant, NetworkId: n.GetId()}); err != nil {
		t.Fatal(err)
	}
	if st := get().GetStatus(); st.GetState() != cloudv1.NetworkState_NETWORK_STATE_DELETING {
		t.Errorf("after DeleteNetwork GetNetwork says %s", st.GetState())
	}
}

// The clusters using a network come from the database on every read.
func TestStoredNetworkListsItsClusters(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	n := f.create(t, "prod", nil)
	if err := f.store.Clusters().Create(ctx, &model.Cluster{TenantID: tenant, Name: "one", NetworkID: n.GetId(), Provider: model.ProviderAWS,
		Region: "us-east-1", Resource: model.Resource{State: "CLUSTER_STATE_RUNNING", Spec: model.JSON(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.GetNetwork(ctx, &cloudv1.GetNetworkRequest{TenantId: tenant, NetworkId: n.GetId()})
	if err != nil || len(res.GetNetwork().GetStatus().GetClusterIds()) != 1 || res.GetNetwork().GetStatus().GetClusterIds()[0] != "one" {
		t.Errorf("cluster_ids = %v (%v), want the cluster created since the status was recorded", res.GetNetwork().GetStatus().GetClusterIds(), err)
	}
}
