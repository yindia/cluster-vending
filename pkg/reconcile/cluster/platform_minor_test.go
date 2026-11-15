package cluster

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The components follow an upgrade only at their step: the platform's at
// PLATFORM_COMPONENTS, the tenant's at TENANT_COMPONENTS; before, they
// keep the minor the upgrade started from.
func TestComponentsMinor(t *testing.T) {
	at := func(step cloudv1.ClusterUpgradePhase) *cloudv1.ClusterStatus {
		return &cloudv1.ClusterStatus{CurrentKubernetesVersion: "1.37",
			Upgrade: &cloudv1.ClusterUpgrade{FromVersion: "1.36", ToVersion: "1.37", Phase: step}}
	}
	tests := []struct {
		name           string
		spec           string
		stored         *cloudv1.ClusterStatus
		platform, tent string
	}{
		{"no upgrade", "1.37", &cloudv1.ClusterStatus{CurrentKubernetesVersion: "1.37"}, "1.37", "1.37"},
		{"a new cluster", "1.37", nil, "1.37", "1.37"},
		{"asked for, not seen yet", "1.37", &cloudv1.ClusterStatus{CurrentKubernetesVersion: "1.36.4"}, "1.36", "1.36"},
		{"control plane", "1.37", at(controlPlane), "1.36", "1.36"},
		{"node groups", "1.37", at(stepNodes), "1.36", "1.36"},
		{"add-ons", "1.37", at(stepAddons), "1.36", "1.36"},
		{"platform components", "1.37", at(stepPlatform), "1.37", "1.36"},
		{"tenant components", "1.37", at(stepTenant), "1.37", "1.37"},
		{"a recorded upgrade to another minor", "1.38", at(stepNodes), "1.37", "1.37"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := componentsMinor(tt.spec, tt.stored, false); got != tt.platform {
				t.Errorf("platform: %q, want %q", got, tt.platform)
			}
			if got := componentsMinor(tt.spec, tt.stored, true); got != tt.tent {
				t.Errorf("tenant: %q, want %q", got, tt.tent)
			}
		})
	}
}
