package cluster

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A component whose Release is being deleted (its definition was
// disabled, or it left the release) is being removed, not installed.
func TestComponentPhaseOfARemovedChart(t *testing.T) {
	tests := []struct {
		name  string
		state cloudv1.ManagedResourceState
		want  cloudv1.ComponentPhase
	}{
		{"deleting", cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_DELETING, cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING},
		{"creating", cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_CREATING, cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING},
		{"ready", cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY, cloudv1.ComponentPhase_COMPONENT_PHASE_READY},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := map[string]*cloudv1.ManagedResource{"kyverno": {Name: "kyverno", State: tt.state}}

			phase, message, _ := componentPhase([]string{"kyverno"}, state, nil, nil)

			if phase != tt.want {
				t.Fatalf("phase = %v (%q), want %v", phase, message, tt.want)
			}
			if tt.want == cloudv1.ComponentPhase_COMPONENT_PHASE_DELETING && message != removingMessage {
				t.Errorf("message = %q, want %q", message, removingMessage)
			}
		})
	}
}
