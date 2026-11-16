package cluster

import (
	"context"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	platform "github.com/yindia/cluster-vending/pkg/reconcile"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

// Reads use recorded resource status and a live platform freeze lookup.
func TestGetClusterReadsStoredResourcesAndLiveFreeze(t *testing.T) {
	var reads atomic.Int64
	w := newWorld(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, freeze := obj.(*cloudv1alpha1.ReleaseHold); !freeze {
				reads.Add(1)
			}
			return c.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			reads.Add(1)
			return c.List(ctx, list, opts...)
		},
	}, nil)
	ctx := t.Context()
	get := func() *cloudv1.Cluster {
		t.Helper()
		reads.Store(0)
		res, err := w.svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID})
		if err != nil {
			t.Fatalf("GetCluster: %v", err)
		}
		list, err := w.svc.ListClusters(ctx, &cloudv1.ListClustersRequest{TenantId: worldTenant})
		if err != nil || len(list.GetClusters()) != 1 || list.GetClusters()[0].GetStatus().GetState() != res.GetCluster().GetStatus().GetState() {
			t.Fatalf("ListClusters: %v, %v", list.GetClusters(), err)
		}
		if got := reads.Load(); got != 0 {
			t.Fatalf("GetCluster and ListClusters read the management cluster %d times", got)
		}
		return res.GetCluster()
	}

	// Nothing recorded yet: the row's own state, no resources.
	if st := get().GetStatus(); st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_UPDATING || len(st.GetManagedResources()) != 0 {
		t.Errorf("before any pass: %s, %d resources", st.GetState(), len(st.GetManagedResources()))
	}
	// The loop's pass applies the cluster and records what it saw.
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	etag := w.cluster(t).ETag
	recorded := get()
	if st := recorded.GetStatus(); len(st.GetManagedResources()) < 20 || st.GetState() == cloudv1.ClusterState_CLUSTER_STATE_UNSPECIFIED {
		t.Errorf("after the pass: %s, %d resources recorded", st.GetState(), len(st.GetManagedResources()))
	}
	live, err := w.svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID, Live: true})
	if err != nil || reads.Load() == 0 || len(live.GetCluster().GetStatus().GetManagedResources()) != len(recorded.GetStatus().GetManagedResources()) {
		t.Errorf("GetCluster live: %v, %d reads, %d resources", err, reads.Load(), len(live.GetCluster().GetStatus().GetManagedResources()))
	}
	// Another pass with nothing new records nothing: the etag stays.
	if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
		t.Fatal(err)
	}
	if after := w.cluster(t).ETag; after != etag {
		t.Errorf("recording the status changed the etag: %s -> %s", etag, after)
	}

	// A delete is stored: GetCluster shows it at once.
	if _, err := w.svc.DeleteCluster(ctx, &cloudv1.DeleteClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID}); err != nil {
		t.Fatal(err)
	}
	if st := get().GetStatus(); st.GetState() != cloudv1.ClusterState_CLUSTER_STATE_DELETING {
		t.Errorf("after DeleteCluster GetCluster says %s", st.GetState())
	}
}

