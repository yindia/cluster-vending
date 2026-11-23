package rollout

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// The readiness gate reads the cluster status summary (PROP-02 5.2): an
// unreachable cluster, or node counts older than 15 minutes, wait.
func TestComputeWaitsOnTheSummary(t *testing.T) {
	summarized := func(age time.Duration, reachable bool) cloudv1alpha1.Cluster {
		c := cluster("next", cloudv1alpha1.ClusterTier2, "1.35")
		at := metav1.NewTime(now.Add(-age))
		c.Status.Nodes = &cloudv1alpha1.ClusterNodesStatus{Total: 3, Ready: 3, NodesCollectedAt: &at}
		status := metav1.ConditionTrue
		if !reachable {
			status = metav1.ConditionFalse
		}
		c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{Type: cloudv1alpha1.ConditionReachable, Status: status,
			Reason: cloudv1alpha1.ReasonClusterUnreachable, LastTransitionTime: at})
		return c
	}
	tests := []struct {
		name    string
		cluster cloudv1alpha1.Cluster
		want    string // the gate's message; "" for none
	}{
		{name: "no summary yet", cluster: cluster("next", cloudv1alpha1.ClusterTier2, "1.35")},
		{name: "14 minutes old", cluster: summarized(14*time.Minute, true)},
		{name: "16 minutes old", cluster: summarized(16*time.Minute, true), want: "the cluster's summary is 16 minutes old"},
		{name: "unreachable", cluster: summarized(time.Minute, false), want: "the platform cannot read the cluster"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := rowOf(t, Compute(input(tt.cluster)), "next")
			if tt.want == "" {
				if row.Gate != nil {
					t.Fatalf("gate = %+v", row.Gate)
				}
				return
			}
			if row.State != upgrade.Blocked || row.Gate == nil || row.Gate.Code != upgrade.GateNotReady || row.Gate.Message != tt.want {
				t.Fatalf("row = %+v gate %+v", row, row.Gate)
			}
		})
	}
}
