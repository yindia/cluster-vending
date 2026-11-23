package cli

import (
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// cluster get prints the cluster status summary (PROP-02 phase 2b): the
// counts with their ages, the groups' requested and allocatable totals, and
// Reachable with its reason; "not read yet" before any pass, never zeros.
func TestPrintClusterSummary(t *testing.T) {
	withClock(t)
	groups := []*cloudv1.NodeGroupStatus{
		{Name: "system", PlatformManaged: true, Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, Nodes: 3, ReadyNodes: 2, NotReadyNodes: 1,
			AllocatableCpuMillis: 4000, RequestedCpuMillis: 900, AllocatableMemoryBytes: 8 << 30, RequestedMemoryBytes: 2 << 30, PodCount: 7},
		{Name: "batch", Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_PENDING},
	}
	read := func() *cloudv1.ClusterStatus {
		return &cloudv1.ClusterStatus{
			NodeGroups: groups,
			Nodes:      &cloudv1.ClusterNodeCounts{Total: 3, Ready: 2, NotReady: 1, NodesCollectedAt: ago(4 * time.Minute)},
			Violations: &cloudv1.ClusterViolationCounts{Total: 5, PolicyCollectedAt: ago(40 * time.Minute),
				BySeverity: []*cloudv1.ClusterCount{{Name: "high", Count: 3}, {Name: "low", Count: 2}}},
			ViolationEvents: &cloudv1.ClusterViolationEventCounts{Total: 1, PolicyCollectedAt: ago(40 * time.Minute),
				ByReason: []*cloudv1.ClusterCount{{Name: "PolicyViolation", Count: 1}}},
			Reachable: &cloudv1.ClusterReachability{Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, LastSuccessfulAt: ago(4 * time.Minute)},
		}
	}
	unreachable := read()
	unreachable.Nodes.NodesCollectedAt = ago(20 * time.Minute)
	unreachable.Reachable = &cloudv1.ClusterReachability{Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: "CREDENTIALS_REJECTED", LastSuccessfulAt: ago(20 * time.Minute)}
	tests := []struct {
		name string
		st   *cloudv1.ClusterStatus
		want []string
	}{
		{name: "not read yet", st: &cloudv1.ClusterStatus{}, want: []string{
			"Reachable:     not read yet\n",
			"Nodes:         not read yet\n",
			"Violations:    not read yet\n",
		}},
		{name: "read", st: read(), want: []string{
			"Reachable:     yes\n",
			"Nodes:         3 (2 ready, 1 not ready), collected 4m ago\n",
			"Violations:    5 (high 3, low 2); 1 policy events in the last hour (PolicyViolation 1), collected 40m ago (old)\n",
			"\nNode Groups:\n" +
				"  NAME     OWNER      PHASE     NODES   READY    CPU REQUESTED      MEMORY REQUESTED      PODS     MESSAGE\n" +
				"  system   platform   Ready     3       2        900m/4000m (22%)   2048Mi/8192Mi (25%)   7        <none>\n" +
				"  batch    user       Pending   0       <none>   <none>             <none>                <none>   <none>\n",
		}},
		{name: "unreachable keeps the last counts", st: unreachable, want: []string{
			"Reachable:     no (CREDENTIALS_REJECTED); showing the counts of 20m ago\n",
			"Nodes:         3 (2 ready, 1 not ready), collected 20m ago (old)\n",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testCluster()
			c.Status = tt.st
			got := printed(t, &cloudv1.GetClusterResponse{Cluster: c}, OutputTable)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("output lacks %q:\n%s", want, got)
				}
			}
		})
	}
}
