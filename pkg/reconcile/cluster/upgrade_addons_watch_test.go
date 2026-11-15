package cluster

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The add-ons' step is watched: add-ons that fail or do not settle in 30
// minutes are recovered up to three times, then the upgrade halts.
func TestWatchAddons(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	was := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = was })

	failedView := []*cloudv1.ManagedResource{{Kind: "Addon", Name: "c1-coredns",
		State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_FAILED, Reason: "UPDATE_FAILED", Message: "pods not ready"}}
	updating := []*cloudv1.ManagedResource{{Kind: "Addon", Name: "c1-coredns", State: cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING}}
	step := func(since time.Duration, recoveries int32, halted bool) (prev, out *cloudv1.ClusterUpgrade) {
		start := timestamppb.New(at.Add(-since))
		prev = &cloudv1.ClusterUpgrade{Phase: stepAddonsPhase, Applied: true, PhaseStartTime: start, Recoveries: recoveries, Halted: halted,
			Message: "halted before"}
		out = &cloudv1.ClusterUpgrade{Phase: stepAddonsPhase, Applied: true, PhaseStartTime: start, AddonsBehind: []string{"coredns"}}
		return prev, out
	}
	row := &model.Cluster{ID: "c1"}
	tests := []struct {
		name           string
		since          time.Duration
		recoveries     int32
		halted         bool
		views          []*cloudv1.ManagedResource
		retry          bool
		wantRecoveries int32
		wantHalted     bool
		wantMessage    string
	}{
		{name: "updating within the watch", since: 10 * time.Minute, views: updating, wantMessage: ""},
		{name: "failed: recovered at once", since: time.Minute, views: failedView, wantRecoveries: 1,
			wantMessage: "recovering the add-ons coredns (attempt 1 of 3): Addon c1-coredns: UPDATE_FAILED pods not ready"},
		{name: "a recovered add-on's failure waits for the grace", since: time.Minute, recoveries: 1, views: failedView, wantRecoveries: 1},
		{name: "not settled within 30m: recovered", since: 31 * time.Minute, recoveries: 1, views: updating, wantRecoveries: 2,
			wantMessage: "not settled within 30m0s"},
		{name: "after the last recovery: halted", since: 31 * time.Minute, recoveries: 3, views: failedView, wantRecoveries: 3, wantHalted: true,
			wantMessage: "after 3 recoveries: Addon c1-coredns: UPDATE_FAILED"},
		{name: "halted stays halted", since: time.Hour, recoveries: 3, halted: true, views: updating, wantRecoveries: 3, wantHalted: true,
			wantMessage: "halted before"},
		{name: "a retry starts the watch over", since: time.Hour, recoveries: 3, halted: true, views: failedView, retry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev, out := step(tt.since, tt.recoveries, tt.halted)
			r := *row
			if tt.retry {
				r.Retry = &model.Retry{At: at}
			}

			watchAddons(&r, prev, out, tt.views)

			if out.GetRecoveries() != tt.wantRecoveries || out.GetHalted() != tt.wantHalted {
				t.Fatalf("recoveries %d halted %v, want %d %v (%s)", out.GetRecoveries(), out.GetHalted(), tt.wantRecoveries, tt.wantHalted, out.GetMessage())
			}
			if !strings.Contains(out.GetMessage(), tt.wantMessage) {
				t.Errorf("message %q, want it to say %q", out.GetMessage(), tt.wantMessage)
			}
			if recovered := addonsRecovering(prev, out); recovered != (tt.wantRecoveries > tt.recoveries) {
				t.Errorf("recovering = %v", recovered)
			}
			if tt.wantRecoveries > tt.recoveries && !out.GetPhaseStartTime().AsTime().Equal(at) {
				t.Errorf("a recovery's watch starts at %v, want now", out.GetPhaseStartTime().AsTime())
			}
		})
	}
}
