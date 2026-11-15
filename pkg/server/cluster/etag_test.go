package cluster

import (
	"testing"

	"connectrpc.com/connect/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The etag a user gets from GetCluster stays good while the controller
// works on the cluster (applies it, records it RUNNING): only a change of
// what was asked for makes it stale.
func TestTheUsersETagSurvivesTheControllersProgress(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	got, err := w.svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID})
	if err != nil {
		t.Fatal(err)
	}
	etag := got.GetCluster().GetEtag()

	// The controller's passes: resolve, apply, record what they saw.
	for range 2 {
		if err := w.svc.Reconcile(ctx, worldTenant, w.row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if after := w.cluster(t); after.ETag != etag || after.AppliedGeneration != after.Generation {
		t.Fatalf("after the passes: etag %s (was %s), applied %d of %d", after.ETag, etag, after.AppliedGeneration, after.Generation)
	}

	res, err := w.svc.UpdateCluster(ctx, &cloudv1.UpdateClusterRequest{TenantId: worldTenant,
		Cluster:    &cloudv1.Cluster{Id: w.row.ID, Etag: etag, DisplayName: "Prod"},
		UpdateMask: mask("display_name")})
	if err != nil {
		t.Fatalf("UpdateCluster with the etag of the user's GetCluster: %v", err)
	}
	if res.GetCluster().GetEtag() == etag {
		t.Error("the user's change kept the etag")
	}
	_, err = w.svc.UpdateCluster(ctx, &cloudv1.UpdateClusterRequest{TenantId: worldTenant,
		Cluster:    &cloudv1.Cluster{Id: w.row.ID, Etag: etag, DisplayName: "Again"},
		UpdateMask: mask("display_name")})
	if connect.CodeOf(err) != connect.CodeAborted && connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("an update with the etag from before that change: %v, want it refused", err)
	}
}
