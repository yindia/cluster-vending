package rollout

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
	"github.com/yindia/cluster-vending/pkg/release"
	"github.com/yindia/cluster-vending/pkg/upgrade"
)

// A rollout's target is its channel's minor, never one on no channel
// (extended support): an automatic cluster left on 1.35 (no channel in
// input's release) moves on to stable's 1.36, and one on 1.36 stays.
func TestRolloutNeverTargetsAnExtendedMinor(t *testing.T) {
	left := cluster("left", cloudv1alpha1.ClusterTier2, "1.35")
	left.Status.Deprecations.TargetVersion = "1.36"
	in := input(left, cluster("current", cloudv1alpha1.ClusterTier2, "1.36"))
	content := reconcile.ReleaseOf(in.Release.Name, in.Release.Spec.Content)

	res := Compute(in)

	if release.Ending(content, res.Rollout.TargetMinor) || res.Rollout.TargetMinor != "1.36" {
		t.Errorf("target minor %q, want stable's 1.36", res.Rollout.TargetMinor)
	}
	for _, r := range res.Rows {
		if release.Ending(content, r.Target) {
			t.Errorf("%s targets %s, a minor on no channel", r.Name, r.Target)
		}
	}
	if r := rowOf(t, res, "left"); r.State != upgrade.Pending || r.Target != "1.36" {
		t.Errorf("automatic cluster left on 1.35: %+v, want pending to 1.36", r)
	}
	if r := rowOf(t, res, "current"); r.State != upgrade.Succeeded || r.Target != "1.36" {
		t.Errorf("cluster on 1.36: %+v, want it to stay", r)
	}
}

// An edit the controller refused (EditRefused: automatic on a minor on no
// channel) is not taken for a request to upgrade automatically.
func TestRolloutTakesARefusedEditAsManual(t *testing.T) {
	c := cluster("refused", cloudv1alpha1.ClusterTier2, "1.35")
	c.Status.Deprecations.TargetVersion = "1.36"
	c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{Type: conditionEditRefused, Status: metav1.ConditionTrue,
		Reason: "Invalid", LastTransitionTime: metav1.Now()})

	r := rowOf(t, Compute(input(c)), "refused")

	if r.State != upgrade.Blocked || r.Gate == nil || r.Gate.Code != upgrade.GateManual {
		t.Errorf("refused edit: %+v, want blocked as manual", r)
	}
}
