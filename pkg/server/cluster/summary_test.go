package cluster

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// GetCluster returns the cluster status summary the controller recorded
// (PROP-02 phase 2b): absent before a pass, the counts after one, and the
// last counts with Reachable False when the cluster cannot be read.
func TestGetClusterCarriesTheStatusSummary(t *testing.T) {
	read := model.JSON(`{"reachable":true,"at":"2026-11-20T12:00:00Z",` +
		`"nodes":{"total":3,"ready":2,"notReady":1,"nodesCollectedAt":"2026-11-20T12:00:00Z"},` +
		`"nodeGroups":{"system":{"ready":2,"notReady":1,"allocatableCPUMillis":4000,"requestedCPUMillis":900,"podCount":7}},"requestsKnown":true}`)
	unreachable := model.JSON(`{"reachable":false,"reason":"CREDENTIALS_REJECTED","at":"2026-11-20T12:20:00Z"}`)
	tests := []struct {
		name      string
		summaries []model.JSON
		check     func(t *testing.T, st *cloudv1.ClusterStatus)
	}{
		{name: "not read yet", check: func(t *testing.T, st *cloudv1.ClusterStatus) {
			if st.GetNodes() != nil || st.GetReachable() != nil || st.GetViolations() != nil {
				t.Fatalf("summary before any pass: %v %v %v", st.GetNodes(), st.GetReachable(), st.GetViolations())
			}
		}},
		{name: "read", summaries: []model.JSON{read}, check: func(t *testing.T, st *cloudv1.ClusterStatus) {
			if n := st.GetNodes(); n.GetTotal() != 3 || n.GetReady() != 2 || n.GetNodesCollectedAt() == nil {
				t.Fatalf("nodes = %v", n)
			}
			if r := st.GetReachable(); r.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE || r.GetReason() != "" {
				t.Fatalf("reachable = %v", r)
			}
			g := groupNamed(st, "system")
			if g.GetRequestedCpuMillis() != 900 || g.GetAllocatableCpuMillis() != 4000 || g.GetPodCount() != 7 || g.GetReadyNodes() != 2 {
				t.Fatalf("system group = %v", g)
			}
		}},
		{name: "unreachable keeps the last counts", summaries: []model.JSON{read, unreachable}, check: func(t *testing.T, st *cloudv1.ClusterStatus) {
			r := st.GetReachable()
			if r.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_FALSE || r.GetReason() != "CREDENTIALS_REJECTED" || r.GetLastSuccessfulAt() == nil {
				t.Fatalf("reachable = %v", r)
			}
			if st.GetNodes().GetTotal() != 3 {
				t.Fatalf("counts lost: %v", st.GetNodes())
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			for _, s := range tt.summaries {
				if err := w.store.Clusters().SetSummary(t.Context(), worldTenant, w.row.ID, s); err != nil {
					t.Fatalf("SetSummary: %v", err)
				}
			}
			res, err := w.svc.GetCluster(t.Context(), &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: w.row.ID})
			if err != nil {
				t.Fatalf("GetCluster: %v", err)
			}
			tt.check(t, res.GetCluster().GetStatus())
		})
	}
}

func groupNamed(st *cloudv1.ClusterStatus, name string) *cloudv1.NodeGroupStatus {
	for _, g := range st.GetNodeGroups() {
		if g.GetName() == name {
			return g
		}
	}
	return nil
}
