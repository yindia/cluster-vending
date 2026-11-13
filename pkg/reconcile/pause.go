package reconcile

import (
	"fmt"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// ConditionPaused is the condition of a network or cluster some of whose
// managed resources are paused (AdminService.PauseReconcile): present and
// True while any is, absent otherwise.
const ConditionPaused = "Paused"

// PausedCount is how many of views are paused: they carry
// crossplane.io/paused, so their providers do not act on them.
func PausedCount(views []*cloudv1.ManagedResource) int {
	n := 0
	for _, v := range views {
		if v.GetPaused() {
			n++
		}
	}
	return n
}

// PausedCondition is the Paused condition for views, or nil when none of
// them is paused. A pause is the operator's (a provider upgrade): it is
// reported, and it is never a failure.
func PausedCondition(views []*cloudv1.ManagedResource) *cloudv1.Condition {
	n := PausedCount(views)
	if n == 0 {
		return nil
	}
	return &cloudv1.Condition{Type: ConditionPaused, Status: cloudv1.ConditionStatus_CONDITION_STATUS_TRUE,
		Reason:  managed.ReasonPaused,
		Message: fmt.Sprintf("%d of %d resources are paused: nothing changes in the cloud until they are resumed (admin resume)", n, len(views))}
}

// PausedNote is what a status message says about paused resources: ""
// when none is, else "; N resources paused" to append.
func PausedNote(views []*cloudv1.ManagedResource) string {
	if n := PausedCount(views); n > 0 {
		return fmt.Sprintf("; %d resources paused", n)
	}
	return ""
}
