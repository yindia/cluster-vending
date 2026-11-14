package cluster

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// GetCluster shows the rollout record the controller wrote, with no other
// read.
func TestGetClusterShowsItsRollout(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	ctx := t.Context()
	record := `{"channel":"alpha","fromVersion":"1.36","toVersion":"1.37","startTime":"2026-11-15T02:00:00Z",` +
		`"result":"CLUSTER_ROLLOUT_RESULT_BLOCKED","message":"the maintenance window opens Sat 02:00 UTC","tier":2}`
	if err := w.store.Clusters().SetRollout(ctx, worldTenant, w.row.ID, []byte(record)); err != nil {
		t.Fatal(err)
	}

	res, err := w.svc.GetCluster(ctx, &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID})

	got := res.GetCluster().GetStatus().GetRollout()
	if err != nil || got.GetResult() != cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED ||
		got.GetToVersion() != "1.37" || got.GetTier() != 2 || got.GetMessage() == "" {
		t.Fatalf("rollout = %v, %v", got, err)
	}
}
