package clustersummary

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestWait(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	read := func(age time.Duration, status cloudv1.ConditionStatus) *cloudv1.ClusterStatus {
		return &cloudv1.ClusterStatus{
			Nodes:     &cloudv1.ClusterNodeCounts{Total: 3, NodesCollectedAt: timestamppb.New(now.Add(-age))},
			Reachable: &cloudv1.ClusterReachability{Status: status},
		}
	}
	tests := []struct {
		name string
		sum  *cloudv1.ClusterStatus
		want string
	}{
		{name: "no summary yet", sum: nil, want: ""},
		{name: "14 minutes old", sum: read(14*time.Minute, cloudv1.ConditionStatus_CONDITION_STATUS_TRUE), want: ""},
		{name: "exactly 15 minutes", sum: read(15*time.Minute, cloudv1.ConditionStatus_CONDITION_STATUS_TRUE), want: ""},
		{name: "16 minutes old", sum: read(16*time.Minute, cloudv1.ConditionStatus_CONDITION_STATUS_TRUE), want: "the cluster's summary is 16 minutes old"},
		{name: "unreachable, fresh counts", sum: read(time.Minute, cloudv1.ConditionStatus_CONDITION_STATUS_FALSE), want: "the platform cannot read the cluster"},
		{name: "unreachable before any count", sum: &cloudv1.ClusterStatus{Reachable: &cloudv1.ClusterReachability{Status: cloudv1.ConditionStatus_CONDITION_STATUS_FALSE}},
			want: "the platform cannot read the cluster"},
		{name: "reachable without node counts", sum: &cloudv1.ClusterStatus{Reachable: &cloudv1.ClusterReachability{Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE}}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Wait(tt.sum, now); got != tt.want {
				t.Fatalf("Wait = %q, want %q", got, tt.want)
			}
		})
	}
}
