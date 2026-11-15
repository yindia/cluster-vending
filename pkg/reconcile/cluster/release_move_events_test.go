package cluster

import (
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

func withMove(m *cloudv1.ClusterReleaseMove) *cloudv1.ClusterStatus {
	return &cloudv1.ClusterStatus{ReleaseMove: m}
}

// A release move says when it starts, as each component is upgraded, when
// it halts and when it ends; a pass that changes nothing says nothing.
func TestMoveEvents(t *testing.T) {
	pending := &cloudv1.ClusterReleaseMove{Current: "a", Target: "b", Phase: movePending}
	first := &cloudv1.ClusterReleaseMove{Current: "a", Target: "b", Phase: moveComponents, Component: "base", Step: 1, Steps: 2}
	second := &cloudv1.ClusterReleaseMove{Current: "a", Target: "b", Phase: moveComponents, Component: "mesh", Step: 2, Steps: 2}
	halted := &cloudv1.ClusterReleaseMove{Current: "a", Target: "b", Phase: moveHalted, Component: "mesh", Step: 2, Steps: 2,
		Message: "mesh 2.1.0 failed: boom"}
	done := &cloudv1.ClusterReleaseMove{Current: "b", Phase: moveIdle, Steps: 2, Step: 2}
	recovering := &cloudv1.ClusterReleaseMove{Current: "a", Target: "b", Phase: moveComponents, Component: "mesh", Step: 2, Steps: 2,
		Recoveries: 1, Message: "recovering mesh (attempt 1 of 3): it 2.1.0 failed: boom"}
	tests := []struct {
		name          string
		before, after *cloudv1.ClusterReleaseMove
		want          []string // reason: message part
	}{
		{name: "starts", before: pending, after: first, want: []string{reconcile.ReasonReleaseMoveStarted + ": a to b"}},
		{name: "next component", before: first, after: second, want: []string{reconcile.ReasonComponentUpgraded + ": base"}},
		{name: "halts", before: second, after: halted, want: []string{reconcile.ReasonReleaseMoveHalted + ": boom"}},
		{name: "ends", before: second, after: done,
			want: []string{reconcile.ReasonComponentUpgraded + ": mesh", reconcile.ReasonReleaseMoveFinished + ": release b"}},
		{name: "recovers", before: second, after: recovering,
			want: []string{reconcile.ReasonReleaseMoveRecovering + ": attempt 1 of 3"}},
		{name: "unchanged", before: first, after: first},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notes := moveEvents(withMove(tt.before), withMove(tt.after))

			if len(notes) != len(tt.want) {
				t.Fatalf("notes = %+v, want %v", notes, tt.want)
			}
			for i, w := range tt.want {
				reason, part, _ := strings.Cut(w, ": ")
				if notes[i].Reason != reason || !strings.Contains(notes[i].Message, part) {
					t.Errorf("note %d = %+v, want %s saying %q", i, notes[i], reason, part)
				}
			}
		})
	}
}
