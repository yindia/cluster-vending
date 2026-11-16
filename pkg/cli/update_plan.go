package cli

import (
	"fmt"
	"strings"

	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Print the resource impact before recording an update. The same plan ID
// must pass the server's recheck; no destructive change is silently replanned.
func reviewUpdatePlan(o Options, plan *v1.Plan) error {
	if plan == nil || plan.GetPlanId() == "" {
		return fmt.Errorf("the server returned no verifiable resource plan; the update was not applied")
	}
	var text strings.Builder
	if err := printPlan(&text, plan); err != nil {
		return err
	}
	o.progressf("%s", text.String())
	if !plan.GetApplicable() {
		return fmt.Errorf("the plan is not applicable; the update was not applied")
	}
	if o.PlanID != "" && o.PlanID != plan.GetPlanId() {
		return fmt.Errorf("the reviewed plan is stale; the update was not applied; run --plan again")
	}
	return nil
}
