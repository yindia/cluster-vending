package cluster

import (
	"strings"
	"testing"
	"time"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/helmrel"
)

// While the platform's own backoff pauses a Release, Crossplane reports it
// paused with no error: the component still says why it failed and when
// it is tried again, never the generic "reconciliation is paused".
func TestBackoffPauseKeepsTheRememberedFailure(t *testing.T) {
	next := time.Date(2026, 11, 25, 15, 29, 45, 0, time.UTC)
	lastErr := "failed to record the release: connection reset by peer"
	tests := []struct {
		name  string
		class helmrel.ErrorClass
		phase cloudv1.ComponentPhase
		want  string
	}{
		{name: "transient", class: helmrel.ClassTransient, phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING,
			want: "retrying after a transient error, next try at 2026-11-25T15:29:45Z: " + lastErr},
		{name: "release error", class: helmrel.Classify("chart failed: invalid values"), phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED,
			want: "next try at 2026-11-25T15:29:45Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &cloudv1.PlatformComponent{Name: "capsule", Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING,
				Message: "reconciliation is paused (crossplane.io/paused): its provider does not act until it is resumed"}
			st := helmrel.State{Attempts: 2, Class: tt.class, LastError: lastErr, NextRetry: next}
			d := &releaseDetail{Chart: "capsule", PausedByUs: true, BackoffUntil: next} // Synced is ReconcilePaused: no error

			reportBackoff(c, st, d, true)

			if c.GetPhase() != tt.phase || !strings.Contains(c.GetMessage(), tt.want) || strings.Contains(c.GetMessage(), "crossplane.io/paused") {
				t.Errorf("phase %s, message %q; want %s containing %q", c.GetPhase(), c.GetMessage(), tt.phase, tt.want)
			}
		})
	}
}
