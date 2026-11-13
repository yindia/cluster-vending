package reconcile

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestPausedCondition(t *testing.T) {
	view := func(paused bool) *cloudv1.ManagedResource {
		return &cloudv1.ManagedResource{Kind: "NodeGroup", Paused: paused}
	}
	tests := []struct {
		name  string
		views []*cloudv1.ManagedResource
		want  string // the condition's message starts with it; "": no condition
		note  string
	}{
		{name: "nothing recorded"},
		{name: "none paused", views: []*cloudv1.ManagedResource{view(false), view(false)}},
		{name: "some paused", views: []*cloudv1.ManagedResource{view(true), view(false), view(true)},
			want: "2 of 3 resources are paused", note: "; 2 resources paused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := PausedCondition(tt.views)

			if tt.want == "" {
				if cond != nil || PausedNote(tt.views) != "" {
					t.Fatalf("condition %v, note %q; want none", cond, PausedNote(tt.views))
				}
				return
			}
			if cond == nil || cond.GetType() != ConditionPaused || cond.GetStatus() != cloudv1.ConditionStatus_CONDITION_STATUS_TRUE ||
				cond.GetReason() != managed.ReasonPaused || !strings.HasPrefix(cond.GetMessage(), tt.want) {
				t.Fatalf("condition = %v, want Paused=True starting %q", cond, tt.want)
			}
			if got := PausedNote(tt.views); got != tt.note {
				t.Errorf("note = %q, want %q", got, tt.note)
			}
		})
	}
}
