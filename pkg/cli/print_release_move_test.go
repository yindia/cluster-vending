package cli

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// cluster get's Release line says where a release move is.
func TestReleaseLine(t *testing.T) {
	withClock(t)
	since := timestamppb.New(clock().Add(-4 * time.Minute))
	tests := []struct {
		name string
		st   *cloudv1.ClusterStatus
		want string
	}{
		{name: "no move", st: &cloudv1.ClusterStatus{PlatformRelease: "shipped-a"}, want: "shipped-a"},
		{name: "idle", st: &cloudv1.ClusterStatus{PlatformRelease: "shipped-b",
			ReleaseMove: &cloudv1.ClusterReleaseMove{Current: "shipped-b", Phase: cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_IDLE}},
			want: "shipped-b"},
		{name: "moving", st: &cloudv1.ClusterStatus{PlatformRelease: "shipped-a",
			ReleaseMove: &cloudv1.ClusterReleaseMove{Current: "shipped-a", Target: "shipped-b", Component: "karpenter", Step: 3, Steps: 9,
				Phase: cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_COMPONENTS, ComponentStartTime: since}},
			want: "shipped-a → shipped-b: karpenter (3 of 9), since 4m"},
		{name: "halted", st: &cloudv1.ClusterStatus{PlatformRelease: "shipped-a",
			ReleaseMove: &cloudv1.ClusterReleaseMove{Current: "shipped-a", Target: "shipped-b", Component: "karpenter", Step: 3, Steps: 9,
				Phase: cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_HALTED, Message: "karpenter 1.16.0 failed: boom"}},
			want: "shipped-a → shipped-b: halted at karpenter (3 of 9): karpenter 1.16.0 failed: boom"},
		{name: "available", st: &cloudv1.ClusterStatus{PlatformRelease: "shipped-a",
			ReleaseMove: &cloudv1.ClusterReleaseMove{Current: "shipped-a", Target: "shipped-b",
				Phase: cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_PENDING, Message: "waiting its turn in the channel's rollout"}},
			want: "shipped-a (shipped-b available: waiting its turn in the channel's rollout)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := releaseLine(tt.st); got != tt.want {
				t.Errorf("Release: %q, want %q", got, tt.want)
			}
		})
	}
}
