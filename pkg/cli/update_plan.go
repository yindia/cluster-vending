package cli

import (
	"fmt"
	"slices"
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
		if blocking := blockingChanges(plan); blocking != "" {
			return fmt.Errorf("the plan is not applicable: these changes cannot be made (%s); the update was not applied", blocking)
		}
		return fmt.Errorf("the plan is not applicable; the update was not applied")
	}
	if o.PlanID != "" && o.PlanID != plan.GetPlanId() {
		return fmt.Errorf("the reviewed plan is stale; the update was not applied; run --plan again")
	}
	return nil
}

// blockingChanges names a plan's REJECTED_IMMUTABLE changes: their API
// path, else the resource; at most five, "..." for more.
func blockingChanges(plan *v1.Plan) string {
	var paths []string
	for _, c := range plan.GetChanges() {
		if c.GetAction() != v1.PlannedAction_PLANNED_ACTION_REJECTED_IMMUTABLE {
			continue
		}
		path := c.GetSpecPath()
		if path == "" {
			path = strings.TrimSpace(c.GetKind() + " " + c.GetName())
		}
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	if len(paths) > 5 {
		paths = append(paths[:5], "...")
	}
	return strings.Join(paths, ", ")
}
