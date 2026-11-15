package rollout

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// The platform freeze holds every cluster, except those it lets through,
// and says so with its own gate.
func TestPlatformFreeze(t *testing.T) {
	freeze := cloudv1alpha1.ReleaseHold{Spec: cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "provider upgrade",
		Except: []string{"id-dev"}}}
	prod, dev := cluster("prod", cloudv1alpha1.ClusterTier0, "1.35"), cluster("dev", cloudv1alpha1.ClusterTier2, "1.35")
	in := input(prod, dev)
	in.Holds = []cloudv1alpha1.ReleaseHold{freeze}

	res := Compute(in)

	p := rowOf(t, res, "prod")
	if p.Gate == nil || p.Gate.Code != upgrade.GateFrozen || !strings.Contains(p.Gate.Message, "the platform is frozen: provider upgrade") {
		t.Errorf("prod: gate %+v, want FROZEN with the reason", p.Gate)
	}
	if d := rowOf(t, res, "dev"); d.Gate != nil && d.Gate.Code == upgrade.GateFrozen {
		t.Errorf("dev is let through, gate %+v", d.Gate)
	}
	if Freeze(in.Holds, now) == nil {
		t.Error("Freeze: the platform is frozen")
	}
	if got := HoldTarget(freeze.Spec); got != "the platform" {
		t.Errorf("HoldTarget = %q", got)
	}
}

// Each freeze target has its own hold name, and says what it froze, why
// and until when.
func TestFreezeNameAndMessage(t *testing.T) {
	until := metav1.NewTime(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	tests := []struct {
		spec          cloudv1alpha1.ReleaseHoldSpec
		name, message string
	}{
		{cloudv1alpha1.ReleaseHoldSpec{Platform: true, Reason: "provider upgrade"}, "platform-freeze", "the platform is frozen: provider upgrade"},
		{cloudv1alpha1.ReleaseHoldSpec{TenantID: "acme"}, "freeze-tenant-acme", "tenant acme is frozen"},
		{cloudv1alpha1.ReleaseHoldSpec{Channel: "beta", Reason: "quarter end", Until: &until}, "freeze-channel-beta",
			"channel beta is frozen: quarter end (until 2026-12-31T00:00:00Z)"},
		{cloudv1alpha1.ReleaseHoldSpec{Channel: "beta", Tier: cloudv1alpha1.ClusterTier0}, "freeze-channel-beta-tier0", "channel beta tier 0 is frozen"},
	}
	for _, tt := range tests {
		if got := FreezeNameOf(tt.spec); got != tt.name {
			t.Errorf("FreezeNameOf(%+v) = %q, want %q", tt.spec, got, tt.name)
		}
		if got := FreezeMessage(&cloudv1alpha1.ReleaseHold{Spec: tt.spec}); got != tt.message {
			t.Errorf("FreezeMessage(%+v) = %q, want %q", tt.spec, got, tt.message)
		}
	}
}
