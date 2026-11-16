package cluster

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// A pause (AdminService.PauseReconcile, e.g. during a provider upgrade) is
// reported on the cluster and never turns it FAILED.
func TestStatusOfAPausedCluster(t *testing.T) {
	const (
		ready   = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY
		waiting = cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_WAITING
		synced  = cloudv1.ConditionStatus_CONDITION_STATUS_TRUE
		paused  = cloudv1.ConditionStatus_CONDITION_STATUS_FALSE // what Crossplane sets on Synced
	)
	mr := func(kind string, state cloudv1.ManagedResourceState, isPaused bool) *cloudv1.ManagedResource {
		v := &cloudv1.ManagedResource{Kind: kind, Name: kind, State: state, Synced: synced, Generation: 1, ObservedGeneration: 1, Paused: isPaused}
		if isPaused {
			v.Synced = paused
			if state != ready {
				v.Reason = managed.ReasonPaused
			}
		}
		return v
	}
	running := model.Cluster{Resource: model.Resource{State: StateRunning, Generation: 1, AppliedGeneration: 1, ObservedGeneration: 1}}
	tests := []struct {
		name       string
		views      []*cloudv1.ManagedResource
		want       cloudv1.ClusterState
		wantPaused bool
		wantInMsg  string
	}{
		{name: "nothing paused", views: []*cloudv1.ManagedResource{mr("Cluster", ready, false), mr("NodeGroup", ready, false)},
			want: cloudv1.ClusterState_CLUSTER_STATE_RUNNING},
		{name: "ready and paused stays running", views: []*cloudv1.ManagedResource{mr("Cluster", ready, true), mr("NodeGroup", ready, true)},
			want: cloudv1.ClusterState_CLUSTER_STATE_RUNNING, wantPaused: true},
		{name: "a paused resource that is not ready is not a failure",
			views: []*cloudv1.ManagedResource{mr("Cluster", ready, true), mr("NodeGroup", waiting, true)},
			want:  cloudv1.ClusterState_CLUSTER_STATE_UPDATING, wantPaused: true, wantInMsg: "2 resources paused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := running

			st := status(&row, tt.views, false)

			if st.GetState() != tt.want {
				t.Fatalf("state %s (%s), want %s", st.GetState(), st.GetMessage(), tt.want)
			}
			var cond *cloudv1.Condition
			for _, c := range st.GetConditions() {
				if c.GetType() == reconcile.ConditionFrozen {
					cond = c
				}
			}
			if (cond != nil) != tt.wantPaused {
				t.Fatalf("Paused condition = %v, want present %v", cond, tt.wantPaused)
			}
			if !strings.Contains(st.GetMessage(), tt.wantInMsg) {
				t.Errorf("message %q does not say %q", st.GetMessage(), tt.wantInMsg)
			}
		})
	}
}
