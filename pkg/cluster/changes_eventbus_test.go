package cluster

import (
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Rules and targets an earlier build rendered have no eventBusName. Setting
// it to "default" (what unset means) is an update, never a rejected
// immutable change; moving to another bus still is.
func TestEventBusNameUnsetToDefaultIsNotRejected(t *testing.T) {
	for _, kind := range []string{"Rule", "Target"} {
		if got := ActionFor(kind, "eventBusName", true); got != cloudv1.PlannedAction_PLANNED_ACTION_UPDATE_IN_PLACE {
			t.Errorf("%s unset -> default = %v, want update in place", kind, got)
		}
		if got := ActionFor(kind, "eventBusName", false); got != cloudv1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE {
			t.Errorf("%s bus change = %v, want rejected", kind, got)
		}
	}
}
