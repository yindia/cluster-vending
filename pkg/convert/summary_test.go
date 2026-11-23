package convert

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestSummaryFromAPI(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	ts := timestamppb.New(at.Time)
	usage := cloudv1alpha1.NodeGroupUsage{Ready: 2, NotReady: 1, AllocatableCPUMillis: 4000, AllocatableMemoryBytes: 8 << 30,
		AllocatablePods: 58, RequestedCPUMillis: 900, RequestedMemoryBytes: 1 << 30, PodCount: 7, PodsWithoutRequests: 2}
	read := cloudv1alpha1.ClusterStatus{
		Nodes:           &cloudv1alpha1.ClusterNodesStatus{Total: 3, Ready: 2, NotReady: 1, NodesCollectedAt: &at},
		Violations:      &cloudv1alpha1.ClusterViolationsStatus{Total: 4, BySeverity: []cloudv1alpha1.ClusterCount{{Name: "high", Count: 4}}, PolicyCollectedAt: &at},
		ViolationEvents: &cloudv1alpha1.ClusterViolationEventsStatus{Total: 1, ByReason: []cloudv1alpha1.ClusterCount{{Name: "PolicyViolation", Count: 1}}, PolicyCollectedAt: &at},
		Conditions:      []metav1.Condition{{Type: cloudv1alpha1.ConditionReachable, Status: metav1.ConditionTrue, Reason: "Reachable", Message: "read"}},
		NodeGroups:      []cloudv1alpha1.NodeGroupStatus{{Name: "system", Nodes: 3, NodeGroupUsage: usage}, {Name: "web", Nodes: 0}},
	}
	unreachable := *read.DeepCopy()
	unreachable.Conditions = []metav1.Condition{{Type: cloudv1alpha1.ConditionReachable, Status: metav1.ConditionFalse,
		Reason: cloudv1alpha1.ReasonCredentialsRejected, Message: "the workload cluster cannot be read"}}
	group := &cloudv1.NodeGroupStatus{Name: "system", Nodes: 3, ReadyNodes: 2, NotReadyNodes: 1, AllocatableCpuMillis: 4000,
		AllocatableMemoryBytes: 8 << 30, AllocatablePods: 58, RequestedCpuMillis: 900, RequestedMemoryBytes: 1 << 30, PodCount: 7, PodsWithoutRequests: 2}
	counts := &cloudv1.ClusterStatus{
		Nodes:           &cloudv1.ClusterNodeCounts{Total: 3, Ready: 2, NotReady: 1, NodesCollectedAt: ts},
		Violations:      &cloudv1.ClusterViolationCounts{Total: 4, BySeverity: []*cloudv1.ClusterCount{{Name: "high", Count: 4}}, PolicyCollectedAt: ts},
		ViolationEvents: &cloudv1.ClusterViolationEventCounts{Total: 1, ByReason: []*cloudv1.ClusterCount{{Name: "PolicyViolation", Count: 1}}, PolicyCollectedAt: ts},
		NodeGroups:      []*cloudv1.NodeGroupStatus{group},
	}
	withReach := func(r *cloudv1.ClusterReachability) *cloudv1.ClusterStatus {
		out := proto.CloneOf(counts)
		out.Reachable = r
		return out
	}
	tests := []struct {
		name string
		in   *cloudv1alpha1.ClusterStatus
		want *cloudv1.ClusterStatus
	}{
		{name: "nil status", in: nil, want: nil},
		{name: "never read: absent, not zeros", in: &cloudv1alpha1.ClusterStatus{NodeGroups: []cloudv1alpha1.NodeGroupStatus{{Name: "system", Nodes: 2}}}, want: nil},
		{name: "read", in: &read, want: withReach(&cloudv1.ClusterReachability{Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE, Message: "read", LastSuccessfulAt: ts})},
		{name: "unreachable keeps the last counts", in: &unreachable, want: withReach(&cloudv1.ClusterReachability{Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
			Reason: cloudv1alpha1.ReasonCredentialsRejected, Message: "the workload cluster cannot be read", LastSuccessfulAt: ts})},
		{name: "unreachable before any count", in: &cloudv1alpha1.ClusterStatus{Conditions: unreachable.Conditions},
			want: &cloudv1.ClusterStatus{Reachable: &cloudv1.ClusterReachability{Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE,
				Reason: cloudv1alpha1.ReasonCredentialsRejected, Message: "the workload cluster cannot be read"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SummaryFromAPI(tt.in)
			if !proto.Equal(got, tt.want) {
				t.Fatalf("got  %v\nwant %v", got, tt.want)
			}
		})
	}
}

func TestWithSummary(t *testing.T) {
	sum := &cloudv1.ClusterStatus{
		Nodes: &cloudv1.ClusterNodeCounts{Total: 5},
		NodeGroups: []*cloudv1.NodeGroupStatus{
			{Name: "system", ReadyNodes: 2, RequestedCpuMillis: 900, PodCount: 4},
			{Name: cloudv1alpha1.StatusOtherNodeGroup, ReadyNodes: 3, PodCount: 1},
		},
		Reachable: &cloudv1.ClusterReachability{Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE},
	}
	tests := []struct {
		name       string
		st         *cloudv1.ClusterStatus
		sum        *cloudv1.ClusterStatus
		wantGroups []*cloudv1.NodeGroupStatus
		wantNodes  int32
	}{
		{name: "no summary", st: &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{{Name: "system", Nodes: 2}}},
			wantGroups: []*cloudv1.NodeGroupStatus{{Name: "system", Nodes: 2}}},
		{name: "merged by name, the rest appended", st: &cloudv1.ClusterStatus{NodeGroups: []*cloudv1.NodeGroupStatus{
			{Name: "system", Nodes: 2, Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY}, {Name: "web", Nodes: 0}}}, sum: sum,
			wantNodes: 5,
			wantGroups: []*cloudv1.NodeGroupStatus{
				{Name: "system", Nodes: 2, Phase: cloudv1.NodeGroupPhase_NODE_GROUP_PHASE_READY, ReadyNodes: 2, RequestedCpuMillis: 900, PodCount: 4},
				{Name: "web"},
				{Name: cloudv1alpha1.StatusOtherNodeGroup, ReadyNodes: 3, PodCount: 1},
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			WithSummary(tt.st, tt.sum)
			if tt.st.GetNodes().GetTotal() != tt.wantNodes {
				t.Fatalf("nodes = %v", tt.st.GetNodes())
			}
			if len(tt.st.NodeGroups) != len(tt.wantGroups) {
				t.Fatalf("groups = %v", tt.st.NodeGroups)
			}
			for i := range tt.wantGroups {
				if !proto.Equal(tt.st.NodeGroups[i], tt.wantGroups[i]) {
					t.Fatalf("group %d = %v, want %v", i, tt.st.NodeGroups[i], tt.wantGroups[i])
				}
			}
		})
	}
}
