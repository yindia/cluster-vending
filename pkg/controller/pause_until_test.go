package controller

import (
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

// A row paused until a time is looked at again then, so its pause ends on
// time; a pause without an until changes nothing.
func TestUntilPauseEnds(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	soon, later := now.Add(3*time.Minute), now.Add(time.Hour)
	at := func(until *time.Time) *model.Pause {
		return (*model.Pause)(nil).With(model.PauseCluster, &model.Pause{By: "alice", Since: now, Until: until})
	}
	tests := []struct {
		name  string
		pause *model.Pause
		res   time.Duration
		want  time.Duration
	}{
		{"no pause", nil, resyncDelay, resyncDelay},
		{"no until", at(nil), resyncDelay, resyncDelay},
		{"until sooner than the resync", at(&soon), resyncDelay, 3 * time.Minute},
		{"until later than the next look", at(&later), applyingDelay, applyingDelay},
		{"until passed: at once", at(&now), resyncDelay, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := untilPauseEnds(tt.pause, ctrl.Result{RequeueAfter: tt.res}, now)

			if got.RequeueAfter != tt.want {
				t.Fatalf("RequeueAfter = %v, want %v", got.RequeueAfter, tt.want)
			}
		})
	}
}