func TestStoredState(t *testing.T) {
	const (
		unspecified = cloudv1.ClusterState_CLUSTER_STATE_UNSPECIFIED
		running     = cloudv1.ClusterState_CLUSTER_STATE_RUNNING
		updating    = cloudv1.ClusterState_CLUSTER_STATE_UPDATING
		failed      = cloudv1.ClusterState_CLUSTER_STATE_FAILED
		deleting    = cloudv1.ClusterState_CLUSTER_STATE_DELETING
		imported    = cloudv1.ClusterState_CLUSTER_STATE_IMPORTED
	)
	for _, tt := range []struct {
		name     string
		row      string
		recorded cloudv1.ClusterState
		want     cloudv1.ClusterState
	}{
		{"nothing recorded", reconcilecluster.StateProvisioning, unspecified, cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING},
		{"converged", reconcilecluster.StateRunning, running, running},
		{"a change stored since", reconcilecluster.StateUpdating, running, updating},
		{"observed failure", reconcilecluster.StateUpdating, failed, failed},
		{"delete stored since", reconcilecluster.StateDeleting, running, deleting},
		{"taken over since", reconcilecluster.StateUpdating, imported, updating},
		{"still imported", reconcilecluster.StateImported, imported, imported},
	} {
		if got := storedState(tt.row, tt.recorded); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
	// An unreadable recorded status is no reason to fail a read.
	c, err := stored(&model.Cluster{Resource: model.Resource{State: reconcilecluster.StateRunning, Spec: model.JSON(`{}`), Status: model.JSON(`not json`)}})
	if err != nil || c.GetStatus().GetState() != running {
		t.Errorf("unreadable status: %v, %v", c.GetStatus(), err)
	}
}

func TestStoredClusterShowsFreezeBeforeReconcile(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	hold := &cloudv1alpha1.ReleaseHold{ObjectMeta: metav1.ObjectMeta{Name: "platform-freeze"}, Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "maintenance", By: "ops"}}
	if err := w.kube.Create(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	request := &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID}
	result, err := w.svc.GetCluster(t.Context(), request)
	if err != nil || result.GetCluster().GetStatus().GetFreeze().GetReason() != "maintenance" {
		t.Fatalf("frozen: %v, %v", result, err)
	}
	if err := w.kube.Delete(t.Context(), hold); err != nil {
		t.Fatal(err)
	}
	result, err = w.svc.GetCluster(t.Context(), request)
	if err != nil || result.GetCluster().GetStatus().GetFreeze() != nil {
		t.Fatalf("unfrozen: %v, %v", result, err)
	}
}
func TestStoredClusterClearsRecordedFreezeMessage(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	row := w.cluster(t)
	frozen := &model.Freeze{By: "ops", Reason: "maintenance"}
	_, note := platform.RowFreeze(frozen, nil)
	status := &cloudv1.ClusterStatus{State: cloudv1.ClusterState(cloudv1.ClusterState_value[row.State]), Message: "waiting" + note, Freeze: platform.FreezeProto(frozen),
		ManagedResources: []*cloudv1.ManagedResource{{Paused: true}},
		Conditions:       []*cloudv1.Condition{{Type: platform.ConditionFrozen}}}
	encoded, err := reconcilecluster.Encode(status)
	if err != nil {
		t.Fatal(err)
	}
	row.Status = encoded
	if err := w.store.Clusters().Update(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	result, err := w.svc.GetCluster(t.Context(), &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: row.ID})
	if err != nil {
		t.Fatal(err)
	}
	current := result.GetCluster().GetStatus()
	if current.GetFreeze() != nil || current.GetMessage() != "waiting" {
		t.Fatalf("stale freeze: %v", current)
	}
	for _, condition := range current.GetConditions() {
		if condition.GetType() == platform.ConditionFrozen {
			t.Fatal("stale Frozen condition")
		}
	}
}
func TestSharedNetworkTierIsReadLiveByBothClusters(t *testing.T) {
	world := newWorld(t, interceptor.Funcs{}, nil)
	other := &model.Cluster{TenantID: worldTenant, Name: "other", NetworkID: world.row.NetworkID, Provider: world.row.Provider, Region: world.row.Region,
		Resource: model.Resource{State: world.row.State, Spec: world.row.Spec}}
	if err := world.store.Clusters().Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	network, err := world.store.Networks().Get(t.Context(), worldTenant, world.row.NetworkID)
	if err != nil {
		t.Fatal(err)
	}
	spec := &cloudv1.NetworkSpec{}
	if err := protojson.Unmarshal(network.Spec, spec); err != nil {
		t.Fatal(err)
	}
	spec.Tier = cloudv1.Tier_TIER_2
	network.Spec, err = protojson.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := world.store.Networks().Update(t.Context(), network); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{world.row.ID, other.ID} {
		result, err := world.svc.GetCluster(t.Context(), &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: id})
		if err != nil || result.GetCluster().GetStatus().GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_2 {
			t.Fatalf("inherited tier: %v, %v", result, err)
		}
		//nolint:staticcheck // The inherited tier must never be returned in the legacy spec field.
		if result.GetCluster().GetSpec().GetTier() != cloudv1.ClusterTier_CLUSTER_TIER_UNSPECIFIED {
			t.Fatal("cluster retained an independent tier")
		}
	}
}
