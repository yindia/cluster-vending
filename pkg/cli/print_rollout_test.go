package cli

import (
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// cluster get's Rollout line: where the cluster is in its channel's
// rollout before the upgrade starts, then how the upgrade goes.
func TestRolloutLine(t *testing.T) {
	withClock(t)
	hop := func(result cloudv1.ClusterRolloutResult, message string) *cloudv1.ClusterRollout {
		return &cloudv1.ClusterRollout{Channel: "alpha", FromVersion: "1.36", ToVersion: "1.37", Tier: 2,
			StartTime: ago(12 * time.Minute), Result: result, Message: message}
	}
	done := hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_SUCCEEDED, "")
	done.FinishTime = ago(5 * time.Hour)
	tests := []struct {
		name string
		in   *cloudv1.ClusterRollout
		want string
	}{
		{"none", nil, ""},
		{"next", hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING, "next to start"),
			"1.36 → 1.37 via channel alpha (tier 2): next to start"},
		{"waiting", hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING, "waiting: tier 2 first"),
			"1.36 → 1.37 via channel alpha (tier 2): waiting: tier 2 first"},
		{"off", hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING, "automatic upgrades are off (would start)"),
			"1.36 → 1.37 via channel alpha (tier 2): automatic upgrades are off (would start)"},
		{"blocked", hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED, "the maintenance window opens Sat 02:00 UTC"),
			"1.36 → 1.37 via channel alpha (tier 2): blocked: the maintenance window opens Sat 02:00 UTC"},
		{"manual", hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_BLOCKED, "manual upgrades: 1.37 is available, not started"),
			"manual upgrades: 1.37 is available, not started"},
		{"in progress", hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_IN_PROGRESS, ""),
			"1.36 → 1.37 via channel alpha (tier 2): in progress since 12m"},
		{"succeeded", done, "1.36 → 1.37 via channel alpha (tier 2): succeeded 5h ago"},
		{"failed", hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_FAILED, "UpdateClusterVersion: InvalidRequest"),
			"1.36 → 1.37 via channel alpha (tier 2): failed: UpdateClusterVersion: InvalidRequest"},
		{"timed out", hop(cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_TIMED_OUT, "not finished within 1h30m0s"),
			"1.36 → 1.37 via channel alpha (tier 2): timed out: not finished within 1h30m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rolloutLine(tt.in); got != tt.want {
				t.Errorf("got %q\nwant %q", got, tt.want)
			}
		})
	}
}

// cluster get shows the line under the upgrade mode.
func TestPrintClusterDescribeShowsTheRollout(t *testing.T) {
	withClock(t)
	c := testCluster()
	c.Status.Rollout = &cloudv1.ClusterRollout{Channel: "alpha", FromVersion: "1.36", ToVersion: "1.37", Tier: 2,
		StartTime: ago(time.Minute), Result: cloudv1.ClusterRolloutResult_CLUSTER_ROLLOUT_RESULT_PENDING, Message: "next to start"}

	got := printed(t, &cloudv1.GetClusterResponse{Cluster: c}, OutputTable)

	want := "Upgrades:      Automatic (follow the channel's minor)\n" +
		"Rollout:       1.36 → 1.37 via channel alpha (tier 2): next to start\n"
	if !strings.Contains(got, want) {
		t.Errorf("got\n%s\nwant it to contain\n%s", got, want)
	}
}
