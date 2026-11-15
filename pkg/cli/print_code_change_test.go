package cli

import (
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A pending, applying or halted code change is one line: what changes and
// where it is; a done one, or none, says nothing.
func TestCodeChangeLine(t *testing.T) {
	withClock(t)
	tests := []struct {
		c    *cloudv1.CodeChange
		want string
	}{
		{nil, ""},
		{&cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_DONE, Summary: "3 objects change"}, ""},
		{&cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING, Summary: "3 objects change",
			Message: "the platform is frozen: deploy"}, "3 objects change: the platform is frozen: deploy"},
		{&cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_APPLYING, Summary: "1 object changes",
			StartTime: ago(12 * time.Minute)}, "1 object changes: applying since 12m"},
		{&cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_HALTED, Summary: "1 object changes",
			Message: "applying it failed 4 times: boom"}, "1 object changes: halted: applying it failed 4 times: boom"},
	}
	for _, tt := range tests {
		if got := codeChangeLine(tt.c); got != tt.want {
			t.Errorf("codeChangeLine(%v) = %q, want %q", tt.c, got, tt.want)
		}
	}
}

// cluster get prints a pending code change on its Code Change line,
// network get on its Rollout line; channel get the channel's code
// rollout and the clusters it keeps back.
func TestPrintCodeChanges(t *testing.T) {
	withClock(t)
	pending := &cloudv1.CodeChange{Phase: cloudv1.CodeChangePhase_CODE_CHANGE_PHASE_PENDING, Summary: "2 objects change",
		Message: "waiting its turn in the channel's code rollout: tier 2 first"}
	cl := testCluster()
	cl.Status.CodeChange = pending
	if got := printed(t, &cloudv1.GetClusterResponse{Cluster: cl}, OutputTable); !strings.Contains(got,
		"Code Change:   2 objects change: waiting its turn in the channel's code rollout: tier 2 first\n") {
		t.Errorf("cluster get:\n%s", got)
	}
	n := &cloudv1.Network{Name: "main", Status: &cloudv1.NetworkStatus{CodeChange: pending}}
	if got := printed(t, &cloudv1.GetNetworkResponse{Network: n}, OutputTable); !strings.Contains(got,
		"Rollout:    code change: 2 objects change: waiting its turn") {
		t.Errorf("network get:\n%s", got)
	}
	info := &cloudv1.ReleaseChannelInfo{Channel: stable, CodeChanges: &cloudv1.ChangeRollout{Waiting: "tier 2 first",
		Tiers:   []*cloudv1.RolloutTier{{Tier: tier2, Pending: 2}},
		Blocked: []*cloudv1.BlockedMember{{Cluster: "tenant-acme/prod", Gate: "FROZEN", Reason: "the platform is frozen"}}}}
	got := printed(t, &cloudv1.GetReleaseChannelResponse{Channel: info}, OutputTable)
	for _, want := range []string{"Code Changes:     Rolling, 0/2 done; waiting: tier 2 first; 1 kept back\n",
		"Code Changes Kept Back:\n", "  tenant-acme/prod   FROZEN   the platform is frozen\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("channel get lacks %q:\n%s", want, got)
		}
	}
}
