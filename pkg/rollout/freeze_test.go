package rollout

import (
	"strings"
	"testing"

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
	if !Frozen([]cloudv1alpha1.ReleaseHold{freeze}, "id-prod", now) || Frozen([]cloudv1alpha1.ReleaseHold{freeze}, "id-dev", now) {
		t.Error("Frozen: prod frozen and dev let through")
	}
	if got := HoldTarget(freeze.Spec); got != "the platform" {
		t.Errorf("HoldTarget = %q", got)
	}
}
