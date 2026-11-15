package management

import (
	"strings"
	"testing"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// The management cluster's platform source gates release moves.
var _ reconcile.MoveAdmitter = platformSource{}

func TestAdmittedBy(t *testing.T) {
	planned := &cloudv1alpha1.ChannelReleaseMoves{Release: "r2", Admitted: []string{"tenant-acme/prod"}, Waiting: "tier 2 first"}
	tests := []struct {
		name  string
		moves *cloudv1alpha1.ChannelReleaseMoves
		key   string
		want  bool
		why   string
	}{
		{name: "admitted", moves: planned, key: "tenant-acme/prod", want: true},
		{name: "not yet", moves: planned, key: "tenant-acme/dev", why: "tier 2 first"},
		{name: "not planned", key: "tenant-acme/prod", why: "to be planned"},
		{name: "planned for another release", moves: &cloudv1alpha1.ChannelReleaseMoves{Release: "r1",
			Admitted: []string{"tenant-acme/prod"}}, key: "tenant-acme/prod", why: "to be planned"},
		{name: "halted", moves: &cloudv1alpha1.ChannelReleaseMoves{Release: "r2", Halted: true, Waiting: "2 failed"},
			key: "tenant-acme/prod", why: "halted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why, err := admittedBy(tt.moves, "r2", tt.key)
			if err != nil || got != tt.want || !strings.Contains(why, tt.why) {
				t.Fatalf("admitted %v (%q, %v), want %v saying %q", got, why, err, tt.want, tt.why)
			}
		})
	}
}
