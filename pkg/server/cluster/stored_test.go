package cluster

import (
	"context"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// GetCluster and ListClusters answer from the database: the spec and the
// status the loop last recorded. Only GetClusterStatus reads the
// management cluster.
func TestGetClusterReadsOnlyTheDatabase(t *testing.T) {
	var reads atomic.Int64
	w := newWorld(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
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
	live, err := w.svc.GetClusterStatus(ctx, &cloudv1.GetClusterStatusRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil || reads.Load() == 0 || len(live.GetCluster().GetStatus().GetManagedResources()) != len(recorded.GetStatus().GetManagedResources()) {
		t.Errorf("GetClusterStatus: %v, %d reads, %d resources", err, reads.Load(), len(live.GetCluster().GetStatus().GetManagedResources()))
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
		{"nothing recorded", stateProvisioning, unspecified, cloudv1.ClusterState_CLUSTER_STATE_PROVISIONING},
		{"converged", stateRunning, running, running},
		{"a change stored since", stateUpdating, running, updating},
		{"observed failure", stateUpdating, failed, failed},
		{"delete stored since", stateDeleting, running, deleting},
		{"taken over since", stateUpdating, imported, updating},
		{"still imported", stateImported, imported, imported},
	} {
		if got := storedState(tt.row, tt.recorded); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
	// An unreadable recorded status is no reason to fail a read.
	c, err := stored(&model.Cluster{Resource: model.Resource{State: stateRunning, Spec: model.JSON(`{}`), Status: model.JSON(`not json`)}})
	if err != nil || c.GetStatus().GetState() != running {
		t.Errorf("unreadable status: %v, %v", c.GetStatus(), err)
	}
}
