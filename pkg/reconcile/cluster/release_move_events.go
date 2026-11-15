package cluster

import (
	"fmt"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// moveEvents are a release move's transitions: it started, a component
// was upgraded (the move went past it), it halted, it ended.
func moveEvents(before, after *cloudv1.ClusterStatus) []reconcile.Note {
	was, is := before.GetReleaseMove(), after.GetReleaseMove()
	var out []reconcile.Note
	if moving(is) && !moving(was) {
		out = append(out, reconcile.Normal(reconcile.ReasonReleaseMoveStarted,
			fmt.Sprintf("moving the components from release %s to %s, one at a time (%d to upgrade)", is.GetCurrent(), is.GetTarget(), is.GetSteps())))
	}
	// The component the last pass moved is behind it now: upgraded.
	if moving(was) && was.GetPhase() != moveHalted && was.GetComponent() != "" &&
		(is.GetComponent() != was.GetComponent() || (!moving(is) && is.GetCurrent() == was.GetTarget())) {
		out = append(out, reconcile.Normal(reconcile.ReasonComponentUpgraded,
			fmt.Sprintf("%s runs its version of release %s (%d of %d)", was.GetComponent(), was.GetTarget(), was.GetStep(), was.GetSteps())))
	}
	if is.GetComponent() != "" && is.GetComponent() == was.GetComponent() && is.GetRecoveries() > was.GetRecoveries() {
		out = append(out, reconcile.Warning(reconcile.ReasonReleaseMoveRecovering, is.GetMessage()))
	}
	if is.GetPhase() == moveHalted && was.GetPhase() != moveHalted {
		out = append(out, reconcile.Warning(reconcile.ReasonReleaseMoveHalted, is.GetMessage()))
	}
	if moving(was) && !moving(is) && is.GetCurrent() == was.GetTarget() {
		out = append(out, reconcile.Normal(reconcile.ReasonReleaseMoveFinished,
			fmt.Sprintf("the components run release %s", is.GetCurrent())))
	}
	return out
}
