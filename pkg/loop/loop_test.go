package loop

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/mock/gomock"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/database/repository/mock"
)

type recorder struct {
	got  []string
	fail string
}

func (r *recorder) Reconcile(_ context.Context, tenantID, id string) error {
	r.got = append(r.got, tenantID+"/"+id)
	if id == r.fail {
		return errors.New("apply failed")
	}
	return nil
}

func TestPassReconcilesPendingRowsAndKeepsGoing(t *testing.T) {
	ctrl := gomock.NewController(t)
	store, clusters, networks := mock.NewMockStore(ctrl), mock.NewMockClusters(ctrl), mock.NewMockNetworks(ctrl)
	store.EXPECT().Clusters().Return(clusters).AnyTimes()
	store.EXPECT().Networks().Return(networks).AnyTimes()
	clusters.EXPECT().ListPending(gomock.Any(), []string{"UPDATING"}, int64(2)).Return([]model.Cluster{
		{Resource: model.Resource{Base: model.Base{ID: "c-1"}}, TenantID: "acme"},
		{Resource: model.Resource{Base: model.Base{ID: "c-2"}}, TenantID: "acme"}}, nil)
	networks.EXPECT().ListPending(gomock.Any(), []string{"DELETING"}, int64(2)).Return([]model.Network{
		{Resource: model.Resource{Base: model.Base{ID: "n-1"}}, TenantID: "beta"}}, nil)
	clusters.EXPECT().CountByState(gomock.Any()).Return(map[string]int64{"CLUSTER_STATE_RUNNING": 7}, nil)

	cl, nw := &recorder{fail: "c-1"}, &recorder{}
	errsBefore := testutil.ToFloat64(rowErrors.WithLabelValues(kindCluster))
	l := &Loop{Store: store, Clusters: cl, Networks: nw, ClusterStates: []string{"UPDATING"},
		NetworkStates: []string{"DELETING"}, RenderTarget: 2, Logger: slog.New(slog.DiscardHandler)}
	l.Pass(context.Background())

	if len(cl.got) != 2 || cl.got[1] != "acme/c-2" || len(nw.got) != 1 || nw.got[0] != "beta/n-1" {
		t.Errorf("reconciled clusters %v networks %v; want both clusters (after an error too) and the network", cl.got, nw.got)
	}
	if got := testutil.ToFloat64(rowErrors.WithLabelValues(kindCluster)) - errsBefore; got != 1 {
		t.Errorf("error counter rose by %v, want 1", got)
	}
	if got := testutil.ToFloat64(clustersByState.WithLabelValues("CLUSTER_STATE_RUNNING")); got != 7 {
		t.Errorf("clusters by state = %v, want 7", got)
	}
}

// The manager starts runnables that need leader election on the leader
// only.
func TestLoopRunsOnTheLeaderOnly(t *testing.T) {
	var r manager.LeaderElectionRunnable = &Loop{}
	if !r.NeedLeaderElection() {
		t.Fatal("the loop must need leader election")
	}
}
