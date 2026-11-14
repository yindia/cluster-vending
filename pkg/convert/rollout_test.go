package convert

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A cluster's rollout record reads the same both ways, every result.
func TestClusterRolloutRoundTrip(t *testing.T) {
	start := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		result cloudv1.ClusterRolloutResult
		kind   cloudv1alpha1.ClusterRolloutResult
		finish bool
	}{
		{"in progress", cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS, cloudv1alpha1.ClusterRolloutInProgress, false},
		{"succeeded", cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_SUCCEEDED, cloudv1alpha1.ClusterRolloutSucceeded, true},
		{"failed", cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_FAILED, cloudv1alpha1.ClusterRolloutFailed, true},
		{"timed out", cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT, cloudv1alpha1.ClusterRolloutTimedOut, true},
		{"pending", cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING, cloudv1alpha1.ClusterRolloutPending, false},
		{"blocked", cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED, cloudv1alpha1.ClusterRolloutBlocked, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &cloudv1.ClusterRollout{Channel: "alpha", FromVersion: "1.36", ToVersion: "1.37",
				StartTime: timestamppb.New(start), Result: tt.result, Message: "why", Tier: 2}
			if tt.finish {
				p.FinishTime = timestamppb.New(start.Add(time.Hour))
			}

			kind := RolloutToAPI(p)
			back := RolloutFromAPI(kind)

			if kind.Result != tt.kind {
				t.Errorf("kind result = %q, want %q", kind.Result, tt.kind)
			}
			if !proto.Equal(back, p) {
				t.Errorf("round trip = %v, want %v", back, p)
			}
		})
	}
	if RolloutToAPI(nil) != nil || RolloutFromAPI(nil) != nil {
		t.Error("no record must stay none")
	}
}
