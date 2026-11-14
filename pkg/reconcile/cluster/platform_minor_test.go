package cluster

import "testing"

// Platform components follow the minor the control plane runs: during an
// upgrade they keep the old minor's values (cluster-autoscaler's image)
// until EKS reports the control plane on the new one.
func TestComponentsMinor(t *testing.T) {
	tests := []struct {
		name, spec, running, want string
	}{
		{"upgrade under way", "1.37", "1.36", "1.36"},
		{"control plane upgraded", "1.37", "1.37", "1.37"},
		{"patch versions", "1.37", "1.36.4", "1.36"},
		{"not reported yet: a new cluster", "1.37", "", "1.37"},
		{"ahead of the spec (never lowered)", "1.36", "1.37", "1.36"},
		{"unreadable running version", "1.37", "garbage", "1.37"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := componentsMinor(tt.spec, tt.running); got != tt.want {
				t.Errorf("componentsMinor(%q, %q) = %q, want %q", tt.spec, tt.running, got, tt.want)
			}
		})
	}
}
